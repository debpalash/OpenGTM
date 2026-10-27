//go:build integration

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

const fixtures = "../../testdata/mcp"

var (
	binOnce sync.Once
	binPath string
	binErr  error
	binDir  string
)

func TestMain(m *testing.M) {
	code := m.Run()
	if binDir != "" {
		os.RemoveAll(binDir)
	}
	os.Exit(code)
}

// buildBinary compiles cmd/mcp once into a temp dir as opengtm-mcp.
func buildBinary(t *testing.T) string {
	t.Helper()
	binOnce.Do(func() {
		binDir, binErr = os.MkdirTemp("", "opengtm-mcp-bin")
		if binErr != nil {
			return
		}
		binPath = filepath.Join(binDir, "opengtm-mcp")
		out, err := exec.Command("go", "build", "-o", binPath, ".").CombinedOutput()
		if err != nil {
			binErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if binErr != nil {
		t.Fatal(binErr)
	}
	return binPath
}

func appURL(t *testing.T) string {
	t.Helper()
	u, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(testpg.AppLoginRole, "rls_test_only")
	return u.String()
}

type seeded struct {
	pool   *pgxpool.Pool
	ws     string
	leads  []int64 // real ids of the six alpha seed leads
	tokens map[string]string
	ids    map[string]string // token alias -> mcp_tokens id
}

type seedLead struct {
	company, city, website, email, phone, person, spec, sz, status string
	score                                                          int
	tier                                                           string
	dm                                                             *string
}

var seedDM = `[{"name": "Sam Lee", "title": "CEO"}]`

var alphaLeads = []seedLead{
	{"Acme Robotics", "Berlin", "https://acme.example.com", "info@acme.example.com", "+49 30 555 0101", "Ada Lovelace", "Robotics", "51-200", "new", 80, "hot", &seedDM},
	{"Globex Corp", "Austin", "https://globex.example.com", "hello@globex.example.com", "+1 512 555 0102", "Hank Scorpio", "Energy", "201-500", "contacted", 65, "warm", nil},
	{"Éco Solutions", "Pune", "https://eco.example.com", "", "", "Isha Rao", "Sustainability", "11-50", "new", 40, "cold", nil},
	{"Suppressed Inc", "Oslo", "https://sup.example.com", "stop@sup.example.com", "", "", "Retail", "1-10", "new", 30, "cold", nil},
	{"Delete Me Ltd", "Lyon", "", "bye@delete.example.com", "", "", "", "", "new", 10, "unqualified", nil},
	{"東京テック", "東京", "https://tokyo.example.com", "hi@tokyo.example.com", "", "", "Software", "11-50", "qualified", 55, "warm", nil},
}

func seed(t *testing.T) *seeded {
	t.Helper()
	pool := testpg.NewAppPool(t, 4)
	ctx := context.Background()
	sfx := strconv.FormatInt(time.Now().UnixNano(), 10)
	s := &seeded{pool: pool, ws: "t17-" + sfx, tokens: map[string]string{}, ids: map[string]string{}}
	var uid int32
	if err := pool.QueryRow(ctx, `INSERT INTO users (username, hashed_password, is_active, is_admin, role) VALUES ($1,'x',true,false,'user') RETURNING id`, "t17-"+sfx).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, slug, owner_id) VALUES ($1::varchar,$1::text,$1::text,$2::int)`, s.ws, uid); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1,$2,'owner')`, s.ws, uid); err != nil {
		t.Fatal(err)
	}
	tok := func(alias, caps string, expires *time.Time) {
		raw := "ycp_t17_" + alias + "_" + sfx
		h := sha256.Sum256([]byte(raw))
		id := "t17-" + alias + "-" + sfx
		if _, err := pool.Exec(ctx, `INSERT INTO mcp_tokens (id, token_hash, prefix, name, user_id, workspace_id, capabilities, expires_at)
			VALUES ($1,$2,'ycp_x','',$3,$4,$5::json,$6)`, id, hex.EncodeToString(h[:]), uid, s.ws, caps, expires); err != nil {
			t.Fatal(err)
		}
		s.tokens[alias], s.ids[alias] = raw, id
	}
	past := time.Now().Add(-time.Hour)
	tok("read", `["leads:read"]`, nil)
	tok("expired", `["leads:read"]`, &past)
	for _, l := range alphaLeads {
		var id int64
		err := tenancy.InWorkspace(ctx, pool, s.ws, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `INSERT INTO leads (workspace_id, company, city, website, email, phone, contact_person, specialization,
				company_size, status, score, score_tier, decision_makers, source, created_at, updated_at)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,'mcp-seed','2026-08-29T10:00:00','2026-08-29T10:00:00') RETURNING id`,
				s.ws, l.company, l.city, l.website, l.email, l.phone, l.person, l.spec, l.sz, l.status, l.score, l.tier, l.dm).Scan(&id)
		})
		if err != nil {
			t.Fatal(err)
		}
		s.leads = append(s.leads, id)
	}
	t.Cleanup(func() {
		_ = tenancy.InWorkspace(ctx, pool, s.ws, func(tx pgx.Tx) error {
			for _, q := range []string{`DELETE FROM leads`, `DELETE FROM mcp_audit_log`, `DELETE FROM trigger_cap_reservations`} {
				_, _ = tx.Exec(ctx, q)
			}
			return nil
		})
		_, _ = pool.Exec(ctx, `DELETE FROM mcp_tokens WHERE workspace_id=$1`, s.ws)
		_, _ = pool.Exec(ctx, `DELETE FROM workspace_members WHERE workspace_id=$1`, s.ws)
		_, _ = pool.Exec(ctx, `DELETE FROM workspaces WHERE id=$1`, s.ws)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, uid)
	})
	return s
}

func baseEnv(extra ...string) []string {
	return append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "APP_ENV=test"}, extra...)
}

func frame(body string) string {
	return "Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
}

func readFrame(t *testing.T, r *bufio.Reader) (string, error) {
	t.Helper()
	n := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return "", err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if v, ok := strings.CutPrefix(line, "Content-Length: "); ok {
			n, _ = strconv.Atoi(v)
		}
	}
	if n < 0 {
		t.Fatal("frame without Content-Length")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", err
	}
	return string(b), nil
}

func parseFrames(t *testing.T, s string) []string {
	t.Helper()
	var out []string
	r := bufio.NewReader(strings.NewReader(s))
	for {
		b, err := readFrame(t, r)
		if err != nil {
			return out
		}
		out = append(out, b)
	}
}

func fixtureJSON(t *testing.T, name string) (token *string, stdin, stdout string, exit int) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtures, "stdio", name))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Token   *string `json:"token"`
		Request struct {
			Stdin string `json:"stdin"`
		} `json:"request"`
		Response struct {
			Stdout   string `json:"stdout"`
			ExitCode int    `json:"exit_code"`
		} `json:"response"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f.Token, f.Request.Stdin, f.Response.Stdout, f.Response.ExitCode
}

func runBinary(t *testing.T, env []string, stdin string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(buildBinary(t))
	cmd.Env = env
	cmd.Stdin = strings.NewReader(stdin)
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		code = 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("binary did not exit; stderr=%s", e.String())
	}
	return o.String(), e.String(), code
}

func TestBinaryExitsOneWithoutToken(t *testing.T) {
	_, stdin, wantOut, wantExit := fixtureJSON(t, "read__no_token_exits.json")
	out, errs, code := runBinary(t, baseEnv("DATABASE_URL=postgres://nobody@127.0.0.1:1/none"), stdin)
	if code != wantExit || out != wantOut {
		t.Fatalf("code=%d out=%q stderr=%q", code, out, errs)
	}
	if !strings.Contains(errs, "OPENGTM_MCP_TOKEN") {
		t.Fatalf("stderr should say how to fix it: %q", errs)
	}
}

func TestBinaryRefusesSuperuserRole(t *testing.T) {
	s := seed(t)
	out, errs, code := runBinary(t, baseEnv("DATABASE_URL="+os.Getenv("TEST_DATABASE_URL"), "OPENGTM_MCP_TOKEN="+s.tokens["read"]),
		frame(`{"jsonrpc": "2.0", "id": 1, "method": "ping"}`))
	if code != 1 || out != "" || !strings.Contains(errs, "superuser") {
		t.Fatalf("code=%d out=%q stderr=%q", code, out, errs)
	}
	if strings.Contains(errs, s.tokens["read"]) || strings.Contains(errs, "postgres:postgres") {
		t.Fatal("secret in stderr")
	}
}

func TestStdioEndToEndBinary(t *testing.T) {
	s := seed(t)
	env := func(alias string) []string {
		e := baseEnv("DATABASE_URL=" + appURL(t))
		if alias != "" {
			e = append(e, "OPENGTM_MCP_TOKEN="+s.tokens[alias])
		}
		return e
	}
	idRe := regexp.MustCompile(`"id": (\d+)`)
	norm := func(text string) string {
		text = idRe.ReplaceAllStringFunc(text, func(m string) string { // one pass: real ids must not be rewritten again
			n, _ := strconv.Atoi(idRe.FindStringSubmatch(m)[1])
			if n >= 1 && n <= len(s.leads) {
				return fmt.Sprintf(`"id": %d`, s.leads[n-1])
			}
			return m
		})
		return strings.ReplaceAll(text, `"workspace_id": "ws-alpha"`, `"workspace_id": "`+s.ws+`"`)
	}
	text := func(body string) (id string, txt string) {
		var r struct {
			ID     json.RawMessage `json:"id"`
			Result struct {
				Content []struct{ Text string } `json:"content"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(body), &r); err != nil || len(r.Result.Content) != 1 {
			t.Fatalf("bad answer %q: %v", body, err)
		}
		return string(r.ID), r.Result.Content[0].Text
	}

	// Byte-exact: no ids involved.
	for _, name := range []string{"read__initialize_ping_list.json", "read__capability_denied.json"} {
		_, stdin, want, _ := fixtureJSON(t, name)
		if name == "read__capability_denied.json" {
			// F7: Python recorded isError false for a tool error; Go sets true (MCP spec), so the frame is one byte shorter.
			want = strings.Replace(strings.Replace(want, `"isError": false`, `"isError": true`, 1), "Content-Length: 171\r\n", "Content-Length: 170\r\n", 1)
		}
		out, errs, code := runBinary(t, env("read"), stdin)
		if code != 0 || out != want {
			t.Errorf("%s: code=%d stderr=%q\n got %q\nwant %q", name, code, errs, out, want)
		}
	}
	// Seeded ids differ per run: compare the tool text after mapping.
	for _, name := range []string{"read__find_leads.json", "read__unicode_result.json"} {
		_, stdin, want, _ := fixtureJSON(t, name)
		stdin = frame(strings.Replace(parseFrames(t, stdin)[0], `"lead_id": 6`, `"lead_id": `+strconv.FormatInt(s.leads[5], 10), 1))
		out, errs, code := runBinary(t, env("read"), stdin)
		gf, wf := parseFrames(t, out), parseFrames(t, want)
		if code != 0 || len(gf) != 1 || len(wf) != 1 {
			t.Fatalf("%s: code=%d frames=%d/%d stderr=%q", name, code, len(gf), len(wf), errs)
		}
		gid, gt := text(gf[0])
		wid, wt := text(wf[0])
		if gid != wid || gt != norm(wt) {
			t.Errorf("%s:\n got %s\nwant %s", name, gt, norm(wt))
		}
		if !strings.HasPrefix(out, "Content-Length: "+strconv.Itoa(len(gf[0]))+"\r\n\r\n") {
			t.Errorf("%s: header does not match body length", name)
		}
	}
	// Invalid (expired) token: exit 1, stdout empty, no secret on stderr.
	_, stdin, wantOut, wantExit := fixtureJSON(t, "read__invalid_token_exits.json")
	out, errs, code := runBinary(t, env("expired"), stdin)
	if code != wantExit || out != wantOut || strings.Contains(errs, s.tokens["expired"]) {
		t.Fatalf("expired: code=%d out=%q stderr=%q", code, out, errs)
	}
}

// A token revoked while a stdio session is open ends the session with exit 1
// within MCP_STDIO_REVALIDATE_SECONDS (D8).
func TestStdioRevokedSessionExitsOne(t *testing.T) {
	s := seed(t)
	cmd := exec.Command(buildBinary(t))
	cmd.Env = baseEnv("DATABASE_URL="+appURL(t), "OPENGTM_MCP_TOKEN="+s.tokens["read"], "MCP_STDIO_REVALIDATE_SECONDS=1")
	in, _ := cmd.StdinPipe()
	outp, _ := cmd.StdoutPipe()
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()
	r := bufio.NewReader(outp)
	io.WriteString(in, frame(`{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}`))
	if b, err := readFrame(t, r); err != nil || !strings.Contains(b, `"find_leads"`) {
		t.Fatalf("first list: %q %v (stderr %s)", b, err, errb.String())
	}
	if _, err := s.pool.Exec(context.Background(), `UPDATE mcp_tokens SET revoked_at = now() WHERE id=$1`, s.ids["read"]); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1300 * time.Millisecond)
	io.WriteString(in, frame(`{"jsonrpc": "2.0", "id": 2, "method": "tools/list"}`))
	if b, err := readFrame(t, r); err != nil || !strings.Contains(b, "unauthorized") || strings.Contains(b, s.tokens["read"]) {
		t.Fatalf("after revoke: %q %v", b, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
			t.Fatalf("exit: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("session did not end")
	}
}

// SIGTERM during a slow tool call lets that call answer, then the process stops
// reading and exits 0.
func TestGracefulShutdownFinishesInFlightCall(t *testing.T) {
	s := seed(t)
	started := make(chan struct{}, 1)
	py := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		time.Sleep(1500 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"ok","output":{"success":true,"fields":{"company":"Slow Co","open_roles":2}},"error":null,"cost_usd":0}`)
	}))
	defer py.Close()
	cmd := exec.Command(buildBinary(t))
	cmd.Env = baseEnv("DATABASE_URL="+appURL(t), "OPENGTM_MCP_TOKEN="+s.tokens["read"], "PY_WORKER_URL="+py.URL, "PY_WORKER_TOKEN=t17-secret")
	in, _ := cmd.StdinPipe()
	outp, _ := cmd.StdoutPipe()
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()
	io.WriteString(in, frame(`{"jsonrpc": "2.0", "id": 7, "method": "tools/call", "params": {"name": "get_hiring_signals", "arguments": {"company": "Slow Co"}}}`))
	select {
	case <-started:
	case <-time.After(15 * time.Second):
		t.Fatalf("tool never reached the worker; stderr=%s", errb.String())
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	b, err := readFrame(t, bufio.NewReader(outp))
	if err != nil || !strings.Contains(b, `"id": 7`) || !strings.Contains(b, "Slow Co") || strings.Contains(b, `\"error\"`) {
		t.Fatalf("in-flight call was not finished: %q %v (stderr %s)", b, err, errb.String())
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("exit: %v (stderr %s)", err, errb.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("process did not exit after SIGTERM")
	}
	if strings.Contains(errb.String(), "t17-secret") || strings.Contains(errb.String(), s.tokens["read"]) {
		t.Fatal("secret on stderr")
	}
}
