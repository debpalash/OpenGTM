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
	if !strings.HasPrefix(rec.Body.String(), "﻿") || !strings.Contains(rec.Body.String(), "\r\n") {
		t.Error("BOM or CRLF line endings missing")
	}
}
