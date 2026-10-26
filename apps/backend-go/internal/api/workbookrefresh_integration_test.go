//go:build integration

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type refreshEnv struct{ estimateEnv }

// newRefreshEnv is the estimate environment (alice owner, bob viewer-level member of ws-alpha and
// owner of ws-beta, erin editor, the seeded enrichment workbooks) with the refresh routes only.
func newRefreshEnv(t *testing.T, getenv func(string) string) refreshEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	d := Deps{Pool: pool, Signer: testSigner, Identity: st, Config: config.APIConfig{},
		Flags: config.Flags{PGLeadStore: true}, Getenv: getenv}
	registerWorkbookRefresh(r, d)
	e := refreshEnv{estimateEnv{detailEnv{mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool}}}}
	e.purge()
	e.cleanRefresh()
	t.Cleanup(func() { e.cleanRefresh(); e.purge() })
	e.seedDetail(t)
	e.seed(t, "erin", pythonHash, true, "user", false)
	e.member(t, "ws-alpha", e.mustUserByName(t, "erin").ID, "editor")
	e.seedEnrich(t)
	e.workbook(t, "wb-enrich-v1", `[]`, 0)
	return e
}

func (e refreshEnv) clearJobs() {
	_, _ = e.pool.Exec(context.Background(), `DELETE FROM jobs WHERE type = 'refresh_workbook'`)
}

func (e refreshEnv) cleanRefresh() {
	e.clearJobs()
	e.cleanEnrich()
}

func refreshActor(name string) (who, ws string) {
	switch {
	case strings.HasSuffix(name, "no_token"):
		return "", ""
	case strings.HasSuffix(name, "viewer"):
		return "bob", "ws-alpha"
	case strings.HasSuffix(name, "foreign") || strings.HasSuffix(name, "foreign_404"):
		return "bob", "ws-beta"
	case strings.HasSuffix(name, "editor"):
		return "erin", "ws-alpha"
	}
	return "alice", "ws-alpha"
}

func (e refreshEnv) call(who, ws, method, path, body string) contract.Resp {
	hdr := map[string]string{}
	if who != "" {
		hdr = bearer(who)
		hdr["X-Workspace-Id"] = ws
	}
	ctype := ""
	if body != "" {
		ctype = "application/json"
	}
	_, resp := e.do(method, path, ctype, body, hdr)
	return resp
}

func (e refreshEnv) replay(t *testing.T, name string) {
	t.Helper()
	fx := contract.Load(t, "refresh/"+name)
	who, ws := refreshActor(name)
	body := ""
	if fx.Request.Body != nil && string(fx.Request.Body) != "null" {
		body = string(fx.Request.Body)
	}
	got := e.call(who, ws, fx.Request.Method, fx.Request.Path, body)
	t.Run(name, func(t *testing.T) { contract.AssertEqual(t, got, fx.Response) })
}

func TestRefreshPolicyMatchesFixtures(t *testing.T) {
	e := newRefreshEnv(t, nil)
	n := 0
	for _, name := range contractFixtureNames(t, "refresh") {
		e.replay(t, strings.TrimPrefix(name, "refresh/"))
		n++
	}
	if n < 20 {
		t.Fatalf("expected the recorded refresh fixtures, got %d", n)
	}
}

func TestRefreshPolicyDefaults(t *testing.T) {
	e := newRefreshEnv(t, nil)
	r := e.call("alice", "ws-alpha", "PUT", "/api/workbooks/wb-enrich-1/refresh-policy", `{}`)
	want := `{"refresh_policy":{"enabled":true,"interval":null,"on_signal":[],"staleness_ttl_days":{}},"next_in_minutes":null}`
	if r.Status != 200 || strings.TrimSpace(string(r.Body)) != want {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	var stored string
	if err := tenancy.InWorkspace(context.Background(), e.pool, "ws-alpha", func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT refresh_policy::text FROM workbooks WHERE id='wb-enrich-1'`).Scan(&stored)
	}); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(stored), &got); err != nil || got["enabled"] != true || len(got) != 4 {
		t.Fatalf("stored policy %s (%v)", stored, err)
	}
}

func TestRefreshPolicyUnknownWorkbook404(t *testing.T) {
	e := newRefreshEnv(t, nil)
	r := e.call("alice", "ws-alpha", "PUT", "/api/workbooks/nope/refresh-policy", `{"enabled":true}`)
	if r.Status != 404 || strings.TrimSpace(string(r.Body)) != `{"detail":"Workbook not found"}` {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
}

// cohortEnv is a mutable getenv: the env holds one pool (one advisory lock), the cohort varies per case.
type cohortEnv struct{ workspaces string }

func (c *cohortEnv) get(k string) string {
	switch k {
	case "GO_EXECUTOR_ENABLED":
		if c.workspaces != "" {
			return "true"
		}
	case "GO_EXECUTOR_WORKSPACES":
		return c.workspaces
	}
	return ""
}

// The chain job the policy starts uses the cohort executor, like refresh_executor in Python.
func TestRefreshPolicyChainUsesCohortExecutor(t *testing.T) {
	co := &cohortEnv{}
	e := newRefreshEnv(t, co.get)
	for _, c := range []struct{ cohort, want string }{{"", "python"}, {"ws-alpha", "go"}} {
		co.workspaces = c.cohort
		e.clearJobs()
		r := e.call("alice", "ws-alpha", "PUT", "/api/workbooks/wb-enrich-1/refresh-policy", `{"interval":"daily"}`)
		if r.Status != 200 {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
		var ex string
		if err := e.pool.QueryRow(context.Background(), `SELECT executor FROM jobs WHERE type='refresh_workbook'`).Scan(&ex); err != nil || ex != c.want {
			t.Fatalf("cohort %q: executor %q (%v), want %s", c.cohort, ex, err, c.want)
		}
	}
}

func TestRefreshEnqueuesWithWorkspaceID(t *testing.T) {
	co := &cohortEnv{}
	e := newRefreshEnv(t, co.get)
	for _, c := range []struct{ cohort, want string }{{"", "python"}, {"ws-alpha", "go"}, {"ws-other", "python"}} {
		co.workspaces = c.cohort
		e.clearJobs()
		r := e.call("alice", "ws-alpha", "POST", "/api/workbooks/wb-enrich-1/refresh", "")
		if r.Status != 200 || strings.TrimSpace(string(r.Body)) != `{"status":"refreshing"}` {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
		var ex, payload, status string
		var ws, fire *string
		if err := e.pool.QueryRow(context.Background(), `SELECT executor, payload::text, status, workspace_id, fire_key FROM jobs WHERE type='refresh_workbook'`).
			Scan(&ex, &payload, &status, &ws, &fire); err != nil {
			t.Fatal(err)
		}
		if ex != c.want || status != "pending" || ws == nil || *ws != "ws-alpha" || fire != nil {
			t.Errorf("cohort %q: executor %q status %s ws %v fire %v", c.cohort, ex, status, ws, fire)
		}
		if payload != `{"workbook_id":"wb-enrich-1","reason":"manual","workspace_id":"ws-alpha"}` {
			t.Errorf("payload %s", payload)
		}
	}
}

func TestRefreshViewerForbidden(t *testing.T) {
	e := newRefreshEnv(t, nil)
	r := e.call("bob", "ws-alpha", "POST", "/api/workbooks/wb-enrich-1/refresh", "")
	if r.Status != 403 {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	var n int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM jobs WHERE type='refresh_workbook'`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d jobs enqueued for a forbidden caller", n)
	}
}

func TestRefreshForeignWorkbook404(t *testing.T) {
	e := newRefreshEnv(t, nil)
	r := e.call("bob", "ws-beta", "POST", "/api/workbooks/wb-enrich-1/refresh", "")
	if r.Status != 404 || strings.TrimSpace(string(r.Body)) != `{"detail":"Workbook not found"}` {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	var n int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM jobs WHERE type='refresh_workbook'`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d jobs enqueued for a foreign workbook", n)
	}
}

func TestRefreshPolicyBodyLimitAndReadError(t *testing.T) {
	e := newRefreshEnv(t, nil)
	big := `{"on_signal":["` + strings.Repeat("x", maxRefreshPolicyBody) + `"]}`
	if r := e.call("alice", "ws-alpha", "PUT", "/api/workbooks/wb-enrich-1/refresh-policy", big); r.Status != 413 {
		t.Fatalf("oversized: %d %s", r.Status, r.Body)
	}
	// A body that fails mid-read (client abort) is not "too large".
	req := httptest.NewRequest("PUT", "/api/workbooks/wb-enrich-1/refresh-policy", io.NopCloser(iotest.ErrReader(errors.New("aborted"))))
	for k, v := range bearer("alice") {
		req.Header.Set(k, v)
	}
	req.Header.Set("X-Workspace-Id", "ws-alpha")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("read error: %d %s", rec.Code, rec.Body.String())
	}
}
