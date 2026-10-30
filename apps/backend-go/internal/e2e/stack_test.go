//go:build integration

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

const (
	serviceToken = "e2e-internal-token-e2e-internal-token"
	setupToken   = "e2e-setup-token-0123456789abcdef"
)

// stack is the running no-python system: real api and worker binaries (package main cannot be imported), a private
// Postgres, the shared test Redis (own db), and the three fakes.
type stack struct {
	t       *testing.T
	api     string // base URL
	owner   *pgxpool.Pool
	py      *pyGuard
	browser *fakeBrowser
	llm     *fakeLLM
	logs    string
}

var built string

func buildBinaries(t *testing.T) string {
	t.Helper()
	if built != "" {
		return built
	}
	dir, err := os.MkdirTemp(os.Getenv("GOTMPDIR"), "e2e-bin-")
	if err != nil {
		t.Fatal(err)
	}
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..")
	for _, name := range []string{"api", "worker"} {
		cmd := exec.Command("go", "build", "-o", filepath.Join(dir, name), "./cmd/"+name)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go build %s: %v\n%s", name, err, out)
		}
	}
	built = dir
	return dir
}

func freePort(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// startStack boots everything with GO_ALL_NATIVE=true. extra overrides or adds env (e.g. GO_X=false to opt a flag out).
func startStack(t *testing.T, extra map[string]string) *stack {
	t.Helper()
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	bin := buildBinaries(t)
	appPool := testpg.NewAppPool(t, 2) // provisions the RLS login role and holds the cross-package lock
	appDSN := appPool.Config().ConnConfig.ConnString()
	owner, err := pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	for _, q := range []string{ // fresh first-run state; the ledger marker is the plan 14 cutover the harness stands in for
		"TRUNCATE jobs, users, workspaces CASCADE",
		"DELETE FROM app_settings WHERE key = 'setup_complete'",
		"INSERT INTO app_settings (key, value) VALUES ('__collection_ledger_backend', 'postgres') ON CONFLICT (key) DO UPDATE SET value = 'postgres'",
	} {
		if _, err := owner.Exec(context.Background(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	s := &stack{t: t, owner: owner, py: newPyGuard(), llm: newFakeLLM(t), logs: t.TempDir()}
	t.Cleanup(s.py.srv.Close)
	s.browser = newFakeBrowser(t, "e2e-browser-token")
	addr := freePort(t)
	s.api = "http://" + addr
	redis := os.Getenv("REDIS_E2E_URL")
	if redis == "" {
		redis = "redis://127.0.0.1:56380/2"
	}
	env := append(os.Environ(),
		"GO_ALL_NATIVE=true", "APP_ENV=test", "DATABASE_URL="+appDSN, "REDIS_URL="+redis,
		"API_ADDR="+addr, "SERVE_UI=false", "SECRET_KEY=e2e-secret-key-e2e-secret-key-e2e-secret", "SECRETS_PROVIDER=local",
		"SECRETS_MASTER_KEY=e2e-master-key-e2e-master-key-e2e-master", "CREDENTIALS_SOURCE=local",
		"PG_LEAD_STORE=true", "COPILOT_GO_ENABLED=true", "BILLING_ENABLED=false", "EGRESS_ALLOW_HOSTS=127.0.0.1",
		"INTERNAL_API_URL="+s.py.srv.URL, // PY_WORKER_URL is not set at all: there is no Python worker (plan 15)
		"INTERNAL_SERVICE_TOKEN="+serviceToken, "DATA_DIR="+t.TempDir(),
		"BROWSER_SERVICE_URL="+s.browser.srv.URL, "BROWSER_SERVICE_TOKEN="+s.browser.token,
		"AUTOMATIONS_ENABLED=true",
		"SETUP_TOKEN="+setupToken, "GO_WORKER_CONCURRENCY=4", "PG_RLS_REQUIRE_SAFE_ROLE=true",
		"GO_WORKER_DEBUG_ADDR=127.0.0.1:0", // a fixed :6060 collides with any other worker on the host
	)
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	for _, name := range []string{"api", "worker"} {
		s.start(name, filepath.Join(bin, name), env)
	}
	s.waitHTTP(s.api + "/healthz")
	return s
}

func (s *stack) start(name, path string, env []string) {
	logf, err := os.Create(filepath.Join(s.logs, name+".log"))
	if err != nil {
		s.t.Fatal(err)
	}
	cmd := exec.Command(path)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = childProcessAttrs()
	if err := cmd.Start(); err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		logf.Close()
		if s.t.Failed() {
			b, _ := os.ReadFile(logf.Name())
			if len(b) > 4000 {
				b = b[len(b)-4000:]
			}
			s.t.Logf("%s log tail:\n%s", name, b)
		}
	})
}

func (s *stack) waitHTTP(url string) {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(url); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, n := range []string{"api", "worker"} {
		b, _ := os.ReadFile(filepath.Join(s.logs, n+".log"))
		s.t.Logf("%s log:\n%s", n, b)
	}
	s.t.Fatalf("%s did not come up", url)
}

// session is an authenticated API caller.
type session struct {
	s     *stack
	token string
	ws    string
}

func (s *stack) do(ses *session, method, path string, body any) (int, json.RawMessage) {
	s.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, s.api+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if ses != nil {
		req.Header.Set("Authorization", "Bearer "+ses.token)
		if ses.ws != "" {
			req.Header.Set("X-Workspace-Id", ses.ws)
		}
	}
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (s *stack) must(ses *session, method, path string, body any, want int) map[string]any {
	s.t.Helper()
	code, raw := s.do(ses, method, path, body)
	if code != want && !(want == 200 && code/100 == 2) {
		s.t.Fatalf("%s %s = %d, want %d: %.400s", method, path, code, want, raw)
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return m
}

func (s *stack) waitJob(id string, within time.Duration) string {
	s.t.Helper()
	deadline := time.Now().Add(within)
	var st string
	for time.Now().Before(deadline) {
		_ = s.owner.QueryRow(context.Background(), `SELECT status FROM jobs WHERE id=$1`, id).Scan(&st)
		switch st {
		case "completed", "failed", "cancelled", "succeeded", "done":
			return st
		}
		time.Sleep(150 * time.Millisecond)
	}
	return "timeout(" + st + ")"
}

func str(m map[string]any, k string) string { v, _ := m[k].(string); return v }

func pyList(calls []string) string { return fmt.Sprintf("[%s]", strings.Join(calls, "; ")) }

// login posts the OAuth2 password form to /auth/token and returns the session.
func (s *stack) login(user, pw string) *session {
	s.t.Helper()
	resp, err := http.PostForm(s.api+"/auth/token", map[string][]string{"username": {user}, "password": {pw}})
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var tok map[string]any
	_ = json.Unmarshal(b, &tok)
	if resp.StatusCode != 200 || str(tok, "access_token") == "" {
		s.t.Fatalf("login = %d %.200s", resp.StatusCode, b)
	}
	return &session{s: s, token: str(tok, "access_token")}
}

// jobsSince returns "type/executor/status" for jobs with id > after, optionally only the given types.
func (s *stack) jobsSince(after int, types ...string) []string {
	s.t.Helper()
	rows, err := s.owner.Query(context.Background(), `SELECT id, type, executor, status, coalesce(error,'') FROM jobs WHERE id > $1 ORDER BY id`, after)
	if err != nil {
		s.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id int
		var typ, ex, st, er string
		_ = rows.Scan(&id, &typ, &ex, &st, &er)
		if len(types) > 0 {
			ok := false
			for _, t := range types {
				ok = ok || t == typ
			}
			if !ok {
				continue
			}
		}
		if er != "" {
			st += "(" + er[:min(len(er), 120)] + ")"
		}
		out = append(out, fmt.Sprintf("%s/%s/%s", typ, ex, st))
	}
	return out
}

func (s *stack) maxJob() int {
	var n int
	_ = s.owner.QueryRow(context.Background(), `SELECT coalesce(max(id),0) FROM jobs`).Scan(&n)
	return n
}

// drain waits until no job after `after` is pending or processing (or the time runs out) and returns them.
func (s *stack) drain(after int, within time.Duration) []string {
	s.t.Helper()
	deadline := time.Now().Add(within)
	for {
		var n int
		_ = s.owner.QueryRow(context.Background(), `SELECT count(*) FROM jobs WHERE id > $1 AND status IN ('pending','processing')`, after).Scan(&n)
		if n == 0 || time.Now().After(deadline) {
			return s.jobsSince(after)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (s *stack) workspaceID() string {
	var id string
	_ = s.owner.QueryRow(context.Background(), `SELECT id FROM workspaces ORDER BY created_at LIMIT 1`).Scan(&id)
	return id
}
