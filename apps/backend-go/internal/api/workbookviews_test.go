//go:build integration

package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type viewEnv struct{ colEnv }

func newViewEnv(t *testing.T) viewEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	RegisterWorkbookViews(r, Deps{Pool: pool, Signer: testSigner, Identity: st, Config: config.APIConfig{}, Flags: config.Flags{PGLeadStore: true}})
	e := viewEnv{colEnv{mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool}}}
	e.purge()
	t.Cleanup(e.purge)
	e.seedSeed(t)
	return e
}

const viewsBase = "/api/v2/workbooks/wb-alpha-2/views"

func (e viewEnv) alice(method, path, body string) (int, string) {
	return e.call("alice", "ws-alpha", method, path, body)
}

func (e viewEnv) viewRow(t *testing.T, id string) (name, cfg string, upd time.Time, found bool) {
	t.Helper()
	ctx := context.Background()
	err := tenancy.InWorkspace(ctx, e.pool, "ws-alpha", func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT name, coalesce(config::text,'null'), updated_at FROM workbook_views WHERE id=$1`, id).Scan(&name, &cfg, &upd)
		if err == pgx.ErrNoRows {
			return nil
		}
		found = err == nil
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return
}

func (e viewEnv) viewCount(t *testing.T, ws string) int {
	t.Helper()
	ctx := context.Background()
	var n int
	if err := tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM workbook_views`).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func idOf(t *testing.T, body string) string {
	t.Helper()
	var v struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil || v.ID == "" {
		t.Fatalf("no id in %s", body)
	}
	return v.ID
}

func TestViewsListOrderedOldestFirst(t *testing.T) {
	e := newViewEnv(t)
	// Same created_at: order falls back to id.
	e.exec(t, "ws-alpha", `INSERT INTO workbook_views (id,workspace_id,workbook_id,name,config,created_at,updated_at) VALUES
	 ('view-0-first','ws-alpha','wb-alpha-2','Older','{}'::json,'2026-01-01 00:00:00','2026-01-01 00:00:00'),
	 ('view-alpha-0','ws-alpha','wb-alpha-2','Tie','{}'::json,'2026-08-29 10:00:00','2026-08-29 10:00:00')`)
	code, body := e.alice("GET", viewsBase, "")
	if code != 200 {
		t.Fatal(code, body)
	}
	var got struct {
		Views []struct {
			ID string `json:"id"`
		} `json:"views"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, v := range got.Views {
		ids = append(ids, v.ID)
	}
	if strings.Join(ids, ",") != "view-0-first,view-alpha-0,view-alpha-1,view-alpha-2" || got.Total != 4 {
		t.Fatalf("order %v total %d", ids, got.Total)
	}
	if !strings.HasPrefix(body, `{"views":[{"id":"view-0-first","workbook_id":"wb-alpha-2","name":"Older","config":{"filters":[],"sort":[],"hidden_columns":[]},"created_at":"2026-01-01T00:00:00","updated_at":"2026-01-01T00:00:00"}`) || !strings.HasSuffix(body, `"total":4}`) {
		t.Fatalf("key order: %s", body)
	}
}

func TestCreateExpandsConfigDefaults(t *testing.T) {
	e := newViewEnv(t)
	code, body := e.alice("POST", viewsBase, `{"name":"V","config":{"filters":[{"column":"city"}],"sort":[{"column":"city"}],"extra":1}}`)
	if code != 201 {
		t.Fatal(code, body)
	}
	want := `"name":"V","config":{"filters":[{"column":"city","op":"contains","value":null}],"sort":[{"column":"city","dir":"asc"}],"hidden_columns":[]},`
	if !strings.HasPrefix(body, `{"id":"`) || !strings.Contains(body, `"workbook_id":"wb-alpha-2",`+want) {
		t.Fatalf("body %s", body)
	}
	_, cfg, _, ok := e.viewRow(t, idOf(t, body))
	if !ok || cfg != `{"filters":[{"column":"city","op":"contains","value":null}],"sort":[{"column":"city","dir":"asc"}],"hidden_columns":[]}` {
		t.Fatalf("stored %q", cfg)
	}
	code, body = e.alice("POST", viewsBase, `{"name":"Plain"}`)
	if code != 201 || !strings.Contains(body, `"config":{"filters":[],"sort":[],"hidden_columns":[]}`) {
		t.Fatal(code, body)
	}
}

func TestCreateMissingColumn422(t *testing.T) {
	e := newViewEnv(t)
	before := e.viewCount(t, "ws-alpha")
	code, body := e.alice("POST", viewsBase, `{"name":"B","config":{"filters":[{"column":"nope"}],"sort":[{"column":"zzz"}],"hidden_columns":["aaa","city"]}}`)
	if code != 422 || body != `{"detail":"View references missing columns: aaa, nope, zzz. Refresh the workbook and update the view."}` {
		t.Fatal(code, body)
	}
	if e.viewCount(t, "ws-alpha") != before {
		t.Fatal("a view was written")
	}
}

func TestPutRenameOnlyKeepsConfig(t *testing.T) {
	e := newViewEnv(t)
	_, cfg0, upd0, _ := e.viewRow(t, "view-alpha-1")
	code, body := e.alice("PUT", viewsBase+"/view-alpha-1", `{"name":"Renamed"}`)
	if code != 200 || !strings.Contains(body, `"name":"Renamed"`) || !strings.Contains(body, `"filters":[{"column":"city","op":"equals","value":"Berlin"}]`) {
		t.Fatal(code, body)
	}
	name, cfg, upd, _ := e.viewRow(t, "view-alpha-1")
	if name != "Renamed" || cfg != cfg0 || !upd.After(upd0) {
		t.Fatalf("name %q cfg %q (was %q) updated_at %v -> %v", name, cfg, cfg0, upd0, upd)
	}
	// An empty update still stamps updated_at (Python always does).
	code, _ = e.alice("PUT", viewsBase+"/view-alpha-1", `{}`)
	_, _, upd2, _ := e.viewRow(t, "view-alpha-1")
	if code != 200 || !upd2.After(upd) {
		t.Fatal(code, upd, upd2)
	}
}

func TestPutConfigValidated(t *testing.T) {
	e := newViewEnv(t)
	_, cfg0, upd0, _ := e.viewRow(t, "view-alpha-1")
	code, body := e.alice("PUT", viewsBase+"/view-alpha-1", `{"config":{"hidden_columns":["gone"]}}`)
	if code != 422 || !strings.Contains(body, "View references missing columns: gone.") {
		t.Fatal(code, body)
	}
	_, cfg, upd, _ := e.viewRow(t, "view-alpha-1")
	if cfg != cfg0 || !upd.Equal(upd0) {
		t.Fatal("rejected PUT changed the view")
	}
	code, body = e.alice("PUT", viewsBase+"/view-alpha-1", `{"config":{"sort":[{"column":"city","dir":"desc"}]}}`)
	if code != 200 || !strings.Contains(body, `"filters":[],"sort":[{"column":"city","dir":"desc"}]`) {
		t.Fatal(code, body)
	}
}

func TestViewOfOtherWorkbook404(t *testing.T) {
	e := newViewEnv(t)
	for _, m := range []string{"PUT", "DELETE"} {
		code, body := e.alice(m, "/api/v2/workbooks/wb-alpha-1/views/view-alpha-1", `{"name":"Z"}`)
		if code != 404 || body != `{"detail":"View not found"}` {
			t.Fatal(m, code, body)
		}
		code, body = e.alice(m, viewsBase+"/nope", `{"name":"Z"}`)
		if code != 404 || body != `{"detail":"View not found"}` {
			t.Fatal(m, code, body)
		}
		code, body = e.alice(m, "/api/v2/workbooks/nope/views/view-alpha-1", `{"name":"Z"}`)
		if code != 404 || body != `{"detail":"Workbook not found"}` {
			t.Fatal(m, code, body)
		}
	}
	if name, _, _, ok := e.viewRow(t, "view-alpha-1"); !ok || name != "Berlin only" {
		t.Fatal("view changed")
	}
}

// Review focus 6: a workspace mismatch or another workspace's ids write nothing.
func TestWorkspaceMismatchWritesNothing(t *testing.T) {
	e := newViewEnv(t)
	cases := []struct{ who, ws, m, p, b string }{
		{"bob", "ws-beta", "GET", viewsBase, ""},
		{"bob", "ws-beta", "POST", viewsBase, `{"name":"X"}`},
		{"bob", "ws-beta", "PUT", viewsBase + "/view-alpha-1", `{"name":"X"}`},
		{"bob", "ws-beta", "DELETE", viewsBase + "/view-alpha-1", ""},
		{"bob", "ws-alpha", "DELETE", viewsBase + "/view-alpha-1", ""}, // member, not an editor
		{"alice", "ws-beta", "POST", viewsBase, `{"name":"X"}`},        // alice is no member of ws-beta
	}
	for _, c := range cases {
		code, body := e.call(c.who, c.ws, c.m, c.p, c.b)
		if code != 404 && code != 403 {
			t.Errorf("%s %s as %s/%s: %d %s", c.m, c.p, c.who, c.ws, code, body)
		}
	}
	// Even a ws-beta owner cannot reach ws-alpha's workbook: 404 "Workbook not found".
	code, body := e.call("bob", "ws-beta", "PUT", viewsBase+"/view-alpha-1", `{"name":"X"}`)
	if code != 404 || body != `{"detail":"Workbook not found"}` {
		t.Fatal(code, body)
	}
	if e.viewCount(t, "ws-alpha") != 2 || e.viewCount(t, "ws-beta") != 0 {
		t.Fatal("a view was written")
	}
	if name, _, _, _ := e.viewRow(t, "view-alpha-1"); name != "Berlin only" {
		t.Fatalf("renamed to %q", name)
	}
}

func TestDeleteKeepsRows(t *testing.T) {
	e := newViewEnv(t)
	code, body := e.alice("DELETE", viewsBase+"/view-alpha-1", "")
	if code != 200 || body != `{"status":"deleted"}` {
		t.Fatal(code, body)
	}
	if _, _, _, ok := e.viewRow(t, "view-alpha-1"); ok {
		t.Fatal("view still there")
	}
	ctx := context.Background()
	var n int
	if err := tenancy.InWorkspace(ctx, e.pool, "ws-alpha", func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM workbook_rows WHERE workbook_id='wb-alpha-2'`).Scan(&n)
	}); err != nil || n != 25 {
		t.Fatal(n, err)
	}
	if code, _ = e.alice("DELETE", viewsBase+"/view-alpha-1", ""); code != 404 {
		t.Fatal(code)
	}
}

func TestViewerForbiddenOnWrites(t *testing.T) {
	e := newViewEnv(t)
	for _, c := range []struct{ m, p, b string }{
		{"POST", viewsBase, `{"name":"V"}`},
		{"PUT", viewsBase + "/view-alpha-1", `{"name":"V"}`},
		{"DELETE", viewsBase + "/view-alpha-1", ""},
	} {
		code, body := e.call("bob", "ws-alpha", c.m, c.p, c.b)
		if code != 403 || body != `{"detail":"Workspace permission denied: tables.write"}` {
			t.Fatal(c.m, code, body)
		}
	}
	if code, _ := e.call("bob", "ws-alpha", "GET", viewsBase, ""); code != 200 {
		t.Fatal("viewer may list", code)
	}
	if code, _ := e.call("", "", "GET", viewsBase, ""); code != 401 {
		t.Fatal(code)
	}
}

func TestBadOp422Shape(t *testing.T) {
	e := newViewEnv(t)
	code, body := e.alice("POST", viewsBase, `{"name":"B","config":{"filters":[{"column":"city","op":"like"}]}}`)
	want := `{"detail":[{"type":"literal_error","loc":["body","config","filters",0,"op"],"msg":"Input should be 'equals', 'not_equals', 'contains', 'not_contains', 'empty' or 'not_empty'","input":"like","ctx":{"expected":"'equals', 'not_equals', 'contains', 'not_contains', 'empty' or 'not_empty'"}}]}`
	if code != 422 || body != want {
		t.Fatalf("%d %s", code, body)
	}
}

// A NULL or malformed stored config must not make the list 500 (documented hardening).
func TestMalformedStoredConfigListsAsDefault(t *testing.T) {
	e := newViewEnv(t)
	e.exec(t, "ws-alpha", `UPDATE workbook_views SET config = NULL WHERE id='view-alpha-1'`)
	e.exec(t, "ws-alpha", `UPDATE workbook_views SET config = '{"filters":5}'::json WHERE id='view-alpha-2'`)
	code, body := e.alice("GET", viewsBase, "")
	if code != 200 || strings.Count(body, `"config":{"filters":[],"sort":[],"hidden_columns":[]}`) != 2 {
		t.Fatal(code, body)
	}
}

var viewReplay = []struct{ name, who, ws string }{
	{"list_no_token", "", ""},
	{"list_ok", "alice", "ws-alpha"},
	{"list_viewer_ok", "bob", "ws-alpha"},
	{"list_other_workspace", "bob", "ws-beta"},
	{"list_unknown_workbook", "alice", "ws-alpha"},
	{"list_empty_workbook", "alice", "ws-alpha"},
	{"create_ok", "alice", "ws-alpha"},
	{"create_defaults", "erin", "ws-alpha"},
	{"create_null_value_filter", "alice", "ws-alpha"},
	{"create_missing_column", "alice", "ws-alpha"},
	{"create_bad_op", "alice", "ws-alpha"},
	{"create_bad_dir", "alice", "ws-alpha"},
	{"create_empty_column", "alice", "ws-alpha"},
	{"create_config_not_object", "alice", "ws-alpha"},
	{"create_filters_not_list", "alice", "ws-alpha"},
	{"create_empty_name", "alice", "ws-alpha"},
	{"create_long_name", "alice", "ws-alpha"},
	{"create_name_missing", "alice", "ws-alpha"},
	{"create_name_type", "alice", "ws-alpha"},
	{"create_body_list", "alice", "ws-alpha"},
	{"create_viewer_forbidden", "bob", "ws-alpha"},
	{"create_no_token", "", ""},
	{"create_other_workspace", "bob", "ws-beta"},
	{"create_unknown_workbook", "alice", "ws-alpha"},
	{"create_filter_not_object", "alice", "ws-alpha"},
	{"create_sort_not_object", "alice", "ws-alpha"},
	{"create_sort_missing_column_key", "alice", "ws-alpha"},
	{"create_hidden_not_list", "alice", "ws-alpha"},
	{"create_sort_not_list", "alice", "ws-alpha"},
	{"create_config_null", "alice", "ws-alpha"},
	{"create_multi_errors", "alice", "ws-alpha"},
	{"create_name_and_config_bad", "alice", "ws-alpha"},
	{"put_config_not_object", "alice", "ws-alpha"},
	{"put_filter_not_object", "alice", "ws-alpha"},
	{"put_name_long", "alice", "ws-alpha"},
	{"list_after_create", "alice", "ws-alpha"},
	{"put_rename_only", "alice", "ws-alpha"},
	{"put_config", "erin", "ws-alpha"},
	{"put_name_and_config", "alice", "ws-alpha"},
	{"put_empty_body", "alice", "ws-alpha"},
	{"put_null_fields", "alice", "ws-alpha"},
	{"put_missing_column", "alice", "ws-alpha"},
	{"put_bad_op", "alice", "ws-alpha"},
	{"put_empty_name", "alice", "ws-alpha"},
	{"put_viewer_forbidden", "bob", "ws-alpha"},
	{"put_other_workspace", "bob", "ws-beta"},
	{"put_unknown_view", "alice", "ws-alpha"},
	{"put_view_of_other_workbook", "alice", "ws-alpha"},
	{"put_unknown_workbook", "alice", "ws-alpha"},
	{"list_after_put", "alice", "ws-alpha"},
	{"delete_viewer_forbidden", "bob", "ws-alpha"},
	{"delete_other_workspace", "bob", "ws-beta"},
	{"delete_view_of_other_workbook", "alice", "ws-alpha"},
	{"delete_unknown_workbook", "alice", "ws-alpha"},
	{"delete_ok", "alice", "ws-alpha"},
	{"delete_again", "alice", "ws-alpha"},
	{"delete_seed_view", "erin", "ws-alpha"},
	{"list_after_delete", "alice", "ws-alpha"},
}

// normViewBody maps Go-generated view ids to the recorded Python ids and masks
// the timestamps of views created during the run.
func normViewBody(t *testing.T, raw json.RawMessage, ids map[string]string, gen map[string]bool) json.RawMessage {
	t.Helper()
	if len(raw) == 0 {
		return raw
	}
	s := string(raw)
	for goID, pyID := range ids {
		s = strings.ReplaceAll(s, goID, pyID)
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	var walk func(any)
	walk = func(x any) {
		switch n := x.(type) {
		case map[string]any:
			if id, _ := n["id"].(string); gen[id] {
				n["created_at"], n["updated_at"] = "<ts>", "<ts>"
			}
			for _, c := range n {
				walk(c)
			}
		case []any:
			for _, c := range n {
				walk(c)
			}
		}
	}
	walk(v)
	out, _ := json.Marshal(v)
	return out
}

func TestViewsMatchFixtures(t *testing.T) {
	e := newViewEnv(t)
	ids := map[string]string{} // Go id -> recorded Python id
	pyGen := map[string]bool{}
	seen := map[string]bool{}
	for _, c := range viewReplay {
		fx := contract.Load(t, "views/"+c.name)
		seen["views/"+c.name] = true
		path, body := fx.Request.Path, fixtureBody(fx)
		for goID, pyID := range ids {
			path = strings.ReplaceAll(path, pyID, goID)
		}
		hdr := map[string]string{}
		if c.who != "" {
			hdr = e.hdr(c.who, c.ws)
		}
		_, got := e.do(fx.Request.Method, path, "application/json", body, hdr)
		if strings.HasPrefix(c.name, "create_") && got.Status == 201 {
			var g, p struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(got.Body, &g)
			_ = json.Unmarshal(fx.Response.Body, &p)
			ids[g.ID], pyGen[p.ID] = p.ID, true
		}
		got.Body = normViewBody(t, got.Body, ids, pyGen)
		want := fx.Response
		want.Body = normViewBody(t, want.Body, nil, pyGen)
		t.Run(c.name, func(t *testing.T) { contract.AssertEqual(t, got, want) })
	}
	for _, n := range contractFixtureNames(t, "views") {
		if !seen[n] {
			t.Errorf("fixture never replayed: %s", n)
		}
	}
}
