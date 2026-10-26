//go:build integration

package wbcrud_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// importHook records RowsAdded calls and runs probe on the very transaction the
// rows were inserted in.
type importHook struct {
	calls [][]int64
	probe func(tx pgx.Tx)
}

func (h *importHook) RowsAdded(_ context.Context, tx pgx.Tx, _, _ string, ids []int64) (int, error) {
	h.calls = append(h.calls, append([]int64(nil), ids...))
	if h.probe != nil {
		h.probe(tx)
	}
	return len(ids), nil
}

type impEnv struct {
	s    *wbcrud.Service
	ctx  context.Context
	t    *testing.T
	hook *importHook
}

func newImpEnv(t *testing.T) impEnv {
	s, ctx := setup(t)
	h := &importHook{}
	s.Hook = h
	return impEnv{s: s, ctx: ctx, t: t, hook: h}
}

func (e impEnv) workbook(ws, columns, srcType, srcCfg string) string {
	e.t.Helper()
	id := insert(e.t, e.s, e.ctx, ws, "imp", srcType, srcCfg, `{}`)
	e.exec(ws, `UPDATE workbooks SET columns_config=$1::json, updated_at='2026-01-01 00:00:00' WHERE id=$2`, columns, id)
	return id
}

func (e impEnv) exec(ws, sql string, args ...any) {
	e.t.Helper()
	if err := e.s.WithTx(e.ctx, ws, func(tx pgx.Tx) error { _, err := tx.Exec(e.ctx, sql, args...); return err }); err != nil {
		e.t.Fatal(err)
	}
}

func (e impEnv) one(ws, sql string, args ...any) (out string) {
	e.t.Helper()
	if err := e.s.WithTx(e.ctx, ws, func(tx pgx.Tx) error { return tx.QueryRow(e.ctx, sql, args...).Scan(&out) }); err != nil {
		e.t.Fatal(err)
	}
	return
}

func (e impEnv) rowData(ws, wb string) []string {
	e.t.Helper()
	var out []string
	err := e.s.WithTx(e.ctx, ws, func(tx pgx.Tx) error {
		rows, err := tx.Query(e.ctx, `SELECT position::text || '|' || data::text FROM workbook_rows WHERE workbook_id=$1 ORDER BY position, id`, wb)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return err
			}
			out = append(out, s)
		}
		return rows.Err()
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e impEnv) state(ws, wb string) string {
	return e.one(ws, `SELECT columns_config::text || '#' || source_type || '#' || source_config::text || '#' || updated_at::text FROM workbooks WHERE id=$1`, wb)
}

// importAs runs Import in a workspace tx, answering the guard or error.
func (e impEnv) importAs(ws, wb string, req wbcrud.ImportRequest, batchHook func(int) error) (*wbcrud.Obj, *wbcrud.GuardError, error) {
	e.t.Helper()
	var (
		res   *wbcrud.Obj
		guard *wbcrud.GuardError
	)
	err := e.s.WithTx(e.ctx, ws, func(tx pgx.Tx) error {
		w, err := e.s.Owned(e.ctx, tx, ws, wb, true)
		if err != nil {
			return err
		}
		im := wbcrud.Importer{Service: e.s, BatchHook: batchHook}
		var g *wbcrud.GuardError
		res, g, err = im.Import(e.ctx, tx, ws, w, &req)
		if err != nil {
			return err
		}
		if g != nil {
			guard = g
			return errors.New("guard") // roll back, like the route
		}
		return nil
	})
	if guard != nil {
		return nil, guard, nil
	}
	return res, nil, err
}

func errString(o *wbcrud.Obj) string {
	b, _ := o.MarshalJSON()
	return string(b)
}

func parseImport(t *testing.T, body string) wbcrud.ImportRequest {
	t.Helper()
	req, errs, err := wbcrud.ReadImportRequest(strings.NewReader(body), 1<<30)
	if err != nil || errs != nil {
		t.Fatalf("parse: %v %v", err, errs)
	}
	return req
}

const colCompany = `{"id":"company","name":"Company","type":"lead_field","width":200,"lead_field":"company"}`

func TestImportCreatesColumnsAndRows(t *testing.T) {
	e := newImpEnv(t)
	wb := e.workbook(wsA, `[`+colCompany+`]`, "empty", `{"keep":1}`)
	res, guard, err := e.importAs(wsA, wb, parseImport(t,
		`{"rows":[{"Company":"Acme","Custom Field":"x","Website":"https://acme.com"},{"Company":"Beta","Custom Field":"y"}],"file_name":"leads.csv"}`), nil)
	if guard != nil || err != nil {
		t.Fatalf("guard=%v err=%v", guard, err)
	}
	if got := errString(res); !strings.HasPrefix(got, `{"created":2,"added":2,"skipped_duplicates":0,"total_rows":2,"columns_added":["Custom Field","Website"],"mapping":{"Company":"company","Custom Field":"custom_field","Website":"website"},"analysis":{"source_system":"generic"`) {
		t.Errorf("response: %s", got)
	}
	rows := e.rowData(wsA, wb)
	if len(rows) != 2 || rows[0] != `0|{"company": "Acme", "custom_field": "x", "website": "https://acme.com"}` ||
		rows[1] != `1|{"company": "Beta", "custom_field": "y"}` {
		t.Errorf("rows: %q", rows)
	}
	cols := e.one(wsA, `SELECT columns_config::text FROM workbooks WHERE id=$1`, wb)
	if !strings.Contains(cols, `{"id":"custom_field","name":"Custom Field","type":"lead_field","lead_field":"custom_field","width":200}`) ||
		!strings.Contains(cols, `"id":"website","name":"Website"`) {
		t.Errorf("columns: %s", cols)
	}
	if st := e.one(wsA, `SELECT source_type FROM workbooks WHERE id=$1`, wb); st != "csv" {
		t.Errorf("source_type %q", st)
	}
	cfg := e.one(wsA, `SELECT source_config::text FROM workbooks WHERE id=$1`, wb)
	if !strings.HasPrefix(cfg, `{"keep":1,"last_csv_import":{"file_name":"leads.csv","source_system":"generic","mapping":{`) ||
		!strings.Contains(cfg, `"collisions":[],"rows":2,"columns_added":2,"imported_at":"`) || !strings.HasSuffix(cfg, `+00:00"}}`) {
		t.Errorf("source_config: %s", cfg)
	}
	// Review focus 1: updated_at advanced (the sidebar orders by it).
	if adv := e.one(wsA, `SELECT (updated_at > '2026-01-01 00:00:00')::text FROM workbooks WHERE id=$1`, wb); adv != "true" {
		t.Error("updated_at was not stamped")
	}
}

func TestImportAppendsAfterMaxPosition(t *testing.T) {
	e := newImpEnv(t)
	wb := e.workbook(wsA, `[`+colCompany+`]`, "manual", `{}`)
	e.exec(wsA, `INSERT INTO workbook_rows (workspace_id, workbook_id, position, data, enrichments) VALUES ($1,$2,5,'{"company":"Old"}','{}'),($1,$2,9,'{"company":"Older"}','{}')`, wsA, wb)
	if _, g, err := e.importAs(wsA, wb, parseImport(t, `{"rows":[{"Company":"New"}]}`), nil); g != nil || err != nil {
		t.Fatalf("%v %v", g, err)
	}
	rows := e.rowData(wsA, wb)
	if len(rows) != 3 || rows[2] != `10|{"company": "New"}` {
		t.Errorf("rows: %q", rows)
	}
	if st := e.one(wsA, `SELECT source_type FROM workbooks WHERE id=$1`, wb); st != "manual" {
		t.Errorf("source_type %q: only 'empty' becomes csv", st)
	}
}

func TestImportMappingOverridesInference(t *testing.T) {
	e := newImpEnv(t)
	wb := e.workbook(wsA, `[`+colCompany+`]`, "empty", `{}`)
	res, _, err := e.importAs(wsA, wb, parseImport(t,
		`{"rows":[{"Email":"a@b.co","Org":"Acme","Skip me":"zzz"}],"mapping":{"Email":"phone","Org":"Company","Skip me":null}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := errString(res); !strings.Contains(got, `"mapping":{"Email":"phone","Org":"company","Skip me":null}`) {
		t.Errorf("mapping: %s", got)
	}
	if rows := e.rowData(wsA, wb); len(rows) != 1 || rows[0] != `0|{"phone": "a@b.co", "company": "Acme"}` {
		t.Errorf("rows: %q", rows)
	}
}

func TestImportDedupeWithinFileAndAgainstWorkbook(t *testing.T) {
	e := newImpEnv(t)
	wb := e.workbook(wsA, `[`+colCompany+`]`, "manual", `{}`)
	e.exec(wsA, `INSERT INTO workbook_rows (workspace_id, workbook_id, position, data, enrichments) VALUES ($1,$2,0,'{"website":"https://www.existing.io/x"}','{}'),($1,$2,1,'{"company":"Zed Inc."}','{}')`, wsA, wb)
	body := `{"rows":[
		{"Company":"A","Website":"existing.io"},
		{"Company":"Zed, LLC"},
		{"Company":"Fresh","Website":"fresh.com"},
		{"Company":"Fresh Dup","Website":"http://www.fresh.com/a"},
		{"Company":"Same Name"},{"Company":"same name inc"},
		{"Notes":"no identity 1"},{"Notes":"no identity 2"}]}`
	res, _, err := e.importAs(wsA, wb, parseImport(t, body), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := errString(res); !strings.HasPrefix(got, `{"created":4,"added":4,"skipped_duplicates":4,"total_rows":6,`) {
		t.Errorf("response: %s", got)
	}
	// dedupe=false keeps every importable row
	wb2 := e.workbook(wsA, `[`+colCompany+`]`, "manual", `{}`)
	res, _, err = e.importAs(wsA, wb2, parseImport(t, strings.Replace(body, `{"rows"`, `{"dedupe":false,"rows"`, 1)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := errString(res); !strings.HasPrefix(got, `{"created":8,"added":8,"skipped_duplicates":0,"total_rows":8,`) {
		t.Errorf("dedupe off: %s", got)
	}
}

func TestImport501Columns400(t *testing.T) {
	e := newImpEnv(t)
	wb := e.workbook(wsA, `[`+colCompany+`]`, "empty", `{}`)
	before := e.state(wsA, wb)
	var cells []string
	for i := 0; i < 501; i++ {
		cells = append(cells, fmt.Sprintf(`"col %d":"v"`, i))
	}
	_, guard, err := e.importAs(wsA, wb, parseImport(t, `{"rows":[{`+strings.Join(cells, ",")+`}]}`), nil)
	if err != nil || guard == nil || guard.Status != 400 || guard.Detail != "CSV has more than 500 columns" {
		t.Fatalf("guard=%v err=%v", guard, err)
	}
	if n := len(e.rowData(wsA, wb)); n != 0 || e.state(wsA, wb) != before {
		t.Errorf("a refused import wrote something: rows=%d", n)
	}
	// exactly 500 headers is accepted
	if _, g, err := e.importAs(wsA, wb, parseImport(t, `{"rows":[{`+strings.Join(cells[:500], ",")+`}]}`), nil); g != nil || err != nil {
		t.Errorf("500 columns refused: %v %v", g, err)
	}
}

func TestImportAllEmptyRows400(t *testing.T) {
	e := newImpEnv(t)
	wb := e.workbook(wsA, `[`+colCompany+`]`, "empty", `{}`)
	before := e.state(wsA, wb)
	// the 501-column check comes second: an all-empty file answers "no importable rows" first
	var cells []string
	for i := 0; i < 501; i++ {
		cells = append(cells, fmt.Sprintf(`"c%d":""`, i))
	}
	for _, body := range []string{`{"rows":[{"Company":""},{"a":null},{}]}`, `{"rows":[{` + strings.Join(cells, ",") + `}]}`} {
		_, guard, err := e.importAs(wsA, wb, parseImport(t, body), nil)
		if err != nil || guard == nil || guard.Status != 400 || guard.Detail != "CSV has no importable rows" {
			t.Fatalf("guard=%v err=%v", guard, err)
		}
	}
	if e.state(wsA, wb) != before || len(e.rowData(wsA, wb)) != 0 {
		t.Error("a refused import wrote something")
	}
}

func TestImportIsAtomicOnFailure(t *testing.T) {
	e := newImpEnv(t)
	wb := e.workbook(wsA, `[`+colCompany+`]`, "empty", `{"keep":1}`)
	before := e.state(wsA, wb)
	var b strings.Builder
	b.WriteString(`{"rows":[`)
	for i := 0; i < 2500; i++ { // three batches of 1000
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"Company":"C%d","Extra":%d}`, i, i)
	}
	b.WriteString(`]}`)
	batches := 0
	boom := errors.New("injected failure on the last batch")
	_, guard, err := e.importAs(wsA, wb, parseImport(t, b.String()), func(i int) error {
		batches++
		if i == 2 {
			return boom
		}
		return nil
	})
	if !errors.Is(err, boom) || guard != nil {
		t.Fatalf("err=%v guard=%v", err, guard)
	}
	if batches != 3 {
		t.Errorf("batches = %d, want 3", batches)
	}
	if n := len(e.rowData(wsA, wb)); n != 0 {
		t.Errorf("%d rows survived a failed import", n)
	}
	if after := e.state(wsA, wb); after != before {
		t.Errorf("workbook changed by a failed import:\n%s\n%s", before, after)
	}
	if len(e.hook.calls) != 0 {
		t.Errorf("hook fired for a failed import: %v", e.hook.calls)
	}
}

func TestImportFiresHookOnce(t *testing.T) {
	e := newImpEnv(t)
	wb := e.workbook(wsA, `[`+colCompany+`]`, "empty", `{}`)
	var seen int
	e.hook.probe = func(tx pgx.Tx) {
		// same transaction: the new rows are visible although nothing committed yet
		_ = tx.QueryRow(e.ctx, `SELECT count(*) FROM workbook_rows WHERE workbook_id=$1`, wb).Scan(&seen)
	}
	var b strings.Builder
	b.WriteString(`{"rows":[`)
	for i := 0; i < 1500; i++ { // two batches, one hook call
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"Company":"Co%d"}`, i)
	}
	b.WriteString(`]}`)
	if _, g, err := e.importAs(wsA, wb, parseImport(t, b.String()), nil); g != nil || err != nil {
		t.Fatalf("%v %v", g, err)
	}
	if len(e.hook.calls) != 1 || len(e.hook.calls[0]) != 1500 || seen != 1500 {
		t.Fatalf("hook calls=%d seen=%d", len(e.hook.calls), seen)
	}
	ids := e.hook.calls[0]
	for i := 1; i < len(ids); i++ {
		if ids[i] <= ids[i-1] {
			t.Fatal("ids not in insertion order")
		}
	}
	// nothing new inserted (all duplicates) -> no hook call
	e.hook.calls = nil
	if res, _, err := e.importAs(wsA, wb, parseImport(t, `{"rows":[{"Company":"Co1"}]}`), nil); err != nil || !strings.HasPrefix(errString(res), `{"created":0,"added":0,"skipped_duplicates":1,`) {
		t.Fatalf("%v %s", err, errString(res))
	}
	if len(e.hook.calls) != 0 {
		t.Errorf("hook fired with no new rows: %v", e.hook.calls)
	}
}

func TestImportOtherWorkspaceIsNotFound(t *testing.T) {
	e := newImpEnv(t)
	wb := e.workbook(wsA, `[`+colCompany+`]`, "empty", `{}`)
	_, _, err := e.importAs(wsB, wb, parseImport(t, `{"rows":[{"Company":"X"}]}`), nil)
	if !errors.Is(err, wbcrud.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if n := len(e.rowData(wsA, wb)); n != 0 {
		t.Errorf("foreign import wrote %d rows", n)
	}
}

func TestImportMemoryBounded(t *testing.T) {
	e := newImpEnv(t)
	wb := e.workbook(wsA, `[`+colCompany+`]`, "empty", `{}`)
	var b strings.Builder
	b.WriteString(`{"rows":[`)
	for i := 0; i < wbcrud.MaxImportRows; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"Company":"Company number %d","Website":"https://d%d.test","Email":"p%d@d%d.test","Notes":"%s","City":"c%d","N":%d}`,
			i, i, i, i, strings.Repeat("n", 40), i%97, i)
	}
	b.WriteString(`]}`)
	req := parseImport(t, b.String())
	body := b.Len()
	b.Reset()
	var peak uint64
	batches := 0
	start := time.Now()
	res, g, err := e.importAs(wsA, wb, req, func(int) error {
		batches++
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		if m.HeapAlloc > peak {
			peak = m.HeapAlloc
		}
		return nil
	})
	req = wbcrud.ImportRequest{}
	if g != nil || err != nil {
		t.Fatalf("%v %v", g, err)
	}
	if got := errString(res); !strings.HasPrefix(got, `{"created":50000,"added":50000,"skipped_duplicates":0,"total_rows":50000,`) {
		t.Fatalf("response: %.200s", got)
	}
	if batches != 50 {
		t.Errorf("batches = %d, want 50 (1000 rows each)", batches)
	}
	// ~6 MiB of body; a streaming import must stay far below buffering it several times over.
	if peak > 200<<20 {
		t.Errorf("peak heap %d MiB for a %d MiB body", peak>>20, body>>20)
	}
	t.Logf("50k rows in %v, peak heap %d MiB, body %d MiB", time.Since(start), peak>>20, body>>20)
}
