//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
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

type detailEnv struct{ mcpEnv }

func newDetailEnv(t *testing.T) detailEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	d := Deps{Pool: pool, Signer: testSigner, Identity: st, Config: config.APIConfig{}, Flags: config.Flags{PGLeadStore: true}}
	RegisterWorkbooks(r, d)
	RegisterWorkbookDetail(r, d)
	e := detailEnv{mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool}}
	e.purge()
	t.Cleanup(e.purge)
	return e
}

func (e detailEnv) purge() {
	ctx := context.Background()
	for _, ws := range []string{"ws-alpha", "ws-beta"} {
		_ = tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
			for _, tbl := range []string{"connector_runs", "workbook_enrichments", "workbook_views", "workbook_rows", "workbooks", "leads"} {
				if _, err := tx.Exec(ctx, "DELETE FROM "+tbl); err != nil {
					return err
				}
			}
			return nil
		})
	}
}

// seedDetail loads users, workspaces and the dump of the FastAPI contract database that the fixtures were recorded
// against (legacy/python/scripts/dump_contract_seed.py after contract_seed.py + contract_seed_detail.py).
func (e detailEnv) seedDetail(t *testing.T) {
	t.Helper()
	for _, u := range []struct {
		name  string
		admin bool
	}{{"alice", true}, {"bob", false}} {
		role := "user"
		if u.admin {
			role = "admin"
		}
		e.seed(t, u.name, pythonHash, true, role, u.admin)
	}
	alice, bob := e.mustUserByName(t, "alice"), e.mustUserByName(t, "bob")
	e.workspace(t, "ws-alpha", alice.ID)
	e.workspace(t, "ws-beta", bob.ID)
	e.member(t, "ws-alpha", alice.ID, "owner")
	e.member(t, "ws-alpha", bob.ID, "member")
	e.member(t, "ws-beta", bob.ID, "owner")
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "wbcrud", "detail_seed.json"))
	if err != nil {
		t.Fatal(err)
	}
	var seed map[string]map[string]struct {
		Columns []string        `json:"columns"`
		Rows    json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(raw, &seed); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for ws, tables := range seed {
		err := tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
			for _, tbl := range []string{"leads", "workbooks", "workbook_rows", "workbook_views", "workbook_enrichments", "connector_runs"} {
				d := tables[tbl]
				cols := strings.Join(d.Columns, ", ")
				freeSeedIDs(t, tbl, d.Columns, d.Rows)
				if _, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %[1]s (%[2]s) SELECT %[2]s FROM json_populate_recordset(null::%[1]s, $1::json)`, tbl, cols), string(d.Rows)); err != nil {
					return fmt.Errorf("%s %s: %w", ws, tbl, err)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// actor says who sent a recorded request (fixtures keep Authorization and X-Workspace-Id redacted).
func detailActor(name string) (who, ws string) {
	switch {
	case strings.HasSuffix(name, "_no_token"):
		return "", ""
	case name == "detail_beta_member_alpha_ws", name == "runs_beta_member":
		return "bob", "ws-beta"
	case strings.HasSuffix(name, "_as_viewer"):
		return "bob", "ws-alpha"
	}
	return "alice", "ws-alpha"
}

func (e detailEnv) get(who, ws, path string) *httptest.ResponseRecorder {
	hdr := map[string]string{}
	if who != "" {
		hdr = bearer(who)
		hdr["X-Workspace-Id"] = ws
	}
	rec, _ := e.do("GET", path, "", "", hdr)
	return rec
}

// replay sends the recorded request of detail/<name> and compares status, headers and body. CSV bodies (recorded as a
// JSON string) are compared byte for byte.
func (e detailEnv) replay(t *testing.T, name string, opts ...contract.Option) {
	t.Helper()
	fx := contract.Load(t, "detail/"+name)
	who, ws := detailActor(name)
	rec := e.get(who, ws, fx.Request.Path)
	if strings.HasPrefix(fx.Response.Headers["content-type"], "text/csv") {
		var want string
		if err := json.Unmarshal(fx.Response.Body, &want); err != nil && fx.Response.Status == 200 {
			t.Fatalf("%s: csv fixture body: %v", name, err)
		}
		if rec.Code != fx.Response.Status {
			t.Fatalf("%s: status %d, want %d: %s", name, rec.Code, fx.Response.Status, rec.Body.String())
		}
		if got := rec.Body.String(); got != want {
			t.Errorf("%s: csv bytes differ\n got %q\nwant %q", name, got, want)
		}
		for _, h := range []string{"Content-Type", "Content-Disposition"} {
			if w, ok := fx.Response.Headers[strings.ToLower(h)]; ok && rec.Header().Get(h) != w {
				t.Errorf("%s: %s = %q, want %q", name, h, rec.Header().Get(h), w)
			}
		}
		return
	}
	got := contract.Resp{Status: rec.Code, Headers: map[string]string{}, Body: json.RawMessage(rec.Body.Bytes())}
	for _, k := range []string{"Content-Type", "WWW-Authenticate", "Allow"} {
		if v := rec.Header().Get(k); v != "" {
			got.Headers[k] = v
		}
	}
	t.Run(name, func(t *testing.T) { contract.AssertEqual(t, got, fx.Response, opts...) })
}

func detailFixtureNames(t *testing.T, prefixes ...string) []string {
	t.Helper()
	var out []string
	for _, n := range contractFixtureNames(t, "detail") {
		n = strings.TrimPrefix(n, "detail/")
		for _, p := range prefixes {
			if strings.HasPrefix(n, p) {
				out = append(out, n)
				break
			}
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		t.Fatalf("no fixtures for %v", prefixes)
	}
	return out
}

func (e detailEnv) replayAll(t *testing.T, names []string) {
	t.Helper()
	for _, n := range names {
		e.replay(t, n)
	}
}

func TestDetailOffsetPageMatchesFixture(t *testing.T) {
	e := newDetailEnv(t)
	e.seedDetail(t)
	e.replayAll(t, []string{"detail_page1", "detail_page2_size10", "detail_page3_size10", "detail_page_beyond", "detail_huge_page",
		"detail_empty_workbook", "detail_as_viewer", "detail_wb3", "detail_wb3_cursor", "detail_search_blank", "detail_empty_view_id"})
}

func TestDetailCursorFixtures(t *testing.T) {
	e := newDetailEnv(t)
	e.seedDetail(t)
	e.replayAll(t, detailFixtureNames(t, "detail_cm_", "detail_cv_", "detail_cs_", "detail_cursor", "detail_cursor_"))
}

func TestDetailViewAndSearchApplied(t *testing.T) {
	e := newDetailEnv(t)
	e.seedDetail(t)
	e.replayAll(t, []string{"detail_view_filter", "detail_view_sort", "detail_view_sort_city", "detail_view_sort_enrichment",
		"detail_search", "detail_search_enrichment", "detail_search_percent", "detail_wb3_search_unicode",
		"detail_unknown_view", "detail_view_other_workbook"})
}

func TestDetailPageSizeBounds422(t *testing.T) {
	e := newDetailEnv(t)
	e.seedDetail(t)
	e.replayAll(t, detailFixtureNames(t, "detail_422_"))
}

func TestDetailLegacyLeadsFallback(t *testing.T) {
	e := newDetailEnv(t)
	e.seedDetail(t)
	e.replayAll(t, detailFixtureNames(t, "detail_legacy"))
}

func TestDetailOtherWorkspace404(t *testing.T) {
	e := newDetailEnv(t)
	e.seedDetail(t)
	e.replayAll(t, []string{"detail_unknown", "detail_foreign", "detail_beta_member_alpha_ws", "detail_no_token",
		"export_unknown", "export_foreign", "export_no_token", "runs_unknown", "runs_foreign", "runs_no_token"})
	// A member of ws-beta asking for an alpha workbook, and an alpha member naming beta's workspace, never see 403.
	if rec := e.get("alice", "ws-alpha", "/api/workbooks/wb-beta-1"); rec.Code != 404 {
		t.Fatalf("foreign workbook: %d", rec.Code)
	}
	if rec := e.get("bob", "ws-beta", "/api/workbooks/wb-alpha-2/export.csv"); rec.Code != 404 {
		t.Fatalf("foreign export: %d", rec.Code)
	}
	if rec := e.get("bob", "ws-beta", "/api/workbooks/wb-alpha-2/connector-runs"); rec.Code != 404 {
		t.Fatalf("foreign runs: %d", rec.Code)
	}
}

func TestExportCsvMatchesFixtureBytes(t *testing.T) {
	e := newDetailEnv(t)
	e.seedDetail(t)
	e.replayAll(t, detailFixtureNames(t, "export_"))
}

func TestExportSelectedOver1000Is400(t *testing.T) {
	e := newDetailEnv(t)
	e.seedDetail(t)
	e.replayAll(t, []string{"export_row_ids_1000", "export_row_ids_1001", "export_row_ids_1001_unknown_col", "export_row_ids_1001_bad_view"})
}

func TestExportUnknownColumns400(t *testing.T) {
	e := newDetailEnv(t)
	e.seedDetail(t)
	e.replayAll(t, []string{"export_unknown_columns", "export_unknown_columns_12", "export_empty_column_id", "export_row_ids_bad"})
}

func TestConnectorRunsFixtures(t *testing.T) {
	e := newDetailEnv(t)
	e.seedDetail(t)
	e.replayAll(t, detailFixtureNames(t, "runs_"))
}

// Every recorded fixture of the area is replayed by one of the tests above or by this catch-all.
func TestAllDetailFixturesReplayed(t *testing.T) {
	e := newDetailEnv(t)
	e.seedDetail(t)
	for _, n := range contractFixtureNames(t, "detail") {
		e.replay(t, strings.TrimPrefix(n, "detail/"))
	}
}

// A hardening deviation (documented in testdata/contract/README.md): a page number whose offset overflows bigint
// answers an empty page instead of FastAPI's 500.
func TestDetailHugePageIsEmptyNot500(t *testing.T) {
	e := newDetailEnv(t)
	e.seedDetail(t)
	rec := e.get("alice", "ws-alpha", "/api/workbooks/wb-alpha-2?page=9223372036854775807&page_size=5000")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"rows":[]`) || !strings.Contains(rec.Body.String(), `"has_more":false`) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String()[:200])
	}
	rec = e.get("alice", "ws-alpha", "/api/workbooks/wb-alpha-2?page=99999999999999999999")
	if rec.Code != 422 {
		t.Fatalf("page beyond int64: %d", rec.Code)
	}
}

func TestExportHeadersAndStreamingResponse(t *testing.T) {
	e := newDetailEnv(t)
	e.seedDetail(t)
	rec := e.get("alice", "ws-alpha", "/api/workbooks/wb-alpha-3/export.csv")
	if rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/csv; charset=utf-8" {
		t.Errorf("content-type %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != `attachment; filename="Detail_Export.csv"` {
		t.Errorf("content-disposition %q", cd)
	}
	if !strings.HasPrefix(rec.Body.String(), "\ufeff") || !strings.Contains(rec.Body.String(), "\r\n") {
		t.Error("BOM or CRLF line endings missing")
	}
}

// jsonKeys lists the keys of the JSON object at the start of raw, in document order.
func jsonKeys(t *testing.T, raw string) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("not an object: %.80s", raw)
	}
	var keys []string
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

// Fixtures are key-sorted, so Python's response key order (pydantic field order, dict insertion order) is asserted here.
func TestDetailKeyOrderMatchesPydantic(t *testing.T) {
	e := newDetailEnv(t)
	e.seedDetail(t)
	var top struct {
		Rows []json.RawMessage `json:"rows"`
	}
	body := e.get("alice", "ws-alpha", "/api/workbooks/wb-alpha-3?page_size=6&cursor_mode=true").Body.String()
	if err := json.Unmarshal([]byte(body), &top); err != nil {
		t.Fatal(err)
	}
	same := func(what string, got []string, want string) {
		t.Helper()
		if strings.Join(got, ",") != want {
			t.Errorf("%s key order\n got %s\nwant %s", what, strings.Join(got, ","), want)
		}
	}
	same("response", jsonKeys(t, body), "workbook,rows,total_rows,query_total_rows,page,page_size,next_cursor,has_more")
	same("row", jsonKeys(t, string(top.Rows[0])), "lead_id,row_id,position,source_provider,source_record_id,source_rank,source_fetched_at,lead,data,enrichments,canonical_entity_id,corroboration_count")
	var row struct {
		Enrichments map[string]json.RawMessage `json:"enrichments"`
		Data        json.RawMessage            `json:"data"`
	}
	if err := json.Unmarshal(top.Rows[5], &row); err != nil {
		t.Fatal(err)
	}
	same("overlay", jsonKeys(t, string(row.Enrichments["ai"])), "value,status,provider,error,verify_status,provenance,research")
	same("data", jsonKeys(t, string(row.Data)), "company,amount,tags,resume")
	// A stored dict keeps its stored key order (json column), and U+2028 stays raw like Python's ensure_ascii=False.
	if !strings.Contains(body, `"tags":{"k":"v","n":null,"f":1.5}`) {
		t.Errorf("stored key order lost: %.400s", body)
	}
	if !strings.Contains(body, "\u2028") {
		t.Errorf("U+2028 must not be escaped")
	}
	runs := e.get("alice", "ws-alpha", "/api/workbooks/wb-alpha-2/connector-runs?limit=1").Body.String()
	same("runs response", jsonKeys(t, runs), "runs,limit,offset,has_more,next_cursor")
	var rr struct {
		Runs []json.RawMessage `json:"runs"`
	}
	if err := json.Unmarshal([]byte(runs), &rr); err != nil {
		t.Fatal(err)
	}
	same("run", jsonKeys(t, string(rr.Runs[0])), "id,workspace_id,workbook_id,connector,status,query,cursor,requested_count,fetched_count,added_count,updated_count,skipped_count,pages_fetched,source_total,target_met,exhausted,error,started_at,completed_at,created_at,updated_at")
}

// Cross-workspace reads of export and connector runs: 404, nothing streamed (no CSV headers, BOM or rows).
func TestExportAndRunsCrossWorkspace404NothingStreamed(t *testing.T) {
	e := newDetailEnv(t)
	e.seedDetail(t)
	for _, c := range []struct{ name, who, ws, path string }{
		{"export other workspace's workbook", "alice", "ws-alpha", "/api/workbooks/wb-beta-1/export.csv"},
		{"export alpha id from beta", "bob", "ws-beta", "/api/workbooks/wb-alpha-2/export.csv?column_ids=company"},
		{"export with row_ids", "bob", "ws-beta", "/api/workbooks/wb-alpha-3/export.csv?row_ids=101"},
		{"runs other workspace's workbook", "alice", "ws-alpha", "/api/workbooks/wb-beta-1/connector-runs"},
		{"runs alpha id from beta", "bob", "ws-beta", "/api/workbooks/wb-alpha-2/connector-runs?limit=200"},
		// bob owns ws-beta and is a member of ws-alpha: naming the other workspace in the header scopes
		// every lookup to it, so the workbook of the first workspace does not exist there.
		{"export X-Workspace-Id mismatch", "bob", "ws-beta", "/api/workbooks/wb-alpha-2/export.csv"},
		{"runs X-Workspace-Id mismatch", "bob", "ws-alpha", "/api/workbooks/wb-beta-1/connector-runs"},
	} {
		rec := e.get(c.who, c.ws, c.path)
		if rec.Code != 404 {
			t.Errorf("%s: status %d, want 404", c.name, rec.Code)
			continue
		}
		if b := rec.Body.String(); b != `{"detail":"Workbook not found"}` {
			t.Errorf("%s: body %q", c.name, b)
		}
		if rec.Header().Get("Content-Disposition") != "" || strings.HasPrefix(rec.Header().Get("Content-Type"), "text/csv") {
			t.Errorf("%s: CSV headers sent: %v", c.name, rec.Header())
		}
	}
	// A workspace the caller does not belong to is rejected by the gate, never a 200.
	for _, p := range []string{"/api/workbooks/wb-alpha-2/export.csv", "/api/workbooks/wb-alpha-2/connector-runs"} {
		if rec := e.get("alice", "ws-beta", p); rec.Code == 200 || strings.Contains(rec.Body.String(), "Acme") {
			t.Errorf("alice naming ws-beta on %s: %d %.80s", p, rec.Code, rec.Body.String())
		}
	}
}
