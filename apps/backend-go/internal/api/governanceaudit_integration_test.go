//go:build integration

package api

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type gaEnv struct {
	mcpEnv
	r *httpx.Router
}

// newGAEnv seeds the governance fixture world: alice owner of ws-alpha, bob a plain member of ws-alpha and owner
// of ws-beta, erin an editor of ws-alpha. The audit events are rebuilt from the recorded unfiltered responses
// (gov_audit_default = ws-alpha, gov_audit_beta_workspace = ws-beta; the 60 and 4 seeded rows).
func newGAEnv(t *testing.T, mw ...httpx.Middleware) gaEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	r.Use(httpx.RequestID())
	r.Use(mw...)
	registerGovernanceAudit(r, Deps{Pool: pool, Signer: testSigner, Identity: st,
		Flags: config.Flags{PGLeadStore: true}})
	e := gaEnv{mcpEnv: mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool}, r: r}
	e.purge()
	t.Cleanup(e.purge)
	for _, u := range []struct {
		n     string
		admin bool
	}{{"alice", true}, {"bob", false}, {"erin", false}} {
		role := "user"
		if u.admin {
			role = "admin"
		}
		e.seed(t, u.n, pythonHash, true, role, u.admin)
	}
	alice, bob, erin := e.mustUserByName(t, "alice"), e.mustUserByName(t, "bob"), e.mustUserByName(t, "erin")
	e.workspace(t, "ws-alpha", alice.ID)
	e.workspace(t, "ws-beta", bob.ID)
	e.member(t, "ws-alpha", alice.ID, "owner")
	e.member(t, "ws-alpha", bob.ID, "member")
	e.member(t, "ws-alpha", erin.ID, "editor")
	e.member(t, "ws-beta", bob.ID, "owner")
	e.insertFixtureEvents(t, "ws-alpha", "gov_audit_default")
	e.restoreMetadataOrder(t, "ws-alpha", "gov_export_csv_default")
	e.insertFixtureEvents(t, "ws-beta", "gov_audit_beta_workspace")
	return e
}

func (e gaEnv) purge() {
	ctx := context.Background()
	for _, ws := range []string{"ws-alpha", "ws-beta"} {
		_ = tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
			_, _ = tx.Exec(ctx, `DELETE FROM governance_audit_events`)
			return nil
		})
	}
}

func (e gaEnv) insertFixtureEvents(t *testing.T, ws, fixture string) {
	t.Helper()
	fx := contract.Load(t, "governance/"+fixture)
	var body struct {
		Events json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(fx.Response.Body, &body); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	err := tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO governance_audit_events (id, workspace_id, actor_user_id, actor_role, method, route,
			resource_path, response_status, outcome, request_id, metadata_json, created_at)
			SELECT id, $1, actor_user_id, actor_role, method, route, resource_path, response_status, outcome, request_id,
			       metadata::json, created_at::timestamp
			FROM json_to_recordset($2::json) AS x(id text, actor_user_id int, actor_role text, method text, route text,
			       resource_path text, response_status int, outcome text, request_id text, metadata json, created_at text)`,
			ws, string(body.Events))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// restoreMetadataOrder rewrites metadata_json from the CSV fixture, which keeps Python's stored key order (the list
// fixtures are recorded with sorted keys, so seeding from them alone can never reproduce the CSV bytes).
func (e gaEnv) restoreMetadataOrder(t *testing.T, ws, fixture string) {
	t.Helper()
	fx := contract.Load(t, "governance/"+fixture)
	var text string
	if err := json.Unmarshal(fx.Response.Body, &text); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(text, "\ufeff"))).ReadAll()
	if err != nil || len(rows) < 2 {
		t.Fatalf("csv fixture: %v", err)
	}
	ctx := context.Background()
	err = tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
		for _, r := range rows[1:] {
			if _, err := tx.Exec(ctx, `UPDATE governance_audit_events SET metadata_json = $2::json WHERE id = $1 AND workspace_id = $3`, r[0], r[10], ws); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func gaActor(name string) (who, ws string) {
	switch name {
	case "gov_audit_beta_workspace", "gov_audit_cursor_other_workspace", "gov_export_csv_beta":
		return "bob", "ws-beta" // bob's default workspace in the recording
	case "gov_audit_bob_403", "gov_export_csv_bob_403":
		return "bob", "ws-alpha"
	case "gov_audit_erin_403":
		return "erin", "ws-alpha"
	case "gov_audit_nonmember_403":
		return "erin", "ws-beta"
	case "gov_audit_no_auth_401":
		return "", ""
	}
	return "alice", "ws-alpha"
}

func (e gaEnv) get(who, ws, path string) (*httptest.ResponseRecorder, contract.Resp) {
	hdr := map[string]string{}
	if who != "" {
		hdr = bearer(who)
		hdr["X-Workspace-Id"] = ws
	}
	return e.do("GET", path, "", "", hdr)
}

func TestAuditListMatchesFixtures(t *testing.T) {
	e := newGAEnv(t)
	n := 0
	for _, full := range contractFixtureNames(t, "governance") {
		name := strings.TrimPrefix(full, "governance/")
		// The tail fixtures read back events produced by the recorded write sequence; capabilities is retained on Python.
		if !strings.HasPrefix(name, "gov_audit_") && !strings.HasPrefix(name, "gov_export_csv_") ||
			strings.HasPrefix(name, "gov_audit_tail_") {
			continue
		}
		fx := contract.Load(t, full)
		who, ws := gaActor(name)
		rec, got := e.get(who, ws, fx.Request.Path)
		if strings.HasPrefix(rec.Header().Get("Content-Type"), "text/csv") {
			got.Body, _ = json.Marshal(rec.Body.String())
			if cd := rec.Header().Get("Content-Disposition"); cd != "attachment; filename=opengtm-audit-events.csv" {
				t.Errorf("%s: content-disposition %q", name, cd)
			}
		}
		n++
		t.Run(name, func(t *testing.T) { contract.AssertEqual(t, got, fx.Response) })
	}
	if n < 40 {
		t.Fatalf("only %d audit fixtures replayed", n)
	}
}

func TestAuditListLimitBounds422(t *testing.T) {
	e := newGAEnv(t)
	for _, c := range []struct {
		q    string
		code int
	}{{"limit=0", 422}, {"limit=-1", 422}, {"limit=501", 422}, {"limit=99999999999999999999", 422}, {"limit=abc", 422}, {"limit=1", 200}, {"limit=500", 200}} {
		if rec, _ := e.get("alice", "ws-alpha", "/api/governance/audit-events?"+c.q); rec.Code != c.code {
			t.Errorf("%s: %d", c.q, rec.Code)
		}
	}
}

func TestAuditListBadActorId422(t *testing.T) {
	e := newGAEnv(t)
	for _, q := range []string{"actor_user_id=x", "actor_user_id=", "actor_user_id=99999999999", "actor_user_id=1.5"} {
		rec, _ := e.get("alice", "ws-alpha", "/api/governance/audit-events?"+q)
		if rec.Code != 422 || !strings.Contains(rec.Body.String(), `"actor_user_id"`) {
			t.Errorf("%s: %d %s", q, rec.Code, rec.Body.String())
		}
	}
}

func TestAuditListBadSince422NoPanic(t *testing.T) {
	e := newGAEnv(t)
	for _, v := range []string{"garbage", "", "2030-13-45", "9999999999999999999999", "0000-00-00", "2030-02-30", "1e5", "%00", "2030-01-01T25:00:00"} {
		for _, p := range []string{"since", "before"} {
			rec, _ := e.get("alice", "ws-alpha", "/api/governance/audit-events?"+p+"="+v)
			if rec.Code != 422 || !strings.Contains(rec.Body.String(), `"`+p+`"`) {
				t.Errorf("%s=%q: %d %s", p, v, rec.Code, rec.Body.String())
			}
		}
	}
}

func TestAuditListForgedCursor422(t *testing.T) {
	e := newGAEnv(t)
	for _, c := range []string{"forged", "eyJhIjoxfQ", "WyJ4Il0", "%00"} {
		rec, _ := e.get("alice", "ws-alpha", "/api/governance/audit-events?cursor="+c)
		if rec.Code != 422 || !strings.Contains(rec.Body.String(), "invalid audit event cursor") {
			t.Errorf("%s: %d %s", c, rec.Code, rec.Body.String())
		}
	}
}

func TestAuditExportMatchesFixtureBytes(t *testing.T) {
	e := newGAEnv(t)
	fx := contract.Load(t, "governance/gov_export_csv_default")
	rec, _ := e.get("alice", "ws-alpha", "/api/governance/audit-events/export.csv")
	var want string
	_ = json.Unmarshal(fx.Response.Body, &want)
	if rec.Code != 200 || rec.Body.String() != want {
		t.Fatalf("status %d, bytes differ (%d vs %d)", rec.Code, rec.Body.Len(), len(want))
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/csv; charset=utf-8" {
		t.Errorf("content-type %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != "attachment; filename=opengtm-audit-events.csv" {
		t.Errorf("content-disposition %q", cd)
	}
}

// slowWriter throttles body writes so the export is still running when the client goes away.
type slowWriter struct {
	http.ResponseWriter
	rows *atomic.Int64
}

func (s slowWriter) Write(p []byte) (int, error) {
	s.rows.Add(1)
	time.Sleep(3 * time.Millisecond)
	return s.ResponseWriter.Write(p)
}
func (s slowWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func TestAuditExportClientDisconnectStopsQuery(t *testing.T) {
	var rows atomic.Int64
	done := make(chan struct{}, 1)
	mw := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "export.csv") {
				defer func() { done <- struct{}{} }()
				w = slowWriter{w, &rows}
			}
			next.ServeHTTP(w, r)
		})
	}
	e := newGAEnv(t, mw)
	ctx := context.Background()
	if err := tenancy.InWorkspace(ctx, e.pool, "ws-alpha", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO governance_audit_events (id, workspace_id, actor_role, method, route, resource_path, response_status, outcome, request_id, metadata_json, created_at)
			SELECT 'bulk-' || g, 'ws-alpha', 'admin', 'GET', '/r', '/p', 200, 'success', 'rid', '{}'::json, timestamp '2031-01-01' + g * interval '1 second' FROM generate_series(1, 3000) g`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	cctx, cancel := context.WithCancel(ctx)
	req, _ := http.NewRequestWithContext(cctx, "GET", srv.URL+"/api/governance/audit-events/export.csv", nil)
	for k, v := range bearer("alice") {
		req.Header.Set(k, v)
	}
	req.Header.Set("X-Workspace-Id", "ws-alpha")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	_, _ = resp.Body.Read(buf)
	cancel()
	_ = resp.Body.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("export handler kept running after the client disconnected")
	}
	if n := rows.Load(); n >= 3060 {
		t.Fatalf("export wrote all %d rows despite the disconnect", n)
	}
}

func TestAuditRoutesRequireAdmin403ForEditor(t *testing.T) {
	e := newGAEnv(t)
	for _, p := range []string{"/api/governance/audit-events", "/api/governance/audit-events/export.csv"} {
		for _, who := range []string{"erin", "bob"} {
			if rec, _ := e.get(who, "ws-alpha", p); rec.Code != 403 {
				t.Errorf("%s %s: %d", who, p, rec.Code)
			}
		}
		if rec, _ := e.get("", "", p); rec.Code != 401 {
			t.Errorf("anon %s: %d", p, rec.Code)
		}
	}
}

func TestAuditRoutesTenantIsolation(t *testing.T) {
	e := newGAEnv(t)
	_, resp := e.get("bob", "ws-beta", "/api/governance/audit-events?limit=500")
	var body struct{ Events []struct{ ID string } }
	_ = json.Unmarshal(resp.Body, &body)
	if len(body.Events) != 4 {
		t.Fatalf("beta saw %d events", len(body.Events))
	}
	for _, ev := range body.Events {
		if !strings.HasPrefix(ev.ID, "22222222") {
			t.Errorf("foreign event %s", ev.ID)
		}
	}
	rec, _ := e.get("bob", "ws-beta", "/api/governance/audit-events/export.csv")
	if strings.Contains(rec.Body.String(), "11111111-0000") {
		t.Error("export leaked alpha rows")
	}
	if rec, _ := e.get("alice", "ws-beta", "/api/governance/audit-events"); rec.Code != 403 && rec.Code != 404 {
		t.Errorf("alice into beta: %d", rec.Code)
	}
}
