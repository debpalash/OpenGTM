//go:build integration

package ambitionimport_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ambitionbox"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ambitionimport"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/automationhook"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/sourceingest"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

const (
	wsA = "ws-vec-ab"
	wsB = "ws-vec-ab-b"
)

var t0 = time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)

// fakeFetch scripts AmbitionBox pages (the Python generator monkeypatches
// ambitionbox.fetch_company_page the same way).
type fakeFetch struct {
	mu     sync.Mutex
	pages  map[int]sourceingest.Page
	fail   map[int]string // page -> error text, fires once
	before map[int]func() // page -> hook run once before the page is returned
	fired  map[string]bool
	calls  []ambitionbox.PageParams
	all    func(page int) // called on every fetch (barriers)
}

func (f *fakeFetch) FetchCompanyPage(ctx context.Context, p ambitionbox.PageParams) (sourceingest.Page, error) {
	f.mu.Lock()
	f.calls = append(f.calls, p)
	all := f.all
	var pre func()
	if h, ok := f.before[p.Page]; ok && !f.fired[fmt.Sprint("b", p.Page)] {
		f.fired[fmt.Sprint("b", p.Page)] = true
		pre = h
	}
	msg, failing := f.fail[p.Page]
	if failing && f.fired[fmt.Sprint("f", p.Page)] {
		failing = false
	}
	if failing {
		f.fired[fmt.Sprint("f", p.Page)] = true
	}
	pg, ok := f.pages[p.Page]
	f.mu.Unlock()
	if all != nil {
		all(p.Page)
	}
	if pre != nil {
		pre()
	}
	if failing {
		return sourceingest.Page{}, errors.New(msg)
	}
	if !ok {
		return sourceingest.Page{}, fmt.Errorf("fakeFetch: no page %d scripted", p.Page)
	}
	return pg, nil
}

func (f *fakeFetch) callPages() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []int
	for _, c := range f.calls {
		out = append(out, c.Page)
	}
	return out
}

func company(i int) *wbcrud.Obj {
	o := wbcrud.NewObj()
	o.Set("company_id", i)
	o.Set("name", fmt.Sprintf("Co %d", i))
	o.Set("industry", "IT")
	o.Set("rating", 3.5)
	o.Set("review_count", 10+i)
	o.Set("profile_url", fmt.Sprintf("https://ab/%d", i))
	return o
}

func ids(a, b int) []int {
	var out []int
	for i := a; i <= b; i++ {
		out = append(out, i)
	}
	return out
}

func ip(i int) *int { return &i }

// pageOf is the generator's page_of: ConnectorPage.from_records, optional has_more override.
func pageOf(page int, idl []int, totalPages, sourceTotal *int, hasMore *bool) sourceingest.Page {
	recs := make([]sourceingest.Record, len(idl))
	for k, i := range idl {
		u := fmt.Sprintf("https://ab/%d", i)
		recs[k] = sourceingest.Record{Provider: "ambitionbox", RecordID: fmt.Sprint(i), Data: company(i),
			Rank: (page-1)*20 + k, SourceURL: &u, FetchedAt: t0}
	}
	pg := sourceingest.NewPage("ambitionbox", recs, page, 20, sourceTotal, totalPages)
	if hasMore != nil {
		pg.HasMore = *hasMore
	}
	return pg
}

type capPub struct {
	mu   sync.Mutex
	msgs []map[string]any
}

func (p *capPub) Publish(_ context.Context, _ string, msg map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := map[string]any{}
	for k, v := range msg {
		cp[k] = v
	}
	p.msgs = append(p.msgs, cp)
}

type capHook struct {
	mu      sync.Mutex
	emits   [][]int64
	failOn  int // 1-based call number that fails once (0 = never)
	calls   int
	inTxSee func(ctx context.Context, tx pgx.Tx, ids []int64) error
}

func (h *capHook) RowsAdded(ctx context.Context, tx pgx.Tx, ws, wb string, rowIDs []int64) (int, error) {
	h.mu.Lock()
	h.calls++
	n := h.calls
	h.mu.Unlock()
	if h.inTxSee != nil {
		if err := h.inTxSee(ctx, tx, rowIDs); err != nil {
			return 0, err
		}
	}
	if h.failOn == n {
		return 0, errors.New("hook boom")
	}
	h.mu.Lock()
	h.emits = append(h.emits, append([]int64(nil), rowIDs...))
	h.mu.Unlock()
	return len(rowIDs), nil
}

func (h *capHook) SignalsInserted(context.Context, pgx.Tx, string, []automationhook.SignalRef) (int, error) {
	return 0, nil
}

type env struct {
	t     *testing.T
	pool  *pgxpool.Pool
	owner *pgxpool.Pool
	ctx   context.Context
	fetch *fakeFetch
	pub   *capPub
	hook  *capHook
	d     ambitionimport.Deps
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := testpg.NewAppPool(t, 16)
	ctx := context.Background()
	owner, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	e := &env{t: t, pool: pool, owner: owner, ctx: ctx,
		fetch: &fakeFetch{pages: map[int]sourceingest.Page{}, fail: map[int]string{}, before: map[int]func(){}, fired: map[string]bool{}},
		pub:   &capPub{}, hook: &capHook{}}
	e.d = ambitionimport.Deps{Pool: pool, Fetcher: e.fetch, Hook: e.hook, Progress: e.pub,
		Executor: func(string) string { return "go" }}
	clean := func() {
		for _, w := range []string{wsA, wsB} {
			_ = tenancy.InWorkspace(ctx, pool, w, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `DELETE FROM workbooks`)
				return err
			})
		}
		_, _ = owner.Exec(ctx, `DROP TRIGGER IF EXISTS ai_test_fail_job ON jobs`)
		_, _ = owner.Exec(ctx, `DELETE FROM jobs WHERE type IN ('ambitionbox_import','trigger_eval')`)
	}
	clean()
	t.Cleanup(clean)
	return e
}

func (e *env) in(ws string, fn func(tx pgx.Tx) error) {
	e.t.Helper()
	if err := tenancy.InWorkspace(e.ctx, e.pool, ws, fn); err != nil {
		e.t.Fatal(err)
	}
}

// seedWorkbook inserts a workbook with rows (data, provider, record id).
func (e *env) seedWorkbook(ws, id, name, sourceType string, rows [][3]any) {
	e.t.Helper()
	e.in(ws, func(tx pgx.Tx) error {
		if _, err := tx.Exec(e.ctx, `INSERT INTO workbooks (id, name, workspace_id, source_type, source_config, filter_criteria, columns_config, total_rows, status)
			VALUES ($1,$2,$3,$4,'{}','{}','[]',$5,'draft')`, id, name, ws, sourceType, len(rows)); err != nil {
			return err
		}
		for k, r := range rows {
			data, _ := json.Marshal(r[0])
			var prov, rid any
			if r[1] != nil {
				prov, rid = r[1], r[2]
			}
			if _, err := tx.Exec(e.ctx, `INSERT INTO workbook_rows (workspace_id, workbook_id, position, data, enrichments, source_provider, source_record_id)
				VALUES ($1,$2,$3,$4::json,'{}'::json,$5,$6)`, ws, id, k, string(data), prov, rid); err != nil {
				return err
			}
		}
		return nil
	})
}

// claimFor inserts a processing job row and returns the matching lease.
func (e *env) claimFor(ws, wbID, runID string) *queue.Claim {
	e.t.Helper()
	payload := fmt.Sprintf(`{"workspace_id": %q, "workbook_id": %q, "run_id": %q}`, ws, wbID, runID)
	return e.claimPayload(ws, payload)
}

func (e *env) claimPayload(ws, payload string) *queue.Claim {
	e.t.Helper()
	var id int32
	var locked time.Time
	err := e.owner.QueryRow(e.ctx, `INSERT INTO jobs (executor, type, payload, workspace_id, priority, status, created_at, next_run_at,
		max_retries, retry_count, worker_id, locked_at, started_at, last_heartbeat, lease_token)
		VALUES ('go','ambitionbox_import',$1::json,$2,2,'processing',(now() AT TIME ZONE 'utc'),(now() AT TIME ZONE 'utc'),3,0,'w-test',
		(now() AT TIME ZONE 'utc'),(now() AT TIME ZONE 'utc'),(now() AT TIME ZONE 'utc'),7) RETURNING id, locked_at`, payload, ws).Scan(&id, &locked)
	if err != nil {
		e.t.Fatal(err)
	}
	return &queue.Claim{ID: int64(id), Type: "ambitionbox_import", WorkspaceID: ws, Payload: json.RawMessage(payload),
		WorkerID: "w-test", LockedAt: locked, LeaseToken: 7}
}

func (e *env) queryJSON(ws, sql string, args ...any) any {
	e.t.Helper()
	var raw []byte
	e.in(ws, func(tx pgx.Tx) error { return tx.QueryRow(e.ctx, sql, args...).Scan(&raw) })
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		e.t.Fatalf("decode %s: %v", raw, err)
	}
	return v
}

// runView is the vector's run_view: no id/workbook_id, timestamps as booleans.
func (e *env) runView(ws, runID string) map[string]any {
	m := e.queryJSON(ws, `SELECT to_jsonb(r) - 'id' - 'workbook_id' FROM connector_runs r WHERE id=$1`, runID).(map[string]any)
	for _, k := range []string{"started_at", "completed_at", "created_at", "updated_at"} {
		m[k] = m[k] != nil
	}
	return m
}

func (e *env) wbView(ws, wbID, runID string) (map[string]any, any) {
	w := e.queryJSON(ws, `SELECT jsonb_build_object('name',name,'status',status,'source_type',source_type,'total_rows',total_rows,
		'source_config',source_config::jsonb,
		'columns_config_ids',(SELECT coalesce(jsonb_agg(c->>'id' ORDER BY ord),'[]'::jsonb) FROM json_array_elements(columns_config) WITH ORDINALITY t(c,ord)))
		FROM workbooks WHERE id=$1`, wbID).(map[string]any)
	if sc, ok := w["source_config"].(map[string]any); ok {
		if _, has := sc["active_run_id"]; has {
			sc["active_run_id"] = "<run_id>"
		}
	}
	rows := e.queryJSON(ws, `SELECT coalesce(jsonb_agg(jsonb_build_object('position',position,'source_provider',source_provider,
		'source_record_id',source_record_id,'source_rank',source_rank,'data',data::jsonb) ORDER BY position,id),'[]'::jsonb)
		FROM workbook_rows WHERE workbook_id=$1`, wbID)
	return w, rows
}

func norm(v any) any {
	b, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}

func assertEq(t *testing.T, what string, got, want any) {
	t.Helper()
	g, w := norm(got), norm(want)
	if !reflect.DeepEqual(g, w) {
		gb, _ := json.Marshal(g)
		wb, _ := json.Marshal(w)
		t.Errorf("%s mismatch\n got: %.1500s\nwant: %.1500s", what, gb, wb)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
