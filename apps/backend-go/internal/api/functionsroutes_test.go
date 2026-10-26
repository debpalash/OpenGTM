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

type fnEnv struct {
	mcpEnv
}

func newFnEnv(t *testing.T) fnEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 4)
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	RegisterFunctions(r, Deps{Pool: pool, Signer: testSigner, Identity: st, Config: config.APIConfig{}, Flags: config.Flags{PGLeadStore: true}})
	e := fnEnv{mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool}}
	e.purge()
	t.Cleanup(e.purge)
	return e
}

func (e fnEnv) purge() {
	_, _ = e.pool.Exec(context.Background(), `DELETE FROM workbook_functions WHERE id LIKE 't15-%' OR category LIKE 't15-%' OR name LIKE 't15 %'`)
}

// user seeds a member of ws with role (creating ws owned by a throwaway owner on first use).
func (e fnEnv) user(t *testing.T, ws, name, role string, globalAdmin bool) map[string]string {
	t.Helper()
	e.seed(t, name, pythonHash, true, "user", globalAdmin)
	u := e.mustUserByName(t, name)
	if !e.wsExists(ws) {
		e.seed(t, name+"-own", pythonHash, true, "user", false)
		o := e.mustUserByName(t, name+"-own")
		e.workspace(t, ws, o.ID)
	}
	e.member(t, ws, u.ID, role)
	h := bearer(name)
	h["X-Workspace-Id"] = ws
	return h
}

func (e fnEnv) fn(t *testing.T, id, name, cat string, usage int, chain string) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(),
		`INSERT INTO workbook_functions (id,name,description,category,columns_chain,version,usage_count,created_at,updated_at)
		 VALUES ($1,$2,'d',$3,$4::jsonb,1,$5,1788000000,1788000000)`, id, name, cat, chain, usage); err != nil {
		t.Fatal(err)
	}
}

func (e fnEnv) workbookIn(t *testing.T, ws, id, cols string) {
	t.Helper()
	ctx := context.Background()
	err := tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
		var c any
		if cols != "" {
			c = cols
		}
		_, err := tx.Exec(ctx, `INSERT INTO workbooks (id,name,workspace_id,columns_config,updated_at) VALUES ($1,$1,$2,$3::json,'2026-01-01 00:00:00')`, id, ws, c)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = tenancy.InWorkspace(context.Background(), e.pool, ws, func(tx pgx.Tx) error {
			_, _ = tx.Exec(context.Background(), `DELETE FROM workbooks WHERE id=$1`, id)
			return nil
		})
	})
}

func (e fnEnv) wbCols(t *testing.T, ws, id string) (cols string, updated time.Time) {
	t.Helper()
	ctx := context.Background()
	err := tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT coalesce(columns_config::text,'null'), updated_at FROM workbooks WHERE id=$1`, id).Scan(&cols, &updated)
	})
	if err != nil {
		t.Fatal(err)
	}
	return
}

func (e fnEnv) usage(t *testing.T, id string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT usage_count FROM workbook_functions WHERE id=$1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e fnEnv) count(t *testing.T, where string, args ...any) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM workbook_functions WHERE `+where, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e fnEnv) call(method, path, body string, hdr map[string]string) (int, map[string]any) {
	rec, _ := e.do(method, path, "application/json", body, hdr)
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m
}

func TestListFilterByCategory(t *testing.T) {
	e := newFnEnv(t)
	h := e.user(t, "t15-ws1", "t15-u1", "editor", false)
	e.fn(t, "t15-a1", "t15 a1", "t15-a", 1, `[{"id":"x"}]`)
	e.fn(t, "t15-a2", "t15 a2", "t15-a", 5, `[{"id":"x"},{"id":"y"}]`)
	e.fn(t, "t15-b1", "t15 b1", "t15-b", 9, `[]`)

	rec, _ := e.do("GET", "/api/functions?category=t15-a", "", "", h)
	var out struct {
		Functions []map[string]any `json:"functions"`
		Total     int              `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if out.Total != 2 || len(out.Functions) != 2 || out.Functions[0]["id"] != "t15-a2" || out.Functions[1]["id"] != "t15-a1" {
		t.Fatalf("filter/order wrong: %s", rec.Body.String())
	}
	if out.Functions[0]["columns_count"] != float64(2) {
		t.Fatalf("columns_count: %v", out.Functions[0])
	}
	// Python key order: id,name,description,category,columns_chain,columns_count,version,usage_count,created_at
	body := rec.Body.String()
	last := -1
	for _, k := range []string{`"id"`, `"name"`, `"description"`, `"category"`, `"columns_chain"`, `"columns_count"`, `"version"`, `"usage_count"`, `"created_at"`} {
		i := strings.Index(body, k)
		if i <= last {
			t.Fatalf("key order: %s at %d after %d in %s", k, i, last, body)
		}
		last = i
	}
	if !strings.Contains(body, `"created_at":1788000000.0`) {
		t.Fatalf("created_at must serialise as a float: %s", body)
	}

	// empty category behaves as no filter (Python `if category:`)
	_, all := e.call("GET", "/api/functions?category=", "", h)
	if all["total"].(float64) < 3 {
		t.Fatalf("empty category must list everything: %v", all["total"])
	}
	_, none := e.call("GET", "/api/functions?category=t15-none", "", h)
	if none["total"] != float64(0) || string(fnJSON(none["functions"])) != "[]" {
		t.Fatalf("%v", none)
	}
}

func fnJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func TestListRequiresAuth(t *testing.T) {
	e := newFnEnv(t)
	fx := contract.Load(t, "functions/list_no_token")
	_, got := e.do("GET", "/api/functions", "", "", nil)
	contract.AssertEqual(t, got, fx.Response)
}

func TestFunctionsGetUnknown404(t *testing.T) {
	e := newFnEnv(t)
	h := e.user(t, "t15-ws2", "t15-u2", "editor", false)
	fx := contract.Load(t, "functions/get_unknown")
	_, got := e.do("GET", "/api/functions/nope-t15", "", "", h)
	contract.AssertEqual(t, got, fx.Response)
}

func TestGetShapeAndNullableColumns(t *testing.T) {
	e := newFnEnv(t)
	h := e.user(t, "t15-ws3", "t15-u3", "viewer", false)
	e.fn(t, "t15-g1", "t15 g1", "t15-g", 3, `[{"z":1,"id":"c1","n":{"b":1.5,"a":null}}]`)
	if _, err := e.pool.Exec(context.Background(), `UPDATE workbook_functions SET description=NULL, category=NULL WHERE id='t15-g1'`); err != nil {
		t.Fatal(err)
	}
	rec, _ := e.do("GET", "/api/functions/t15-g1", "", "", h)
	// jsonb key order (length, then bytewise) is what psycopg hands to Python
	want := `{"id":"t15-g1","name":"t15 g1","description":null,"category":null,"columns_chain":[{"n":{"a":null,"b":1.5},"z":1,"id":"c1"}],"version":1,"usage_count":3}`
	if rec.Code != 200 || rec.Body.String() != want {
		t.Fatalf("%d\n got %s\nwant %s", rec.Code, rec.Body.String(), want)
	}
}

func TestCreateRequiresGlobalAdmin(t *testing.T) {
	e := newFnEnv(t)
	h := e.user(t, "t15-ws4", "t15-u4", "admin", false) // workspace admin, not global
	fx := contract.Load(t, "functions/create_non_admin")
	_, got := e.do("POST", "/api/functions", "application/json", `{"name":"t15 nope","columns_chain":[]}`, h)
	contract.AssertEqual(t, got, fx.Response)
	if e.count(t, `name='t15 nope'`) != 0 {
		t.Fatal("non-admin created a function")
	}
	fx = contract.Load(t, "functions/delete_non_admin")
	e.fn(t, "t15-d0", "t15 d0", "t15-d", 0, `[]`)
	_, got = e.do("DELETE", "/api/functions/t15-d0", "", "", h)
	contract.AssertEqual(t, got, fx.Response)
	if e.count(t, `id='t15-d0'`) != 1 {
		t.Fatal("non-admin deleted a function")
	}
}

func TestFunctionsCreateShapeMatchesFixture(t *testing.T) {
	e := newFnEnv(t)
	h := e.user(t, "t15-ws5", "t15-u5", "admin", true)
	for _, c := range []struct {
		fx, body string
		chain    string
	}{
		{"functions/create_ok", "", `[{"id":"c1","name":"C1","type":"lead_field","extra":{"z":1,"a":[1.5,null]}},{"key":"legacy"}]`},
		{"functions/create_defaults", "", `[]`},
	} {
		fx := contract.Load(t, c.fx)
		before := float64(time.Now().Unix()) - 2
		rec, got := e.do("POST", "/api/functions", "application/json", string(fx.Request.Body), h)
		contract.AssertEqual(t, got, fx.Response, contract.IgnorePaths("$.id"))
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		id, _ := out["id"].(string)
		if id == "" {
			t.Fatalf("%s: no id: %s", c.fx, rec.Body.String())
		}
		t.Cleanup(func() { _, _ = e.pool.Exec(context.Background(), `DELETE FROM workbook_functions WHERE id=$1`, id) })
		var (
			chain, desc, cat string
			ver, usage       int
			cr, up           float64
		)
		if err := e.pool.QueryRow(context.Background(),
			`SELECT columns_chain::text, description, category, version, usage_count, created_at, updated_at FROM workbook_functions WHERE id=$1`, id).
			Scan(&chain, &desc, &cat, &ver, &usage, &cr, &up); err != nil {
			t.Fatal(err)
		}
		if ver != 1 || usage != 0 || cr != up || cr < before || cr > float64(time.Now().Unix())+2 {
			t.Fatalf("%s: stored ver=%d usage=%d created=%v updated=%v", c.fx, ver, usage, cr, up)
		}
		var a, b any
		_ = json.Unmarshal([]byte(chain), &a)
		_ = json.Unmarshal([]byte(c.chain), &b)
		if string(fnJSON(a)) != string(fnJSON(b)) {
			t.Fatalf("%s: chain %s != %s", c.fx, chain, c.chain)
		}
		if c.fx == "functions/create_defaults" && (desc != "" || cat != "custom") {
			t.Fatalf("defaults: desc=%q cat=%q", desc, cat)
		}
	}
}

func TestFunctionsCreateValidationMatchesFixtures(t *testing.T) {
	e := newFnEnv(t)
	h := e.user(t, "t15-ws6", "t15-u6", "admin", true)
	for _, name := range []string{
		"create_missing_name", "create_missing_chain", "create_chain_not_dict", "create_bad_types", "create_no_body",
	} {
		fx := contract.Load(t, "functions/"+name)
		body := string(fx.Request.Body)
		if body == "null" { // the recorded request had no body at all
			body = ""
		}
		_, got := e.do("POST", "/api/functions", "application/json", body, h)
		contract.AssertEqual(t, got, fx.Response)
	}
	if e.count(t, `name='X'`) != 0 {
		t.Fatal("invalid create persisted")
	}
}

func TestDeleteUnknownStillOK(t *testing.T) {
	e := newFnEnv(t)
	h := e.user(t, "t15-ws7", "t15-u7", "admin", true)
	fx := contract.Load(t, "functions/delete_unknown")
	_, got := e.do("DELETE", "/api/functions/t15-never", "", "", h)
	contract.AssertEqual(t, got, fx.Response)
	e.fn(t, "t15-del", "t15 del", "t15-d", 0, `[]`)
	_, got = e.do("DELETE", "/api/functions/t15-del", "", "", h)
	contract.AssertEqual(t, got, contract.Load(t, "functions/delete_ok").Response)
	_, got = e.do("DELETE", "/api/functions/t15-del", "", "", h)
	contract.AssertEqual(t, got, contract.Load(t, "functions/delete_again").Response)
	if e.count(t, `id='t15-del'`) != 0 {
		t.Fatal("not deleted")
	}
}

func TestApplyAppendsOnlyNewColumns(t *testing.T) {
	e := newFnEnv(t)
	h := e.user(t, "t15-ws8", "t15-u8", "editor", false)
	e.workbookIn(t, "t15-ws8", "t15-wb1", `[{"id":"a","name":"A"},{"key":"b"}]`)
	e.fn(t, "t15-f1", "t15 f1", "t15-f", 0, `[{"id":"a","name":"dup"},{"id":"c","name":"C"},{"key":"b"},{"key":"d"},{"id":"c","name":"again"}]`)
	_, before := e.wbCols(t, "t15-ws8", "t15-wb1")

	code, out := e.call("POST", "/api/functions/t15-f1/apply", `{"workbook_id":"t15-wb1"}`, h)
	if code != 200 || out["applied"] != "t15 f1" || out["columns_added"] != float64(2) || out["total_columns"] != float64(4) {
		t.Fatalf("%d %v", code, out)
	}
	rec, _ := e.do("POST", "/api/functions/t15-f1/apply", "application/json", `{"workbook_id":"t15-wb1"}`, h)
	if rec.Body.String() != `{"applied":"t15 f1","columns_added":0,"total_columns":4}` {
		t.Fatalf("second apply: %s", rec.Body.String())
	}
	cols, after := e.wbCols(t, "t15-ws8", "t15-wb1")
	var got []map[string]any
	_ = json.Unmarshal([]byte(cols), &got)
	if string(fnJSON(got)) != `[{"id":"a","name":"A"},{"key":"b"},{"id":"c","name":"C"},{"key":"d"}]` {
		t.Fatalf("columns_config: %s", cols)
	}
	if !after.After(before) {
		t.Fatalf("updated_at not advanced: %v -> %v", before, after)
	}
}

func TestApplyNoChangeKeepsUpdatedAtAndNullConfig(t *testing.T) {
	e := newFnEnv(t)
	h := e.user(t, "t15-ws9", "t15-u9", "editor", false)
	e.workbookIn(t, "t15-ws9", "t15-wb2", `[{"id":"a"}]`)
	e.workbookIn(t, "t15-ws9", "t15-wb3", ``) // NULL columns_config
	e.fn(t, "t15-f2", "t15 f2", "t15-f", 0, `[{"id":"a"}]`)
	_, before := e.wbCols(t, "t15-ws9", "t15-wb2")
	code, out := e.call("POST", "/api/functions/t15-f2/apply", `{"workbook_id":"t15-wb2"}`, h)
	if code != 200 || out["columns_added"] != float64(0) || out["total_columns"] != float64(1) {
		t.Fatalf("%d %v", code, out)
	}
	// SQLAlchemy skips the UPDATE when the list is unchanged, so updated_at stays put.
	if _, after := e.wbCols(t, "t15-ws9", "t15-wb2"); !after.Equal(before) {
		t.Fatalf("updated_at changed on no-op apply: %v -> %v", before, after)
	}
	code, out = e.call("POST", "/api/functions/t15-f2/apply", `{"workbook_id":"t15-wb3"}`, h)
	if code != 200 || out["columns_added"] != float64(1) || out["total_columns"] != float64(1) {
		t.Fatalf("null config: %d %v", code, out)
	}
}

func TestApplyDoesNotValidateCycles(t *testing.T) {
	e := newFnEnv(t)
	h := e.user(t, "t15-ws10", "t15-u10", "editor", false)
	e.workbookIn(t, "t15-ws10", "t15-wb4", `[{"id":"a","type":"ai_formula","prompt":"{B}","input_columns":["b"],"name":"A"}]`)
	e.fn(t, "t15-f3", "t15 f3", "t15-f", 0, `[{"id":"b","type":"ai_formula","prompt":"{A}","input_columns":["a"],"name":"B"}]`)
	code, out := e.call("POST", "/api/functions/t15-f3/apply", `{"workbook_id":"t15-wb4"}`, h)
	if code != 200 || out["columns_added"] != float64(1) {
		t.Fatalf("parity: apply must not run cycle validation: %d %v", code, out)
	}
}

func TestApplyOtherWorkspaceWorkbook404(t *testing.T) {
	e := newFnEnv(t)
	h := e.user(t, "t15-ws11", "t15-u11", "editor", false)
	e.seed(t, "t15-u11b", pythonHash, true, "user", false)
	other := e.mustUserByName(t, "t15-u11b")
	e.workspace(t, "t15-ws11b", other.ID)
	e.workbookIn(t, "t15-ws11b", "t15-wb5", `[]`)
	e.fn(t, "t15-f4", "t15 f4", "t15-f", 0, `[{"id":"z"}]`)

	fx := contract.Load(t, "functions/apply_other_workspace")
	_, got := e.do("POST", "/api/functions/t15-f4/apply", "application/json", `{"workbook_id":"t15-wb5"}`, h)
	contract.AssertEqual(t, got, fx.Response)
	if cols, _ := e.wbCols(t, "t15-ws11b", "t15-wb5"); cols != "[]" {
		t.Fatalf("foreign workbook modified: %s", cols)
	}
	if e.usage(t, "t15-f4") != 0 {
		t.Fatal("usage bumped on 404")
	}
	// unknown workbook and unknown function: workbook is checked first
	_, got = e.do("POST", "/api/functions/nope/apply", "application/json", `{"workbook_id":"nope"}`, h)
	contract.AssertEqual(t, got, contract.Load(t, "functions/apply_unknown_both").Response)
	e.workbookIn(t, "t15-ws11", "t15-wb6", `[]`)
	_, got = e.do("POST", "/api/functions/nope/apply", "application/json", `{"workbook_id":"t15-wb6"}`, h)
	contract.AssertEqual(t, got, contract.Load(t, "functions/apply_unknown_function").Response)
}

func TestApplyBumpsUsageOnce(t *testing.T) {
	e := newFnEnv(t)
	h := e.user(t, "t15-ws12", "t15-u12", "editor", false)
	e.workbookIn(t, "t15-ws12", "t15-wb7", `[]`)
	e.fn(t, "t15-f5", "t15 f5", "t15-f", 0, `[{"id":"a"}]`)
	var upd0 float64
	_ = e.pool.QueryRow(context.Background(), `SELECT updated_at FROM workbook_functions WHERE id='t15-f5'`).Scan(&upd0)
	e.call("POST", "/api/functions/t15-f5/apply", `{"workbook_id":"t15-wb7"}`, h)
	if n := e.usage(t, "t15-f5"); n != 1 {
		t.Fatalf("usage after first apply = %d", n)
	}
	e.call("POST", "/api/functions/t15-f5/apply", `{"workbook_id":"t15-wb7"}`, h)
	if n := e.usage(t, "t15-f5"); n != 2 {
		t.Fatalf("usage after second apply = %d", n)
	}
	var upd1 float64
	_ = e.pool.QueryRow(context.Background(), `SELECT updated_at FROM workbook_functions WHERE id='t15-f5'`).Scan(&upd1)
	if upd1 != upd0 {
		t.Fatal("Python's usage bump does not touch updated_at")
	}
}

func TestApplyFunctionNeedsEditorRole(t *testing.T) {
	e := newFnEnv(t)
	h := e.user(t, "t15-ws13", "t15-u13", "viewer", false)
	e.workbookIn(t, "t15-ws13", "t15-wb8", `[]`)
	e.fn(t, "t15-f6", "t15 f6", "t15-f", 0, `[{"id":"a"}]`)
	fx := contract.Load(t, "functions/apply_non_editor")
	_, got := e.do("POST", "/api/functions/t15-f6/apply", "application/json", `{"workbook_id":"t15-wb8"}`, h)
	contract.AssertEqual(t, got, fx.Response)
	if cols, _ := e.wbCols(t, "t15-ws13", "t15-wb8"); cols != "[]" || e.usage(t, "t15-f6") != 0 {
		t.Fatal("viewer mutated state")
	}
}

func TestApplyValidationMatchesFixtures(t *testing.T) {
	e := newFnEnv(t)
	h := e.user(t, "t15-ws14", "t15-u14", "editor", false)
	for _, name := range []string{"apply_missing_body_field", "apply_bad_type"} {
		fx := contract.Load(t, "functions/"+name)
		_, got := e.do("POST", "/api/functions/fn-1/apply", "application/json", string(fx.Request.Body), h)
		contract.AssertEqual(t, got, fx.Response)
	}
}

func TestFunctionsTableIsGlobalAcrossWorkspaces(t *testing.T) {
	e := newFnEnv(t)
	adminA := e.user(t, "t15-ws15a", "t15-u15a", "admin", true)
	userB := e.user(t, "t15-ws15b", "t15-u15b", "viewer", false)
	code, out := e.call("POST", "/api/functions", `{"name":"t15 shared","category":"t15-shared","columns_chain":[{"id":"q"}]}`, adminA)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	id := out["id"].(string)
	t.Cleanup(func() { _, _ = e.pool.Exec(context.Background(), `DELETE FROM workbook_functions WHERE id=$1`, id) })
	code, got := e.call("GET", "/api/functions/"+id, "", userB)
	if code != 200 || got["name"] != "t15 shared" {
		t.Fatalf("other workspace cannot see global function: %d %v", code, got)
	}
	_, list := e.call("GET", "/api/functions?category=t15-shared", "", userB)
	if list["total"] != float64(1) {
		t.Fatalf("%v", list)
	}
}
