//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

const (
	t9Alpha = "t9-alpha"
	t9Beta  = "t9-beta"
)

type wbEnv struct {
	mcpEnv
	users map[string]map[string]string // alice/bob/erin -> auth headers (ws alpha)
}

func newWBEnv(t *testing.T) wbEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 6)
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	RegisterWorkbooks(r, Deps{Pool: pool, Signer: testSigner, Identity: st, Flags: config.Flags{PGLeadStore: true}})
	e := wbEnv{mcpEnv: mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool}, users: map[string]map[string]string{}}
	e.purge()
	t.Cleanup(e.purge)

	_, e.users["alice"] = e.admin(t, t9Alpha, "t9-alice")
	for _, u := range []struct{ name, key, role string }{{"t9-erin", "erin", "editor"}, {"t9-bob", "bob", "member"}} {
		e.seed(t, u.name, pythonHash, true, "user", false)
		id := e.mustUserByName(t, u.name).ID
		e.member(t, t9Alpha, id, u.role)
		h := bearer(u.name)
		h["X-Workspace-Id"] = t9Alpha
		e.users[u.key] = h
	}
	// bob owns the other tenant
	bob := e.mustUserByName(t, "t9-bob")
	e.workspace(t, t9Beta, bob.ID)
	e.member(t, t9Beta, bob.ID, "admin")
	return e
}

func (e wbEnv) purge() {
	ctx := context.Background()
	for _, ws := range []string{t9Alpha, t9Beta} {
		_ = tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
			_, _ = tx.Exec(ctx, `DELETE FROM workbooks`)
			_, _ = tx.Exec(ctx, `DELETE FROM leads`)
			return nil
		})
	}
}

func (e wbEnv) exec(t *testing.T, ws, q string, args ...any) {
	t.Helper()
	ctx := context.Background()
	if err := tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, q, args...)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// seedContract recreates the seeded part of the recording database that the
// workbook fixtures depend on (scripts/contract_seed.py + the extra leads
// named in contract_cases/workbooks.yaml).
func (e wbEnv) seedContract(t *testing.T) {
	t.Helper()
	list := contract.Load(t, "workbooks/list_seed")
	var body struct {
		Workbooks []struct {
			ID            string          `json:"id"`
			Name          string          `json:"name"`
			Description   string          `json:"description"`
			ColumnsConfig json.RawMessage `json:"columns_config"`
		} `json:"workbooks"`
	}
	if err := json.Unmarshal(list.Response.Body, &body); err != nil || len(body.Workbooks) != 2 {
		t.Fatalf("list_seed fixture: %v", err)
	}
	for _, w := range body.Workbooks {
		e.exec(t, t9Alpha, `INSERT INTO workbooks (id,name,description,status,workspace_id,source_type,source_config,filter_criteria,columns_config,total_rows,completed_rows,sync_to_leads,budget_spent_usd,created_at,updated_at)
			VALUES ($1,$2,$3,'idle',$4,'manual','{}'::json,'{}'::json,$5::json,0,0,false,0,'2026-08-29 10:00:00','2026-08-29 10:00:00')`,
			w.ID, w.Name, w.Description, t9Alpha, string(w.ColumnsConfig))
	}
	for i := 0; i < 25; i++ {
		e.exec(t, t9Alpha, `INSERT INTO workbook_rows (workbook_id, workspace_id, position, data, enrichments) VALUES ('wb-alpha-2',$1,$2,'{}'::json,'{}'::json)`, t9Alpha, i)
	}
	e.exec(t, t9Alpha, `INSERT INTO workbook_views (id,workspace_id,workbook_id,name,config) VALUES
	  ('t9-view-1',$1,'wb-alpha-2','Berlin only','{"filters":[{"column":"city","op":"equals","value":"Berlin"}],"hidden_columns":[]}'::json),
	  ('t9-view-2',$1,'wb-alpha-2','Sorted by company','{"sort":[{"column":"company","dir":"asc"}],"hidden_columns":["phone"]}'::json)`, t9Alpha)
	e.exec(t, t9Beta, `INSERT INTO workbooks (id,name,description,status,workspace_id,source_type,source_config,filter_criteria,columns_config,total_rows,completed_rows,sync_to_leads,budget_spent_usd,created_at,updated_at)
		VALUES ('wb-beta-1','Beta Workbook','beta tenant','idle',$1,'manual','{}'::json,'{}'::json,'[]'::json,0,0,false,0,'2026-08-29 10:00:00','2026-08-29 10:00:00')`, t9Beta)

	lead := func(ws, company, city, email, job string) {
		e.exec(t, ws, `INSERT INTO leads (workspace_id, company, city, email, collection_job_id) VALUES ($1,$2,$3,$4,NULLIF($5,''))`, ws, company, city, email, job)
	}
	for i := 0; i < 3; i++ {
		lead(t9Alpha, fmt.Sprintf("Berlin Co %d", i), "Berlin", "", "")
		lead(t9Alpha, fmt.Sprintf("Austin Co %d", i), "Austin", "a@example.com", "")
	}
	lead(t9Alpha, "Austin NoMail", "Austin", "", "")
	lead(t9Alpha, "Job One A", "Oslo", "", "job-1")
	lead(t9Alpha, "Job One B", "Oslo", "", "job-1")
	lead(t9Alpha, "Job Two A", "Lyon", "", "job-2")
	lead(t9Beta, "Beta Berlin", "Berlin", "", "job-1") // other tenant: must never be snapshotted
}

// workbookSteps is the order of workbooks.yaml; user is who sent the request.
var workbookSteps = []struct{ name, who string }{
	{"list_seed", "alice"}, {"slash_redirect", "alice"}, {"list_no_token", ""},
	{"create_empty", "alice"}, {"create_with_columns", "alice"}, {"create_dup_columns", "alice"}, {"create_cycle", "alice"},
	{"create_max_rows_zero", "alice"}, {"create_max_rows_big", "alice"}, {"create_name_empty", "alice"}, {"create_name_long", "alice"},
	{"create_missing_name", "alice"}, {"create_bad_types", "alice"}, {"create_leads_filter", "alice"}, {"create_legacy_filter", "alice"},
	{"create_csv", "alice"}, {"create_job_results", "alice"}, {"create_viewer", "bob"}, {"create_editor", "erin"}, {"create_no_token", ""},
	{"update_name", "alice"}, {"update_noop", "alice"}, {"update_filter", "alice"}, {"update_columns", "alice"}, {"update_columns_ok", "alice"},
	{"update_dup_ids", "alice"}, {"update_cycle", "alice"}, {"update_removes_referenced", "alice"}, {"update_removes_view_column", "alice"},
	{"update_bad_body", "alice"}, {"update_unknown", "alice"}, {"update_foreign", "alice"}, {"update_viewer", "bob"},
	{"from_jobs_new", "alice"}, {"from_jobs_auto_name", "alice"}, {"from_jobs_merge", "alice"}, {"from_jobs_merge_again", "alice"},
	{"from_jobs_empty", "alice"}, {"from_jobs_missing", "alice"}, {"from_jobs_unknown_wb", "alice"}, {"from_jobs_foreign_wb", "alice"},
	{"from_jobs_viewer", "bob"}, {"list_after", "alice"}, {"delete_ok", "alice"}, {"delete_again", "alice"}, {"delete_foreign", "alice"},
	{"delete_viewer", "bob"}, {"delete_no_token", ""}, {"list_final", "alice"},
}

// normalise replaces generated ids and timestamps (everything the seed does not
// fix) so list bodies compare structurally.
func normalise(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			s, isStr := x.(string)
			switch {
			case k == "id" && isStr && len(s) == 36:
				t[k] = "<uuid>"
			case (k == "created_at" || k == "updated_at") && isStr && !strings.HasPrefix(s, "2026-08-29"):
				t[k] = "<ts>"
			default:
				t[k] = normalise(x)
			}
		}
	case []any:
		for i := range t {
			t[i] = normalise(t[i])
		}
	}
	return v
}

func normBody(raw json.RawMessage) json.RawMessage {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return raw
	}
	b, _ := json.Marshal(normalise(v))
	return b
}

// replayWorkbooks runs every recorded case in order (state carries over) and
// asserts the cases in `check` (all when nil).
func replayWorkbooks(t *testing.T, check map[string]bool) {
	e := newWBEnv(t)
	e.seedContract(t)
	idMap := map[string]string{} // recorded generated id -> id issued by Go
	subst := func(s string) string {
		for from, to := range idMap {
			s = strings.ReplaceAll(s, from, to)
		}
		return s
	}
	for _, st := range workbookSteps {
		fx := contract.Load(t, "workbooks/"+st.name)
		var hdr map[string]string
		if st.who != "" {
			hdr = e.users[st.who]
		}
		body := ""
		if len(fx.Request.Body) > 0 && string(fx.Request.Body) != "null" {
			body = subst(string(fx.Request.Body))
		}
		rec, got := e.do(fx.Request.Method, subst(fx.Request.Path), "application/json", body, hdr)
		// learn the generated id of a created workbook
		if fx.Request.Method == "POST" && fx.Response.Status == 201 {
			var want, have struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(fx.Response.Body, &want)
			_ = json.Unmarshal(got.Body, &have)
			if want.ID != "" && have.ID != "" {
				idMap[want.ID] = have.ID
			}
		}
		if check != nil && !check[st.name] {
			continue
		}
		t.Run(st.name, func(t *testing.T) {
			if st.name == "slash_redirect" {
				if rec.Code != 307 || rec.Header().Get("Location") != "/api/workbooks/" {
					t.Fatalf("redirect: %d %q", rec.Code, rec.Header().Get("Location"))
				}
				return
			}
			want := fx.Response
			got.Body, want.Body = normBody(got.Body), normBody(want.Body)
			contract.AssertEqual(t, got, want)
		})
	}
}

func TestWorkbookRoutesMatchFixtures(t *testing.T) { replayWorkbooks(t, nil) }

func TestListMatchesFixture(t *testing.T) {
	replayWorkbooks(t, map[string]bool{"list_seed": true, "list_no_token": true, "list_after": true, "list_final": true})
}

func TestCreateEmptyMatchesFixture(t *testing.T) {
	replayWorkbooks(t, map[string]bool{"create_empty": true, "create_with_columns": true, "create_editor": true})
}

func TestFromJobsMatchesFixture(t *testing.T) {
	replayWorkbooks(t, map[string]bool{"from_jobs_new": true, "from_jobs_auto_name": true, "from_jobs_merge": true, "from_jobs_merge_again": true,
		"from_jobs_empty": true, "from_jobs_missing": true, "from_jobs_unknown_wb": true, "from_jobs_foreign_wb": true})
}

func TestTrailingSlashRedirect307(t *testing.T) {
	e := newWBEnv(t)
	rec, _ := e.do("GET", "/api/workbooks", "", "", e.users["alice"])
	if rec.Code != 307 || rec.Header().Get("Location") != "/api/workbooks/" {
		t.Fatalf("%d %q", rec.Code, rec.Header().Get("Location"))
	}
	rec, _ = e.do("GET", "/api/workbooks?x=1", "", "", e.users["alice"])
	if rec.Code != 307 || rec.Header().Get("Location") != "/api/workbooks/?x=1" {
		t.Fatalf("query must survive the redirect: %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestViewerCannotCreate403(t *testing.T) {
	e := newWBEnv(t)
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/api/workbooks/", `{"name":"x"}`},
		{"POST", "/api/workbooks/from-jobs", `{"job_ids":["a"]}`},
		{"PUT", "/api/workbooks/anything", `{"name":"x"}`},
		{"DELETE", "/api/workbooks/anything", ``},
	} {
		rec, _ := e.do(c.method, c.path, "application/json", c.body, e.users["bob"])
		if rec.Code != 403 || !strings.Contains(rec.Body.String(), "Workspace permission denied: tables.write") {
			t.Errorf("%s %s: %d %s", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
	// readers may list
	if rec, _ := e.do("GET", "/api/workbooks/", "", "", e.users["bob"]); rec.Code != 200 {
		t.Errorf("list as non-editor: %d", rec.Code)
	}
}

func TestOtherWorkspaceWorkbookIs404(t *testing.T) {
	e := newWBEnv(t)
	e.seedContract(t)
	for _, c := range []struct{ method, path, body string }{
		{"PUT", "/api/workbooks/wb-beta-1", `{"name":"x"}`},
		{"DELETE", "/api/workbooks/wb-beta-1", ``},
		{"POST", "/api/workbooks/from-jobs", `{"job_ids":["job-1"],"workbook_id":"wb-beta-1"}`},
	} {
		rec, _ := e.do(c.method, c.path, "application/json", c.body, e.users["alice"])
		if rec.Code != 404 || strings.TrimSpace(rec.Body.String()) != `{"detail":"Workbook not found"}` {
			t.Errorf("%s %s: %d %s", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
	var n int
	_ = tenancy.InWorkspace(context.Background(), e.pool, t9Beta, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM workbooks WHERE id='wb-beta-1' AND name='Beta Workbook'`).Scan(&n)
	})
	if n != 1 {
		t.Fatal("foreign workbook was modified or deleted")
	}
}
