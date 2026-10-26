//go:build integration

package audit_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type row struct {
	ID, Role, Method, Route, Path, Outcome, ReqID, Meta string
	UserID                                              *int32
	Status                                              int
}

func rows(t *testing.T, pool *pgxpool.Pool, ws string) []row {
	t.Helper()
	var out []row
	err := tenancy.InWorkspace(context.Background(), pool, ws, func(tx pgx.Tx) error {
		rs, err := tx.Query(context.Background(), `SELECT id, actor_user_id, actor_role, method, route, resource_path,
			response_status, outcome, request_id, metadata_json::text FROM governance_audit_events
			WHERE workspace_id=$1 ORDER BY created_at, id`, ws)
		if err != nil {
			return err
		}
		defer rs.Close()
		for rs.Next() {
			var r row
			if err := rs.Scan(&r.ID, &r.UserID, &r.Role, &r.Method, &r.Route, &r.Path, &r.Status, &r.Outcome, &r.ReqID, &r.Meta); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rs.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func clean(t *testing.T, pool *pgxpool.Pool, ws string) {
	t.Helper()
	del := func() {
		_ = tenancy.InWorkspace(context.Background(), pool, ws, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(), `DELETE FROM governance_audit_events WHERE workspace_id=$1`, ws)
			return err
		})
	}
	del()
	t.Cleanup(del)
}

// setActor mimics identity.Workspace: a per-route middleware that writes the
// resolved actor into the holder created by the global audit middleware.
func setActor(ws string) httpx.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ws != "" {
				audit.SetActor(r.Context(), 7, "admin", ws)
			}
			next.ServeHTTP(w, r)
		})
	}
}

func newRouter(pool *pgxpool.Pool, ws string, status int) *httpx.Router {
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	h := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		audit.SetMetadata(req.Context(), map[string]any{"k": "v", "api_token": "zzz"})
		w.WriteHeader(status)
	})
	for _, p := range []string{"POST /api/things", "GET /api/things", "DELETE /api/mcp/tokens/{token_id}"} {
		r.Handle(p, h, setActor(ws))
	}
	return r
}

func do(r *httpx.Router, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(""))
	req.Header.Set("X-Request-Id", "req-1")
	r.Handler().ServeHTTP(rec, req)
	return rec
}

func TestWriteWithWorkspaceRecorded(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := "ws-audit-write"
	clean(t, pool, ws)
	do(newRouter(pool, ws, 201), "POST", "/api/things")
	got := rows(t, pool, ws)
	if len(got) != 1 {
		t.Fatalf("rows=%d", len(got))
	}
	g := got[0]
	if g.Method != "POST" || g.Route != "/api/things" || g.Path != "/api/things" || g.Status != 201 ||
		g.Outcome != "success" || g.ReqID != "req-1" || g.Role != "admin" || g.UserID == nil || *g.UserID != 7 ||
		len(g.ID) != 36 {
		t.Fatalf("bad row %+v", g)
	}
	if strings.Contains(g.Meta, "zzz") || !strings.Contains(g.Meta, `"k"`) {
		t.Fatalf("metadata %s", g.Meta)
	}
}

func TestGetNotRecorded(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := "ws-audit-get"
	clean(t, pool, ws)
	do(newRouter(pool, ws, 200), "GET", "/api/things")
	if n := len(rows(t, pool, ws)); n != 0 {
		t.Fatalf("rows=%d", n)
	}
}

func TestNoWorkspaceNotRecorded(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	clean(t, pool, "ws-audit-nows")
	// Count as the table owner (bypasses RLS) so any insert, whatever its
	// workspace, would be seen.
	owner, err := pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	count := func() int {
		var n int
		if err := owner.QueryRow(context.Background(), `SELECT count(*) FROM governance_audit_events WHERE request_id='req-nows'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	r.Handle("POST /api/things", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }), setActor(""))
	req := httptest.NewRequest("POST", "/api/things", nil)
	rec := httptest.NewRecorder()
	req.Header.Set("X-Request-Id", "req-nows")
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 || count() != 0 {
		t.Fatalf("status %d rows %d", rec.Code, count())
	}
	// Sanity: the same counter does see a recorded request.
	ws := "ws-audit-nows"
	clean(t, pool, ws)
	r2 := newRouter(pool, ws, 200)
	req = httptest.NewRequest("POST", "/api/things", nil)
	req.Header.Set("X-Request-Id", "req-nows")
	r2.Handler().ServeHTTP(httptest.NewRecorder(), req)
	if count() != 1 {
		t.Fatalf("counter blind: %d", count())
	}
}

func TestFlusherPreserved(t *testing.T) {
	var flushed bool
	h := audit.Middleware(nil, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Error("writer lost http.Flusher")
			return
		}
		f.Flush()
		flushed = true
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if !flushed || !rec.Flushed {
		t.Fatal("flush did not reach the underlying writer")
	}
}

func TestRouteIsPatternNotConcretePath(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := "ws-audit-route"
	clean(t, pool, ws)
	do(newRouter(pool, ws, 204), "DELETE", "/api/mcp/tokens/abc123")
	got := rows(t, pool, ws)
	if len(got) != 1 || got[0].Route != "/api/mcp/tokens/{token_id}" || got[0].Path != "/api/mcp/tokens/abc123" {
		t.Fatalf("%+v", got)
	}
}

func TestOutcomeMapping(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := "ws-audit-outcome"
	clean(t, pool, ws)
	want := map[int]string{200: "success", 399: "success", 400: "error", 401: "denied", 403: "denied", 404: "error", 500: "error"}
	for st, out := range want {
		do(newRouter(pool, ws, st), "POST", "/api/things")
		_ = out
	}
	seen := map[int]string{}
	for _, r := range rows(t, pool, ws) {
		seen[r.Status] = r.Outcome
	}
	for st, out := range want {
		if seen[st] != out {
			t.Errorf("status %d: got %q want %q", st, seen[st], out)
		}
	}
}

func TestInsertFailureDoesNotBreakResponse(t *testing.T) {
	pool := testpg.NewAppPool(t, 1)
	r := newRouter(pool, "ws-audit-fail", 201)
	pool.Close() // every insert now fails
	rec := do(r, "POST", "/api/things")
	if rec.Code != 201 {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestPanicRecords500(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := "ws-audit-panic"
	clean(t, pool, ws)
	r := httpx.NewRouter()
	r.Use(audit.Middleware(pool, nil))
	r.Handle("POST /boom", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }), setActor(ws))
	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic was swallowed")
			}
		}()
		do(r, "POST", "/boom")
	}()
	got := rows(t, pool, ws)
	if len(got) != 1 || got[0].Status != 500 || got[0].Outcome != "error" {
		t.Fatalf("%+v", got)
	}
}
