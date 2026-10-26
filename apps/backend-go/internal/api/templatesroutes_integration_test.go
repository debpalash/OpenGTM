//go:build integration

package api

import (
	"context"
	"encoding/json"
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
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbregistry"
)

type tplEnv struct{ detailEnv }

func newTplEnv(t *testing.T, mod func(*wbregistry.Registry)) tplEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	d := Deps{Pool: pool, Signer: testSigner, Identity: st, Config: config.APIConfig{}, Flags: config.Flags{PGLeadStore: true}}
	reg, err := wbregistry.Load()
	if err != nil {
		t.Fatal(err)
	}
	if mod != nil {
		mod(reg)
	}
	RegisterWorkbooks(r, d)
	registerTemplates(r, d, reg)
	e := tplEnv{detailEnv{mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool}}}
	e.purge()
	t.Cleanup(e.purge)
	e.seedDetail(t)
	e.seed(t, "erin", pythonHash, true, "user", false)
	e.member(t, "ws-alpha", e.mustUserByName(t, "erin").ID, "editor")
	return e
}

func (e tplEnv) send(who, ws, method, path, body string) (int, string) {
	hdr := map[string]string{}
	if who != "" {
		hdr = bearer(who)
		hdr["X-Workspace-Id"] = ws
	}
	ct := ""
	if body != "" {
		ct = "application/json"
	}
	rec, _ := e.do(method, path, ct, body, hdr)
	return rec.Code, rec.Body.String()
}

func (e tplEnv) count(t *testing.T, ws string) int {
	t.Helper()
	var n int
	err := tenancy.InWorkspace(context.Background(), e.pool, ws, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), "select count(*) from workbooks").Scan(&n)
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func tplActor(name string) (who, ws string) {
	switch name {
	case "instantiate_no_token", "create_no_token":
		return "", ""
	case "instantiate_viewer", "create_viewer":
		return "bob", "ws-alpha"
	case "instantiate_beta_owner":
		return "bob", "ws-beta"
	case "instantiate_foreign_ws", "create_foreign_ws":
		return "erin", "ws-beta"
	case "instantiate_name", "create_hot_leads":
		return "erin", "ws-alpha"
	}
	return "alice", "ws-alpha"
}

func TestTemplateWriteRoutesMatchFixtures(t *testing.T) {
	e := newTplEnv(t, nil)
	for _, n := range contractFixtureNames(t, "templates") {
		n = strings.TrimPrefix(n, "templates/")
		fx := contract.Load(t, "templates/"+n)
		if fx.Request.Method != "POST" || strings.HasPrefix(n, "token_") {
			continue
		}
		who, ws := tplActor(n)
		body := ""
		if len(fx.Request.Body) > 0 && string(fx.Request.Body) != "null" {
			body = string(fx.Request.Body)
		}
		code, out := e.send(who, ws, "POST", fx.Request.Path, body)
		got := contract.Resp{Status: code, Body: json.RawMessage(out)}
		if code == 200 || code == 201 || code >= 400 {
			got.Headers = map[string]string{"content-type": "application/json"}
		}
		fx.Response.Headers = map[string]string{"content-type": "application/json"}
		t.Run(n, func(t *testing.T) {
			contract.AssertEqual(t, got, fx.Response, contract.IgnorePaths("$.id", "$.created_at", "$.updated_at"))
		})
	}
}

func TestInstantiateMatchesCreateResponse(t *testing.T) {
	e := newTplEnv(t, nil)
	reg, _ := wbregistry.Load()
	rc, _ := reg.Recipe("founder-outreach")
	var wb struct {
		Description string          `json:"description"`
		Columns     json.RawMessage `json:"columns"`
	}
	_ = json.Unmarshal(rc.Workbook, &wb)
	desc := wb.Description
	if desc == "" {
		desc = rc.Description
	}
	cb, _ := json.Marshal(map[string]any{"name": rc.Name, "description": desc, "source": "empty", "columns_config": wb.Columns})
	c1, viaCreate := e.send("alice", "ws-alpha", "POST", "/api/workbooks/", string(cb))
	c2, viaInst := e.send("alice", "ws-alpha", "POST", "/api/templates/gallery/founder-outreach/instantiate", "")
	if c1 != 201 || c2 != 201 {
		t.Fatalf("create %d (%.200s) instantiate %d (%.200s)", c1, viaCreate, c2, viaInst)
	}
	contract.AssertEqual(t, contract.Resp{Status: c2, Body: json.RawMessage(viaInst)}, contract.Resp{Status: c1, Body: json.RawMessage(viaCreate)},
		contract.IgnorePaths("$.id", "$.created_at", "$.updated_at"))
}

func TestInstantiateNameOverride(t *testing.T) {
	e := newTplEnv(t, nil)
	code, out := e.send("alice", "ws-alpha", "POST", "/api/templates/gallery/founder-outreach/instantiate", `{"name":"Custom name"}`)
	var wb struct{ Name string }
	if code != 201 || json.Unmarshal([]byte(out), &wb) != nil || wb.Name != "Custom name" {
		t.Fatalf("%d %.200s", code, out)
	}
	if code, out := e.send("alice", "ws-alpha", "POST", "/api/templates/gallery/founder-outreach/instantiate", `{"name":""}`); code != 422 {
		t.Fatalf("empty name: %d %s", code, out)
	}
}

func TestInstantiateViewerForbidden(t *testing.T) {
	e := newTplEnv(t, nil)
	before := e.count(t, "ws-alpha")
	code, out := e.send("bob", "ws-alpha", "POST", "/api/templates/gallery/founder-outreach/instantiate", "")
	if code != 403 || !strings.Contains(out, "tables.write") {
		t.Fatalf("%d %s", code, out)
	}
	code, _ = e.send("bob", "ws-alpha", "POST", "/api/templates/saas-ctos/create", "")
	if code != 403 || e.count(t, "ws-alpha") != before {
		t.Fatalf("viewer create: %d, count changed", code)
	}
}

func TestCreateFromTemplateColumnsAndCounts(t *testing.T) {
	e := newTplEnv(t, nil)
	reg, _ := wbregistry.Load()
	tp, _ := reg.Template("saas-ctos")
	code, out := e.send("alice", "ws-alpha", "POST", "/api/templates/saas-ctos/create", "")
	var resp struct {
		ID                     string `json:"id"`
		Name                   string `json:"name"`
		Template               string `json:"template"`
		ColumnsCount           int    `json:"columns_count"`
		EnrichmentColumnsCount int    `json:"enrichment_columns_count"`
	}
	if code != 200 || json.Unmarshal([]byte(out), &resp) != nil {
		t.Fatalf("%d %s", code, out)
	}
	var fields, enrich []json.RawMessage
	_ = json.Unmarshal(tp.Columns, &fields)
	_ = json.Unmarshal(tp.EnrichmentColumns, &enrich)
	if resp.Template != "saas-ctos" || resp.Name != tp.Name || resp.ColumnsCount != len(fields)+len(enrich) || resp.EnrichmentColumnsCount != len(enrich) {
		t.Fatalf("resp = %+v", resp)
	}
	var srcType, srcCfg, filter, cols string
	err := tenancy.InWorkspace(context.Background(), e.pool, "ws-alpha", func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			"select source_type, source_config::text, filter_criteria::text, columns_config::text from workbooks where id=$1", resp.ID).
			Scan(&srcType, &srcCfg, &filter, &cols)
	})
	if err != nil {
		t.Fatal(err)
	}
	if srcType != "empty" || strings.Contains(srcCfg, "row_storage_version") {
		t.Errorf("source_type=%s source_config=%s (row_storage_version must stay unset)", srcType, srcCfg)
	}
	var gotF, wantF any
	_ = json.Unmarshal([]byte(filter), &gotF)
	_ = json.Unmarshal(tp.Filter, &wantF)
	gb, _ := json.Marshal(gotF)
	wb, _ := json.Marshal(wantF)
	if string(gb) != string(wb) {
		t.Errorf("filter_criteria %s != template filter %s", gb, wb)
	}
	var stored []map[string]any
	_ = json.Unmarshal([]byte(cols), &stored)
	if len(stored) != resp.ColumnsCount || stored[0]["type"] != "lead_field" || stored[0]["width"] != float64(180) {
		t.Errorf("stored columns: %.300s", cols)
	}
	if code, out := e.send("alice", "ws-alpha", "POST", "/api/templates/gallery/create", ""); code != 404 || !strings.Contains(out, "Template not found") {
		t.Errorf("gallery/create: %d %s", code, out)
	}
}

func TestCreateFromTemplateGuards(t *testing.T) {
	bad := func(id, enrich, cols string) func(*wbregistry.Registry) {
		return func(r *wbregistry.Registry) {
			tp := wbregistry.Template{ID: id, Name: "Bad", Description: "d", Category: "sales", Columns: raw(cols), EnrichmentColumns: raw(enrich), Filter: raw(`{}`)}
			r.TemplateSet.Items = append(r.TemplateSet.Items, tp)
		}
	}
	const fields = `[{"key":"company","name":"Company","type":"text"}]`
	cases := []struct{ id, enrich, cols, detail string }{
		{"bad-id", `[{"name":"E","type":"waterfall"}]`, fields, "Template enrichment column IDs must be explicit and unique. Repair the template before creating a workbook."},
		{"bad-dup", `[]`, `[{"key":"a","name":"A","type":"text"},{"key":"a","name":"B","type":"text"}]`, "Template column IDs must be unique"},
		{"bad-cycle", `[{"id":"a","name":"A","type":"ai_formula","prompt":"{b}"},{"id":"b","name":"B","type":"ai_formula","prompt":"{a}"}]`, fields, "Template has circular column dependencies. Repair the template before creating a workbook."},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			e := newTplEnv(t, bad(c.id, c.enrich, c.cols))
			before := e.count(t, "ws-alpha")
			code, out := e.send("alice", "ws-alpha", "POST", "/api/templates/"+c.id+"/create", "")
			var d struct{ Detail string }
			_ = json.Unmarshal([]byte(out), &d)
			if code != 422 || d.Detail != c.detail || e.count(t, "ws-alpha") != before {
				t.Fatalf("%d %s", code, out)
			}
		})
	}
}

func TestEveryShippedRecipeInstantiates(t *testing.T) {
	e := newTplEnv(t, nil)
	reg, _ := wbregistry.Load()
	all, _ := reg.Recipes("")
	if len(all) != 19 {
		t.Fatalf("recipes = %d", len(all))
	}
	for _, rc := range all {
		code, out := e.send("alice", "ws-alpha", "POST", "/api/templates/gallery/"+rc.Slug+"/instantiate", "")
		if code != 201 {
			t.Errorf("%s: %d %.200s", rc.Slug, code, out)
		}
	}
}
