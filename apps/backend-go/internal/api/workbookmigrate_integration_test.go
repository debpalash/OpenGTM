//go:build integration

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

type migrateEnv struct{ leadsEnv }

func newMigrateEnv(t *testing.T, tune func(*wbcrud.Service)) *migrateEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	st := identity.NewStore(pool)
	prev := tuneMigrateService
	tuneMigrateService = func(s *wbcrud.Service) {
		if tune != nil {
			tune(s)
		}
	}
	t.Cleanup(func() { tuneMigrateService = prev })
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	d := Deps{Pool: pool, Signer: testSigner, Identity: st, Config: config.APIConfig{}, Log: quietLog(),
		Flags: config.Flags{PGLeadStore: true, GoRunRoutesEnabled: true}}
	registerWorkbookMigrate(r, d)
	e := &migrateEnv{leadsEnv{mcpEnv{env: env0(r, st), pool: pool}}}
	e.purge()
	e.seedLeads(t)
	e.seed(t, "erin", pythonHash, true, "user", false)
	e.member(t, "ws-alpha", e.mustUserByName(t, "erin").ID, "editor")
	e.clean()
	t.Cleanup(e.clean)
	t.Cleanup(e.purge)
	e.seedWorkbooks(t)
	return e
}

func (e *migrateEnv) clean() {
	for _, q := range []string{
		`DELETE FROM workbook_enrichments WHERE workspace_id IN ('ws-alpha','ws-beta')`,
		`DELETE FROM workbook_rows WHERE workspace_id IN ('ws-alpha','ws-beta')`,
		`DELETE FROM workbooks WHERE workspace_id IN ('ws-alpha','ws-beta')`,
	} {
		c, err := pgx.Connect(context.Background(), os.Getenv("TEST_DATABASE_URL"))
		if err != nil {
			return
		}
		_, _ = c.Exec(context.Background(), q)
		_ = c.Close(context.Background())
	}
}

const staleStamp = "2020-01-01 00:00:00"

// seedWorkbooks mirrors contract_seed.py: wb-enrich-v1 (Berlin filter, three leads), wb-enrich-1 (manual,
// six stored rows), wb-alpha-1 (manual, no rows, fifteen alpha leads) and a ws-beta workbook.
func (e *migrateEnv) seedWorkbooks(t *testing.T) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "runplan", "testdata", "seed_columns.json"))
	if err != nil {
		t.Fatal(err)
	}
	var seed map[string]json.RawMessage
	if err := json.Unmarshal(raw, &seed); err != nil {
		t.Fatal(err)
	}
	ins := `INSERT INTO workbooks (id, name, description, status, workspace_id, source_type, source_config, filter_criteria,
		columns_config, total_rows, completed_rows, sync_to_leads, budget_max_usd, budget_spent_usd, created_at, updated_at)
		VALUES ($1,$1,'','idle',$2,$3,'{}'::json,$4::json,$5::json,0,0,false,0,0,$6::timestamp,$6::timestamp)`
	ownerExec(t, ins, "wb-enrich-v1", "ws-alpha", "leads_filter", `{"city":"Berlin"}`, string(seed["v1_columns"]), staleStamp)
	ownerExec(t, ins, "wb-enrich-1", "ws-alpha", "manual", "{}", string(seed["columns"]), staleStamp)
	ownerExec(t, ins, "wb-alpha-1", "ws-alpha", "manual", "{}", `[{"id":"c1","name":"Company","type":"text","lead_field":"company"}]`, staleStamp)
	ownerExec(t, ins, "wb-beta-1", "ws-beta", "manual", "{}", "[]", staleStamp)
	// Recorded row ids 101..106 stay below testpg.SeqFloor, where no sequence returns: migrated rows take sequence ids
	// and never collide, so the sequence is never moved. FreeIDs removes a row another test left on one of the ids.
	testpg.FreeIDs(t, "workbook_rows", testpg.Span(101, 106)...)
	for k := 0; k < 6; k++ {
		ownerExec(t, `INSERT INTO workbook_rows (id, workbook_id, "position", data, enrichments, workspace_id) VALUES ($1,'wb-enrich-1',$2,'{"company":"x"}'::json,'{}'::json,'ws-alpha')`, 101+k, k)
	}
}

func migActor(name string) (who, ws string) {
	switch name {
	case "migrate_no_token":
		return "", ""
	case "migrate_viewer":
		return "bob", "ws-alpha"
	case "migrate_foreign":
		return "bob", "ws-beta"
	case "migrate_v2_already":
		return "erin", "ws-alpha"
	}
	return "alice", "ws-alpha"
}

func (e *migrateEnv) migrate(who, ws, wb string) (int, string) {
	hdr := map[string]string{}
	if who != "" {
		hdr = bearer(who)
	}
	if ws != "" {
		hdr["X-Workspace-Id"] = ws
	}
	rec, _ := e.do("POST", "/api/workbooks/"+wb+"/migrate", "", "", hdr)
	return rec.Code, rec.Body.String()
}

func dbString(t *testing.T, sql string, args ...any) string {
	t.Helper()
	c, err := pgx.Connect(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	var s *string
	if err := c.QueryRow(context.Background(), sql, args...).Scan(&s); err != nil {
		t.Fatal(err)
	}
	if s == nil {
		return "<null>"
	}
	return *s
}

func migJSONKeys(t *testing.T, raw string) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(raw))
	if _, err := dec.Token(); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for dec.More() {
		k, _ := dec.Token()
		keys = append(keys, k.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

func TestMigrateMatchesFixtures(t *testing.T) {
	e := newMigrateEnv(t, nil)
	names := contractFixtureNames(t, "migrate")
	if len(names) != 8 {
		t.Fatalf("expected 8 migrate fixtures, found %d", len(names))
	}
	// Fixtures were recorded in this order: fresh must run before again.
	order := []string{"migrate_viewer", "migrate_foreign", "migrate_unknown", "migrate_no_token", "migrate_fresh", "migrate_again", "migrate_v2_already", "migrate_manual_empty"}
	for _, name := range order {
		fx := contract.Load(t, "migrate/"+name)
		who, ws := migActor(name)
		t.Run(name, func(t *testing.T) {
			hdr := map[string]string{}
			if who != "" {
				hdr = bearer(who)
			}
			if ws != "" {
				hdr["X-Workspace-Id"] = ws
			}
			_, g := e.do("POST", fx.Request.Path, "", "", hdr)
			contract.AssertEqual(t, g, fx.Response)
		})
	}

	// Row data: Python's trim keeps lead key order and the essentials only; positions follow score DESC, id DESC.
	var wantKeys []string
	zero, _ := json.Marshal(leadsread.Lead{})
	for _, k := range migJSONKeys(t, string(zero)) {
		switch k {
		case "id", "company", "website", "email", "phone", "city":
			wantKeys = append(wantKeys, k)
		}
	}
	got := dbString(t, `SELECT string_agg(position::text || ':' || lead_id::text, ',' ORDER BY position) FROM workbook_rows WHERE workbook_id='wb-enrich-v1'`)
	if parts := strings.Split(got, ","); len(parts) != 3 || !strings.HasPrefix(parts[0], "0:") || !strings.HasPrefix(parts[1], "1:") || !strings.HasPrefix(parts[2], "2:") {
		t.Fatalf("want 3 rows at positions 0..2, got %q", got)
	}
	for pos := 0; pos < 3; pos++ {
		data := dbString(t, `SELECT data::text FROM workbook_rows WHERE workbook_id='wb-enrich-v1' AND position=$1`, pos)
		if g := migJSONKeys(t, data); strings.Join(g, ",") != strings.Join(wantKeys, ",") {
			t.Fatalf("row %d data keys %v, want %v", pos, g, wantKeys)
		}
	}
	if sc := dbString(t, `SELECT source_config::text FROM workbooks WHERE id='wb-enrich-v1'`); sc != `{"city": "Berlin", "row_storage_version": 2}` &&
		sc != `{"city":"Berlin","row_storage_version":2}` {
		t.Fatalf("source_config %s", sc)
	}
	if st := dbString(t, `SELECT source_type FROM workbooks WHERE id='wb-alpha-1'`); st != "leads_filter" {
		t.Fatalf("manual workbook source_type %q, want leads_filter", st)
	}
	// updated_at is stamped when migrated, and the already-migrated no-op path leaves it alone for a marked workbook.
	if s := dbString(t, `SELECT (updated_at > '2021-01-01')::text FROM workbooks WHERE id='wb-enrich-v1'`); s != "true" {
		t.Fatal("updated_at not stamped by migrate")
	}
}

func TestMigrateAlreadyMigratedIsIdempotent(t *testing.T) {
	e := newMigrateEnv(t, nil)
	code, body := e.migrate("alice", "ws-alpha", "wb-enrich-v1")
	if code != 200 || !strings.Contains(body, `"migrated"`) {
		t.Fatalf("%d %s", code, body)
	}
	stamp := dbString(t, `SELECT updated_at::text FROM workbooks WHERE id='wb-enrich-v1'`)
	rows := dbString(t, `SELECT string_agg(id::text, ',' ORDER BY id) FROM workbook_rows WHERE workbook_id='wb-enrich-v1'`)
	for i := 0; i < 2; i++ {
		code, body = e.migrate("alice", "ws-alpha", "wb-enrich-v1")
		if code != 200 || strings.TrimSpace(body) != `{"status":"already_migrated","rows":3}` {
			t.Fatalf("repeat %d: %d %s", i, code, body)
		}
	}
	if s := dbString(t, `SELECT updated_at::text FROM workbooks WHERE id='wb-enrich-v1'`); s != stamp {
		t.Fatalf("an already-marked workbook was re-stamped: %s -> %s", stamp, s)
	}
	if r := dbString(t, `SELECT string_agg(id::text, ',' ORDER BY id) FROM workbook_rows WHERE workbook_id='wb-enrich-v1'`); r != rows {
		t.Fatalf("rows changed: %s -> %s", rows, r)
	}
	// A workbook that already has rows but no marker is marked (and stamped) exactly once.
	code, body = e.migrate("alice", "ws-alpha", "wb-enrich-1")
	if code != 200 || strings.TrimSpace(body) != `{"status":"already_migrated","rows":6}` {
		t.Fatalf("%d %s", code, body)
	}
	stamp = dbString(t, `SELECT updated_at::text FROM workbooks WHERE id='wb-enrich-1'`)
	if stamp == staleStamp || strings.HasPrefix(stamp, "2020") {
		t.Fatalf("first mark did not stamp: %s", stamp)
	}
	e.migrate("alice", "ws-alpha", "wb-enrich-1")
	if s := dbString(t, `SELECT updated_at::text FROM workbooks WHERE id='wb-enrich-1'`); s != stamp {
		t.Fatalf("second mark re-stamped: %s -> %s", stamp, s)
	}
}

type failingLeads struct {
	wbcrud.LeadReader
	calls  atomic.Int32
	failAt int32
	maxLen atomic.Int32
	sizes  atomic.Int32
}

func (f *failingLeads) QueryLeads(ctx context.Context, tx pgx.Tx, flt leadsread.Filter, page, size int) ([]leadsread.Lead, int, error) {
	n := f.calls.Add(1)
	if f.failAt > 0 && n == f.failAt {
		return nil, 0, errors.New("boom")
	}
	if int32(size) > f.sizes.Load() {
		f.sizes.Store(int32(size))
	}
	ls, total, err := f.LeadReader.QueryLeads(ctx, tx, flt, page, size)
	if int32(len(ls)) > f.maxLen.Load() {
		f.maxLen.Store(int32(len(ls)))
	}
	return ls, total, err
}

func TestMigrateAtomicOnFailure(t *testing.T) {
	fl := &failingLeads{LeadReader: wbcrud.PGLeads{}, failAt: 3}
	e := newMigrateEnv(t, func(s *wbcrud.Service) { s.Leads, s.MigratePageSize = fl, 2 })
	ownerExec(t, `INSERT INTO workbook_enrichments (workbook_id, lead_id, column_id, value, status, workspace_id) VALUES ('wb-alpha-1', 1, 'c1', 'v', 'complete', 'ws-alpha')`)
	code, body := e.migrate("alice", "ws-alpha", "wb-alpha-1")
	if code != 500 || strings.Contains(body, "boom") {
		t.Fatalf("failure must be a clean 500 without the cause: %d %s", code, body)
	}
	if n := dbString(t, `SELECT count(*)::text FROM workbook_rows WHERE workbook_id='wb-alpha-1'`); n != "0" {
		t.Fatalf("partial snapshot left behind: %s rows", n)
	}
	if st := dbString(t, `SELECT source_type || source_config::text FROM workbooks WHERE id='wb-alpha-1'`); !strings.HasPrefix(st, "manual") || strings.Contains(st, "row_storage_version") {
		t.Fatalf("workbook marked after a failed migration: %s", st)
	}
	if s := dbString(t, `SELECT updated_at::text FROM workbooks WHERE id='wb-alpha-1'`); !strings.HasPrefix(s, "2020") {
		t.Fatalf("updated_at stamped by a failed migration: %s", s)
	}
	// The retry succeeds once the reader recovers, and migrates everything.
	fl.failAt = 0
	code, body = e.migrate("alice", "ws-alpha", "wb-alpha-1")
	if code != 200 || strings.TrimSpace(body) != `{"status":"migrated","rows":15,"enrichments_migrated":1}` {
		t.Fatalf("retry: %d %s", code, body)
	}
}

func TestMigrateCopiesEnrichmentOverlay(t *testing.T) {
	e := newMigrateEnv(t, nil)
	ins := `INSERT INTO workbook_enrichments (workbook_id, lead_id, column_id, value, status, provider, error, workspace_id) VALUES ('wb-enrich-v1',$1,$2,$3,$4,$5,$6,'ws-alpha')`
	ownerExec(t, ins, 2, "calc_len", "6", "complete", "formula", nil)
	ownerExec(t, ins, 2, "x", nil, nil, nil, "bad")
	ownerExec(t, ins, 9999, "c", "z", "complete", "p", nil) // not in the snapshot, still counted
	code, body := e.migrate("alice", "ws-alpha", "wb-enrich-v1")
	if code != 200 || strings.TrimSpace(body) != `{"status":"migrated","rows":3,"enrichments_migrated":3}` {
		t.Fatalf("%d %s", code, body)
	}
	got := dbString(t, `SELECT enrichments::text FROM workbook_rows WHERE workbook_id='wb-enrich-v1' AND lead_id=2`)
	want := `{"calc_len":{"value":"6","status":"complete","provider":"formula","error":null},"x":{"value":null,"status":"pending","provider":null,"error":"bad"}}`
	var b bytes.Buffer
	if err := json.Compact(&b, []byte(got)); err != nil || b.String() != want {
		t.Fatalf("overlay\n got %s\nwant %s", b.String(), want)
	}
}

func TestMigrateTenantIsolation(t *testing.T) {
	e := newMigrateEnv(t, nil)
	// Another workspace's workbook answers 404 and nothing is written.
	if code, body := e.migrate("bob", "ws-beta", "wb-enrich-v1"); code != 404 || !strings.Contains(body, "Workbook not found") {
		t.Fatalf("%d %s", code, body)
	}
	if n := dbString(t, `SELECT count(*)::text FROM workbook_rows WHERE workbook_id='wb-enrich-v1'`); n != "0" {
		t.Fatalf("foreign migrate wrote %s rows", n)
	}
	// A beta workbook snapshots only beta leads, never alpha's.
	code, body := e.migrate("bob", "ws-beta", "wb-beta-1")
	if code != 200 || strings.TrimSpace(body) != `{"status":"migrated","rows":3,"enrichments_migrated":0}` {
		t.Fatalf("%d %s", code, body)
	}
	if n := dbString(t, `SELECT count(*)::text FROM workbook_rows WHERE workbook_id='wb-beta-1' AND workspace_id='ws-beta' AND lead_id IN (SELECT id FROM leads WHERE workspace_id='ws-beta')`); n != "3" {
		t.Fatalf("beta rows %s", n)
	}
	// A viewer is refused before any work.
	if code, body := e.migrate("bob", "ws-alpha", "wb-alpha-1"); code != 403 {
		t.Fatalf("%d %s", code, body)
	}
	if n := dbString(t, `SELECT count(*)::text FROM workbook_rows WHERE workbook_id='wb-alpha-1'`); n != "0" {
		t.Fatal("viewer migrate wrote rows")
	}
}

func TestMigrateLargeWorkbookBatches(t *testing.T) {
	fl := &failingLeads{LeadReader: wbcrud.PGLeads{}}
	e := newMigrateEnv(t, func(s *wbcrud.Service) { s.Leads = fl })
	leadBase := testpg.ReserveIDs(t, "leads", 10000)
	ownerExec(t, `INSERT INTO leads (id, workspace_id, company, source, score, status, created_at, updated_at)
		SELECT $1::bigint + g, 'ws-alpha', 'Bulk ' || g, 'mig-large', g % 100, 'new', now(), now() FROM generate_series(1, 10000) g`, leadBase)
	ownerExec(t, `UPDATE workbooks SET filter_criteria='{"source":"mig-large"}'::json WHERE id='wb-alpha-1'`)
	code, body := e.migrate("alice", "ws-alpha", "wb-alpha-1")
	if code != 200 || strings.TrimSpace(body) != `{"status":"migrated","rows":10000,"enrichments_migrated":0}` {
		t.Fatalf("%d %s", code, body)
	}
	// Bounded memory: the lead reader is only ever asked for pages of at most 500, and every row landed once.
	if fl.sizes.Load() != 500 || fl.maxLen.Load() > 500 {
		t.Fatalf("page size %d, longest page %d; want pages of 500", fl.sizes.Load(), fl.maxLen.Load())
	}
	if fl.calls.Load() < 20 {
		t.Fatalf("only %d lead pages for 10000 rows", fl.calls.Load())
	}
	if s := dbString(t, `SELECT count(*)::text || '/' || count(DISTINCT position)::text || '/' || min(position)::text || '/' || max(position)::text FROM workbook_rows WHERE workbook_id='wb-alpha-1'`); s != "10000/10000/0/9999" {
		t.Fatalf("rows/distinct positions/min/max = %s", s)
	}
	// score DESC, id DESC: the first row is the highest score with the highest id.
	if s := dbString(t, `SELECT lead_id::text FROM workbook_rows WHERE workbook_id='wb-alpha-1' AND position=0`); s != fmt.Sprint(leadBase+9999) {
		t.Fatalf("first row lead %s", s)
	}
}

func TestMigrateRowCap(t *testing.T) {
	e := newMigrateEnv(t, func(s *wbcrud.Service) { s.MaxMigrateRows = 10 })
	code, body := e.migrate("alice", "ws-alpha", "wb-alpha-1") // 15 leads
	if code != 413 || !strings.Contains(body, "10") {
		t.Fatalf("%d %s", code, body)
	}
	if n := dbString(t, `SELECT count(*)::text FROM workbook_rows WHERE workbook_id='wb-alpha-1'`); n != "0" {
		t.Fatalf("capped migrate wrote %s rows", n)
	}
}

func TestMigrateRowCapAllowsExactlyTheCap(t *testing.T) {
	e := newMigrateEnv(t, func(s *wbcrud.Service) { s.MaxMigrateRows = 15 })
	if code, body := e.migrate("alice", "ws-alpha", "wb-alpha-1"); code != 200 {
		t.Fatalf("at cap: %d %s", code, body)
	}
}
