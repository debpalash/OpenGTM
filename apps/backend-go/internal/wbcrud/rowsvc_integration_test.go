//go:build integration

package wbcrud_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// rowsHook records RowsAdded calls and, when probe is set, runs it on the very
// transaction the rows were inserted in.
type rowsHook struct {
	calls [][]int64
	ws    []string
	wb    []string
	probe func(tx pgx.Tx)
}

func (h *rowsHook) RowsAdded(_ context.Context, tx pgx.Tx, ws, wb string, ids []int64) (int, error) {
	h.calls = append(h.calls, append([]int64(nil), ids...))
	h.ws, h.wb = append(h.ws, ws), append(h.wb, wb)
	if h.probe != nil {
		h.probe(tx)
	}
	return len(ids), nil
}

type rowsEnv struct {
	s   *wbcrud.Service
	ctx context.Context
	t   *testing.T
}

func newRowsEnv(t *testing.T) rowsEnv {
	s, ctx := setup(t)
	return rowsEnv{s: s, ctx: ctx, t: t}
}

func (e rowsEnv) workbook(ws, name, columns string) string {
	e.t.Helper()
	id := insert(e.t, e.s, e.ctx, ws, name, "manual", `{}`, `{}`)
	if columns != "" {
		e.exec(ws, `UPDATE workbooks SET columns_config=$1::json, updated_at='2026-01-01 00:00:00' WHERE id=$2`, columns, id)
	} else {
		e.exec(ws, `UPDATE workbooks SET updated_at='2026-01-01 00:00:00' WHERE id=$1`, id)
	}
	return id
}

func (e rowsEnv) exec(ws, sql string, args ...any) {
	e.t.Helper()
	if err := e.s.WithTx(e.ctx, ws, func(tx pgx.Tx) error { _, err := tx.Exec(e.ctx, sql, args...); return err }); err != nil {
		e.t.Fatal(err)
	}
}

// row inserts a stored row with a fixed updated_at and returns its id.
func (e rowsEnv) row(ws, wb string, pos int, data string) int64 {
	e.t.Helper()
	var id int64
	err := e.s.WithTx(e.ctx, ws, func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, `INSERT INTO workbook_rows (workspace_id, workbook_id, position, data, enrichments, corroboration_count, created_at, updated_at)
			VALUES ($1,$2,$3,$4::json,'{}'::json,1,'2026-01-01 00:00:00','2026-01-01 00:00:00') RETURNING id`, ws, wb, pos, data).Scan(&id)
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e rowsEnv) data(ws string, id int64) (data string, updated time.Time) {
	e.t.Helper()
	err := e.s.WithTx(e.ctx, ws, func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, `SELECT data::text, updated_at FROM workbook_rows WHERE id=$1`, id).Scan(&data, &updated)
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return
}

func (e rowsEnv) count(ws, wb string) (n int) {
	e.t.Helper()
	err := e.s.WithTx(e.ctx, ws, func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, `SELECT count(*) FROM workbook_rows WHERE workbook_id=$1`, wb).Scan(&n)
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return
}

func (e rowsEnv) wbState(ws, wb string) (srcCfg string, updated time.Time) {
	e.t.Helper()
	err := e.s.WithTx(e.ctx, ws, func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, `SELECT source_config::text, updated_at FROM workbooks WHERE id=$1`, wb).Scan(&srcCfg, &updated)
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return
}

// in runs fn in a workspace tx with the workbook loaded (locked).
func (e rowsEnv) in(ws, wb string, fn func(tx pgx.Tx, w wbcrud.Workbook) error) error {
	e.t.Helper()
	return e.s.WithTx(e.ctx, ws, func(tx pgx.Tx) error {
		w, err := e.s.Owned(e.ctx, tx, ws, wb, true)
		if err != nil {
			return err
		}
		return fn(tx, w)
	})
}

func addReq(t *testing.T, raw string) wbcrud.AddRowsRequest {
	t.Helper()
	doc, errs := wbcrud.DecodeBody([]byte(raw))
	if errs != nil {
		t.Fatal(errs)
	}
	req, errs := wbcrud.ParseAddRows(doc)
	if errs != nil {
		t.Fatal(errs)
	}
	return req
}

func (e rowsEnv) add(ws, wb, raw string) (res wbcrud.AddRowsResult, err error) {
	e.t.Helper()
	err = e.in(ws, wb, func(tx pgx.Tx, w wbcrud.Workbook) (err error) {
		res, err = e.s.AddRows(e.ctx, tx, ws, w, addReq(e.t, raw))
		return err
	})
	return
}

func TestAddRowsDedupeSkipsExistingAndRepeated(t *testing.T) {
	e := newRowsEnv(t)
	wb := e.workbook(wsA, "dedupe", "")
	e.row(wsA, wb, 1, `{"website":"https://www.acme.com/x","company":"Existing"}`)
	e.row(wsA, wb, 2, `{"company":"Old Name Inc"}`)
	e.row(wsA, wb, 3, `{"notes":"no identity"}`)
	res, err := e.add(wsA, wb, `{"rows":[
		{"website":"acme.com"},{"domain":"ACME.com"},{"company":"New Co"},{"company":"new co, inc."},
		{"company":"Old Name"},{"notes":"x"},{"notes":"x"},{"website":"fresh.io"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 4 || res.SkippedDuplicates != 4 || res.TotalRows != 7 {
		t.Fatalf("%+v", res)
	}
	res, err = e.add(wsA, wb, `{"dedupe":false,"rows":[{"website":"acme.com"},{"website":"acme.com"}]}`)
	if err != nil || res.Added != 2 || res.SkippedDuplicates != 0 || res.TotalRows != 9 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestAddRowsAppendsPositions(t *testing.T) {
	e := newRowsEnv(t)
	empty := e.workbook(wsA, "empty", "")
	if _, err := e.add(wsA, empty, `{"rows":[{"a":1},{"a":2}]}`); err != nil {
		t.Fatal(err)
	}
	wb := e.workbook(wsA, "pos", "")
	e.row(wsA, wb, 5, `{"a":0}`)
	e.row(wsA, wb, 2, `{"a":0}`)
	if _, err := e.add(wsA, wb, `{"rows":[{"a":1,"id":7,"b":1.50},{"a":2,"id":"x"},{"a":3}]}`); err != nil {
		t.Fatal(err)
	}
	type rr struct {
		Pos     int
		Data    string
		Enrich  string
		Lead    *int
		Corrob  int
		Created time.Time
	}
	read := func(w string) (out []rr) {
		_ = e.s.WithTx(e.ctx, wsA, func(tx pgx.Tx) error {
			rows, err := tx.Query(e.ctx, `SELECT position, data::text, enrichments::text, lead_id, corroboration_count, created_at FROM workbook_rows WHERE workbook_id=$1 ORDER BY id`, w)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var r rr
				if err := rows.Scan(&r.Pos, &r.Data, &r.Enrich, &r.Lead, &r.Corrob, &r.Created); err != nil {
					return err
				}
				out = append(out, r)
			}
			return rows.Err()
		})
		return
	}
	got := read(empty)
	if len(got) != 2 || got[0].Pos != 1 || got[1].Pos != 2 {
		t.Fatalf("empty workbook positions: %+v", got)
	}
	got = read(wb)
	if len(got) != 5 || got[2].Pos != 6 || got[3].Pos != 7 || got[4].Pos != 8 {
		t.Fatalf("max+1 positions: %+v", got)
	}
	if got[2].Data != `{"a": 1, "id": 7, "b": 1.5}` || got[2].Lead == nil || *got[2].Lead != 7 {
		t.Fatalf("data/lead_id: %+v", got[2])
	}
	if got[3].Lead != nil {
		t.Fatalf("a non-integer id must not become lead_id (python 500s): %+v", got[3])
	}
	if got[2].Enrich != `{}` || got[2].Corrob != 1 || time.Since(got[2].Created) > time.Minute {
		t.Fatalf("defaults: %+v", got[2])
	}
}

func TestAddRowsFiresHookWithNewIDs(t *testing.T) {
	e := newRowsEnv(t)
	hook := &rowsHook{}
	e.s.Hook = hook
	wb := e.workbook(wsA, "hook", "")
	e.row(wsA, wb, 1, `{"website":"dup.io"}`)
	var seenInTx int
	hook.probe = func(tx pgx.Tx) {
		_ = tx.QueryRow(e.ctx, `SELECT count(*) FROM workbook_rows WHERE workbook_id=$1`, wb).Scan(&seenInTx)
	}
	res, err := e.add(wsA, wb, `{"rows":[{"website":"dup.io"},{"a":1},{"a":2}]}`)
	if err != nil || res.Added != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	if len(hook.calls) != 1 || len(hook.calls[0]) != 2 || hook.ws[0] != wsA || hook.wb[0] != wb {
		t.Fatalf("hook calls: %+v %v %v", hook.calls, hook.ws, hook.wb)
	}
	if seenInTx != 3 {
		t.Fatalf("hook must run on the inserting tx (sees its own rows): saw %d", seenInTx)
	}
	var ids []int64
	_ = e.s.WithTx(e.ctx, wsA, func(tx pgx.Tx) error {
		rows, _ := tx.Query(e.ctx, `SELECT id FROM workbook_rows WHERE workbook_id=$1 AND position > 1 ORDER BY id`, wb)
		defer rows.Close()
		for rows.Next() {
			var id int64
			_ = rows.Scan(&id)
			ids = append(ids, id)
		}
		return nil
	})
	if !reflect.DeepEqual(hook.calls[0], ids) {
		t.Fatalf("hook ids %v, inserted %v", hook.calls[0], ids)
	}
	// nothing new: no ids are announced
	hook.calls = nil
	if _, err := e.add(wsA, wb, `{"rows":[{"website":"dup.io"}]}`); err != nil {
		t.Fatal(err)
	}
	for _, c := range hook.calls {
		if len(c) != 0 {
			t.Fatalf("hook announced ids for nothing added: %v", c)
		}
	}
}

func TestAddRowsRollsBackWithTheTransaction(t *testing.T) {
	e := newRowsEnv(t)
	wb := e.workbook(wsA, "rb", "")
	boom := errors.New("boom")
	err := e.in(wsA, wb, func(tx pgx.Tx, w wbcrud.Workbook) error {
		if _, err := e.s.AddRows(e.ctx, tx, wsA, w, addReq(t, `{"rows":[{"a":1},{"a":2}]}`)); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) || e.count(wsA, wb) != 0 {
		t.Fatalf("%v rows=%d", err, e.count(wsA, wb))
	}
}

func TestAddRowsLargeBatchIsChunked(t *testing.T) {
	e := newRowsEnv(t)
	wb := e.workbook(wsA, "big", "")
	var b strings.Builder
	b.WriteString(`{"rows":[`)
	for i := 0; i < 5200; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"company":"Co %d","n":%d}`, i, i)
	}
	b.WriteString(`]}`)
	res, err := e.add(wsA, wb, b.String())
	if err != nil || res.Added != 5200 || res.TotalRows != 5200 {
		t.Fatalf("%+v %v", res, err)
	}
	var minPos, maxPos int
	_ = e.s.WithTx(e.ctx, wsA, func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, `SELECT min(position), max(position) FROM workbook_rows WHERE workbook_id=$1`, wb).Scan(&minPos, &maxPos)
	})
	if minPos != 1 || maxPos != 5200 {
		t.Fatalf("positions %d..%d", minPos, maxPos)
	}
}

func TestDeleteRowsOnlyOwnWorkbook(t *testing.T) {
	e := newRowsEnv(t)
	a := e.workbook(wsA, "a", "")
	b := e.workbook(wsA, "b", "")
	a1, a2 := e.row(wsA, a, 1, `{}`), e.row(wsA, a, 2, `{}`)
	b1 := e.row(wsA, b, 1, `{}`)
	var n int64
	err := e.in(wsA, a, func(tx pgx.Tx, w wbcrud.Workbook) (err error) {
		n, err = e.s.DeleteRows(e.ctx, tx, wsA, w, []int64{a1, b1, 99999999, a1})
		return err
	})
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if e.count(wsA, a) != 1 || e.count(wsA, b) != 1 {
		t.Fatalf("a=%d b=%d", e.count(wsA, a), e.count(wsA, b))
	}
	cfg, up := e.wbState(wsA, a)
	if cfg != `{"row_storage_version": 2}` || !up.After(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("mark_row_storage: %s %v", cfg, up)
	}
	// nothing deleted: workbook untouched
	_ = a2
	before := up
	err = e.in(wsA, a, func(tx pgx.Tx, w wbcrud.Workbook) (err error) {
		n, err = e.s.DeleteRows(e.ctx, tx, wsA, w, []int64{b1})
		return err
	})
	if err != nil || n != 0 {
		t.Fatalf("%d %v", n, err)
	}
	if _, up := e.wbState(wsA, a); !up.Equal(before) {
		t.Fatalf("delete of nothing stamped updated_at: %v -> %v", before, up)
	}
	// already marked: a further real delete changes nothing on the workbook
	err = e.in(wsA, a, func(tx pgx.Tx, w wbcrud.Workbook) (err error) {
		n, err = e.s.DeleteRows(e.ctx, tx, wsA, w, []int64{a2})
		return err
	})
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if _, up := e.wbState(wsA, a); !up.Equal(before) {
		t.Fatalf("source_config already at version 2: no UPDATE, no stamp (%v -> %v)", before, up)
	}
}

const viewCols = `[{"id":"company","name":"Company","type":"lead_field","lead_field":"company"},{"id":"city","name":"City","type":"lead_field","lead_field":"city"}]`

func (e rowsEnv) view(ws, wb, id, cfg string) {
	e.exec(ws, `INSERT INTO workbook_views (id, workspace_id, workbook_id, name, config, created_at, updated_at) VALUES ($1,$2,$3,'v',$4::json,'2026-01-01 00:00:00','2026-01-01 00:00:00')`, id, ws, wb, cfg)
}

func (e rowsEnv) deleteQuery(ws, wb string, expected int64, confirm string, view, search *string) (res wbcrud.DeleteMatchingResult, guard *wbcrud.GuardError, err error) {
	e.t.Helper()
	err = e.in(ws, wb, func(tx pgx.Tx, w wbcrud.Workbook) (err error) {
		res, guard, err = e.s.DeleteMatching(e.ctx, tx, ws, w, wbcrud.DeleteMatchingRequest{ExpectedCount: expected, Confirmation: confirm, ViewID: view, Search: search})
		return err
	})
	return
}

func strp(s string) *string { return &s }

func TestDeleteQueryDriftIs409(t *testing.T) {
	e := newRowsEnv(t)
	wb := e.workbook(wsA, "dq", viewCols)
	for i := 0; i < 3; i++ {
		e.row(wsA, wb, i+1, `{"city":"Berlin"}`)
	}
	e.view(wsA, wb, "v-dq-1", `{"filters":[{"column":"city","op":"equals","value":"berlin"}]}`)
	_, _, err := e.deleteQuery(wsA, wb, 2, "DELETE 2 ROWS", strp("v-dq-1"), nil)
	var drift *wbcrud.CountDrift
	if !errors.As(err, &drift) || drift.Expected != 2 || drift.Actual != 3 {
		t.Fatalf("want drift 2 vs 3, got %v", err)
	}
	if e.count(wsA, wb) != 3 {
		t.Fatal("drift must delete nothing")
	}
	// drift beats a wrong confirmation (python order)
	_, _, err = e.deleteQuery(wsA, wb, 2, "nope", strp("v-dq-1"), nil)
	if !errors.As(err, &drift) {
		t.Fatalf("drift is checked before the confirmation: %v", err)
	}
}

func TestDeleteQueryConfirmationMismatch400(t *testing.T) {
	e := newRowsEnv(t)
	wb := e.workbook(wsA, "dq2", viewCols)
	e.row(wsA, wb, 1, `{"city":"Berlin"}`)
	e.row(wsA, wb, 2, `{"city":"Berlin"}`)
	for _, c := range []string{"delete 2 rows", "DELETE 2 ROWS ", "", "DELETE 3 ROWS"} {
		_, guard, err := e.deleteQuery(wsA, wb, 2, c, nil, nil)
		if err != nil || guard == nil || guard.Status != 400 || guard.Detail != `confirmation must equal "DELETE 2 ROWS"` {
			t.Fatalf("%q: %+v %v", c, guard, err)
		}
	}
	if e.count(wsA, wb) != 2 {
		t.Fatal("a refused confirmation deleted rows")
	}
	res, guard, err := e.deleteQuery(wsA, wb, 2, "DELETE 2 ROWS", nil, nil)
	if err != nil || guard != nil || res.Deleted != 2 || res.Matched != 2 || e.count(wsA, wb) != 0 {
		t.Fatalf("%+v %+v %v", res, guard, err)
	}
}

func TestDeleteQueryHonoursViewAndSearch(t *testing.T) {
	e := newRowsEnv(t)
	wb := e.workbook(wsA, "dq3", viewCols)
	other := e.workbook(wsA, "dq3-other", viewCols)
	e.row(wsA, wb, 1, `{"company":"Acme 1","city":"Berlin"}`)
	e.row(wsA, wb, 2, `{"company":"Acme 2","city":"Berlin"}`)
	e.row(wsA, wb, 3, `{"company":"Beta 3","city":"Berlin"}`)
	e.row(wsA, wb, 4, `{"company":"Acme 4","city":"Rome"}`)
	e.row(wsA, other, 1, `{"company":"Acme 9","city":"Berlin"}`)
	e.view(wsA, wb, "v-dq-3", `{"filters":[{"column":"city","op":"equals","value":"Berlin"}]}`)
	e.view(wsA, other, "v-dq-3-other", `{}`)
	// view + search narrow together: Berlin AND "acme"
	res, guard, err := e.deleteQuery(wsA, wb, 2, "DELETE 2 ROWS", strp("v-dq-3"), strp("  ACME "))
	if err != nil || guard != nil || res.Deleted != 2 || res.Matched != 2 {
		t.Fatalf("%+v %+v %v", res, guard, err)
	}
	if e.count(wsA, wb) != 2 || e.count(wsA, other) != 1 {
		t.Fatalf("remaining wb=%d other=%d", e.count(wsA, wb), e.count(wsA, other))
	}
	// a view of another workbook is a 404, not a wider delete
	_, guard, err = e.deleteQuery(wsA, wb, 1, "DELETE 1 ROWS", strp("v-dq-3-other"), nil)
	if err != nil || guard == nil || guard.Status != 404 || guard.Detail != "Workbook view not found" {
		t.Fatalf("%+v %v", guard, err)
	}
	// search alone
	res, _, err = e.deleteQuery(wsA, wb, 1, "DELETE 1 ROWS", nil, strp("beta"))
	if err != nil || res.Deleted != 1 || e.count(wsA, wb) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestDeleteQueryLargeSetIsDeletedInOneStatement(t *testing.T) {
	e := newRowsEnv(t)
	wb := e.workbook(wsA, "dq-big", viewCols)
	e.exec(wsA, `INSERT INTO workbook_rows (workspace_id, workbook_id, position, data, enrichments, corroboration_count)
		SELECT $1, $2, g, '{"city":"Oslo"}'::json, '{}'::json, 1 FROM generate_series(1, 20000) g`, wsA, wb)
	res, guard, err := e.deleteQuery(wsA, wb, 20000, "DELETE 20000 ROWS", nil, strp("oslo"))
	if err != nil || guard != nil || res.Deleted != 20000 || e.count(wsA, wb) != 0 {
		t.Fatalf("%+v %+v %v", res, guard, err)
	}
}

const patchCols = `[
 {"id":"company","name":"Company","type":"lead_field","lead_field":"company"},
 {"id":"custom_in","name":"Custom","type":"input"},
 {"id":"mapped","name":"Mapped","type":"input","lead_field":"mapped_key"},
 {"id":"summary","name":"Summary","type":"ai_formula","prompt":"x {Company}","input_columns":["company"]},
 {"id":"derived","name":"Derived","type":"ai_formula","prompt":"{summary}","input_columns":["summary"]}]`

func patchBody(t *testing.T, raw string) *wbcrud.Obj {
	t.Helper()
	doc, errs := wbcrud.DecodeBody([]byte(raw))
	if errs != nil {
		t.Fatal(errs)
	}
	o, errs := wbcrud.ParseDictBody(doc)
	if errs != nil {
		t.Fatal(errs)
	}
	return o
}

func (e rowsEnv) patchRow(ws, wb string, id int64, raw string, recompute bool) (res wbcrud.RowPatch, guard *wbcrud.GuardError, err error) {
	e.t.Helper()
	err = e.in(ws, wb, func(tx pgx.Tx, w wbcrud.Workbook) (err error) {
		res, guard, err = e.s.PatchRow(e.ctx, tx, ws, w, id, patchBody(e.t, raw), recompute)
		return err
	})
	return
}

func TestPatchRowEditableFieldsOnly(t *testing.T) {
	e := newRowsEnv(t)
	wb := e.workbook(wsA, "pr", patchCols)
	id := e.row(wsA, wb, 1, `{"company":"Acme","extra":1}`)
	res, guard, err := e.patchRow(wsA, wb, id, `{"city":"Rome","custom_in":"c","mapped_key":"m","summary":"no","derived":"no","id":5,"row_id":6,"lead_id":7,"unknown":1,"company":"Acme 2"}`, false)
	if err != nil || guard != nil {
		t.Fatalf("%+v %v", guard, err)
	}
	if !reflect.DeepEqual(res.Fields, []string{"city", "custom_in", "mapped_key", "company"}) {
		t.Fatalf("fields (body order, editable only): %v", res.Fields)
	}
	data, _ := e.data(wsA, id)
	if data != `{"company": "Acme 2", "extra": 1, "city": "Rome", "custom_in": "c", "mapped_key": "m"}` {
		t.Fatalf("stored: %s", data)
	}
	for _, body := range []string{`{}`, `{"unknown":1}`, `{"id":1,"row_id":2,"lead_id":3}`, `{"summary":"x","derived":"y"}`} {
		if _, g, err := e.patchRow(wsA, wb, id, body, false); err != nil || g == nil || g.Status != 400 || g.Detail != "No editable row fields supplied" {
			t.Fatalf("%s: %+v %v", body, g, err)
		}
	}
	if _, g, err := e.patchRow(wsA, wb, id+1000, `{"city":"x"}`, false); err != nil || g == nil || g.Status != 404 || g.Detail != "Workbook row not found" {
		t.Fatalf("%+v %v", g, err)
	}
	other := e.workbook(wsA, "pr-other", patchCols)
	if _, g, err := e.patchRow(wsA, other, id, `{"city":"x"}`, false); err != nil || g == nil || g.Status != 404 {
		t.Fatalf("row of another workbook must be 404: %+v %v", g, err)
	}
}

func TestPatchRowFalseVsZeroIsAChange(t *testing.T) {
	e := newRowsEnv(t)
	wb := e.workbook(wsA, "pr2", patchCols)
	id := e.row(wsA, wb, 1, `{"score":false,"city":1,"f":1.0,"notes":{"b":1,"a":2}}`)
	_, base := e.data(wsA, id)
	if _, _, err := e.patchRow(wsA, wb, id, `{"score":false,"city":1,"notes":{"a":2,"b":1}}`, false); err != nil {
		t.Fatal(err)
	}
	if d, up := e.data(wsA, id); !up.Equal(base) || d != `{"score":false,"city":1,"f":1.0,"notes":{"b":1,"a":2}}` {
		t.Fatalf("identical values (key order aside) must not write or stamp: %s %v", d, up)
	}
	if _, _, err := e.patchRow(wsA, wb, id, `{"score":0}`, false); err != nil {
		t.Fatal(err)
	}
	d, up := e.data(wsA, id)
	if !up.After(base) || !strings.Contains(d, `"score": 0`) {
		t.Fatalf("false -> 0 is a change: %s %v", d, up)
	}
	_, base = e.data(wsA, id)
	if _, _, err := e.patchRow(wsA, wb, id, `{"city":1.0}`, false); err != nil { // 1 vs 1.0 differ under json.dumps
		t.Fatal(err)
	}
	if d, up := e.data(wsA, id); !up.After(base) || !strings.Contains(d, `"city": 1.0`) {
		t.Fatalf("1 -> 1.0 is a change: %s %v", d, up)
	}
}

func TestPatchRowReportsReactiveColumns(t *testing.T) {
	e := newRowsEnv(t)
	wb := e.workbook(wsA, "pr3", patchCols)
	id := e.row(wsA, wb, 1, `{"company":"Acme"}`)
	res, _, err := e.patchRow(wsA, wb, id, `{"company":"Acme 2"}`, true)
	if err != nil || !reflect.DeepEqual(res.Reactive, []any{"summary", "derived"}) {
		t.Fatalf("%+v %v", res, err)
	}
	res, _, _ = e.patchRow(wsA, wb, id, `{"company":"Acme 2"}`, true)
	if len(res.Reactive) != 0 {
		t.Fatalf("an unchanged edit recomputes nothing: %+v", res)
	}
	res, _, _ = e.patchRow(wsA, wb, id, `{"company":"Acme 3"}`, false)
	if len(res.Reactive) != 0 {
		t.Fatalf("recompute=false: %+v", res)
	}
}

func bulkReq(t *testing.T, raw string) []wbcrud.RowUpdate {
	t.Helper()
	doc, errs := wbcrud.DecodeBody([]byte(raw))
	if errs != nil {
		t.Fatal(errs)
	}
	req, errs := wbcrud.ParseBulkUpdate(doc)
	if errs != nil {
		t.Fatal(errs)
	}
	return req.Updates
}

func (e rowsEnv) bulk(ws, wb, raw string, recompute bool) (res wbcrud.BulkPatch, guard *wbcrud.GuardError, err error) {
	e.t.Helper()
	err = e.in(ws, wb, func(tx pgx.Tx, w wbcrud.Workbook) (err error) {
		res, guard, err = e.s.BulkPatchRows(e.ctx, tx, ws, w, bulkReq(e.t, raw), recompute)
		return err
	})
	return
}

func TestBulkPatchAtomicOnForeignID(t *testing.T) {
	e := newRowsEnv(t)
	wb := e.workbook(wsA, "bp", patchCols)
	other := e.workbook(wsA, "bp-other", patchCols)
	r1 := e.row(wsA, wb, 1, `{"city":"A"}`)
	r2 := e.row(wsA, wb, 2, `{"city":"B"}`)
	foreign := e.row(wsA, other, 1, `{"city":"F"}`)
	d1, u1 := e.data(wsA, r1)
	for _, raw := range []string{
		fmt.Sprintf(`{"updates":[{"row_id":%d,"fields":{"city":"X"}},{"row_id":%d,"fields":{"city":"X"}}]}`, r1, foreign),
		fmt.Sprintf(`{"updates":[{"row_id":%d,"fields":{"city":"X"}},{"row_id":99999999,"fields":{"city":"X"}}]}`, r1),
	} {
		_, g, err := e.bulk(wsA, wb, raw, false)
		if err != nil || g == nil || g.Status != 404 || g.Detail != "One or more workbook rows were not found" {
			t.Fatalf("%s: %+v %v", raw, g, err)
		}
	}
	// the second item is invalid: the first item's change must not land either
	_, g, err := e.bulk(wsA, wb, fmt.Sprintf(`{"updates":[{"row_id":%d,"fields":{"city":"X"}},{"row_id":%d,"fields":{"city":"Y","derived":"no"}}]}`, r1, r2), false)
	if err != nil || g == nil || g.Status != 400 || g.Detail != fmt.Sprintf("Row %d contains non-editable fields", r2) {
		t.Fatalf("%+v %v", g, err)
	}
	if d, u := e.data(wsA, r1); d != d1 || !u.Equal(u1) {
		t.Fatalf("partial write: %s %v", d, u)
	}
	if d, _ := e.data(wsA, foreign); d != `{"city":"F"}` {
		t.Fatalf("foreign row touched: %s", d)
	}
}

func TestBulkPatchDuplicateIDs400(t *testing.T) {
	e := newRowsEnv(t)
	wb := e.workbook(wsA, "bpd", patchCols)
	r1 := e.row(wsA, wb, 1, `{"city":"A"}`)
	_, g, err := e.bulk(wsA, wb, fmt.Sprintf(`{"updates":[{"row_id":%d,"fields":{"city":"X"}},{"row_id":%d,"fields":{"city":"Y"}}]}`, r1, r1), false)
	if err != nil || g == nil || g.Status != 400 || g.Detail != "Duplicate row IDs are not allowed" {
		t.Fatalf("%+v %v", g, err)
	}
	if d, _ := e.data(wsA, r1); d != `{"city":"A"}` {
		t.Fatal("duplicate request wrote")
	}
}

func TestBulkPatchUnchangedValuesNotStamped(t *testing.T) {
	e := newRowsEnv(t)
	wb := e.workbook(wsA, "bpu", patchCols)
	r1 := e.row(wsA, wb, 1, `{"city":"A","score":0}`)
	r2 := e.row(wsA, wb, 2, `{"city":"B"}`)
	r3 := e.row(wsA, wb, 3, `{"city":"C"}`)
	_, u1 := e.data(wsA, r1)
	res, g, err := e.bulk(wsA, wb, fmt.Sprintf(`{"updates":[
		{"row_id":%d,"fields":{"city":"A","score":0}},{"row_id":%d,"fields":{"city":"B2"}},{"row_id":%d,"fields":{"city":"C","company":"Co"}}]}`, r1, r2, r3), true)
	if err != nil || g != nil || res.Updated != 3 {
		t.Fatalf("%+v %+v %v", res, g, err)
	}
	if d, u := e.data(wsA, r1); !u.Equal(u1) || d != `{"city":"A","score":0}` {
		t.Fatalf("unchanged row rewritten/stamped: %s %v", d, u)
	}
	if d, u := e.data(wsA, r2); !u.After(u1) || d != `{"city": "B2"}` {
		t.Fatalf("changed row: %s %v", d, u)
	}
	if d, u := e.data(wsA, r3); !u.After(u1) || d != `{"city": "C", "company": "Co"}` {
		t.Fatalf("changed row: %s %v", d, u)
	}
	if !reflect.DeepEqual(res.ChangedFields, []string{"city", "company"}) || !reflect.DeepEqual(res.Reactive, []any{"summary", "derived"}) {
		t.Fatalf("%+v", res)
	}
	// all unchanged: nothing reactive, updated_rows still counts every item
	res, _, _ = e.bulk(wsA, wb, fmt.Sprintf(`{"updates":[{"row_id":%d,"fields":{"city":"A"}}]}`, r1), true)
	if res.Updated != 1 || len(res.Reactive) != 0 || len(res.ChangedFields) != 0 {
		t.Fatalf("%+v", res)
	}
}

func TestBulkPatchThousandRowsOneTransaction(t *testing.T) {
	e := newRowsEnv(t)
	wb := e.workbook(wsA, "bp1000", patchCols)
	e.exec(wsA, `INSERT INTO workbook_rows (workspace_id, workbook_id, position, data, enrichments, corroboration_count)
		SELECT $1, $2, g, '{"city":"A"}'::json, '{}'::json, 1 FROM generate_series(1, 1000) g`, wsA, wb)
	var ids []int64
	_ = e.s.WithTx(e.ctx, wsA, func(tx pgx.Tx) error {
		rows, _ := tx.Query(e.ctx, `SELECT id FROM workbook_rows WHERE workbook_id=$1 ORDER BY id`, wb)
		defer rows.Close()
		for rows.Next() {
			var id int64
			_ = rows.Scan(&id)
			ids = append(ids, id)
		}
		return nil
	})
	var b strings.Builder
	b.WriteString(`{"updates":[`)
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"row_id":%d,"fields":{"city":"Z%d"}}`, id, i)
	}
	b.WriteString(`]}`)
	res, g, err := e.bulk(wsA, wb, b.String(), false)
	if err != nil || g != nil || res.Updated != 1000 {
		t.Fatalf("%+v %+v %v", res, g, err)
	}
	var n int
	_ = e.s.WithTx(e.ctx, wsA, func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, `SELECT count(*) FROM workbook_rows WHERE workbook_id=$1 AND data->>'city' LIKE 'Z%'`, wb).Scan(&n)
	})
	if n != 1000 {
		t.Fatalf("%d rows updated", n)
	}
}

func (e rowsEnv) lead(ws string, id int, company, city string) {
	e.exec(ws, `INSERT INTO leads (id, workspace_id, company, city, updated_at) VALUES ($1,$2,$3,$4,'2026-01-01T00:00:00')`, id, ws, company, city)
}

func (e rowsEnv) leadState(ws string, id int) (company, city string, score *int, updated string) {
	e.t.Helper()
	err := e.s.WithTx(e.ctx, ws, func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, `SELECT company, coalesce(city,'<null>'), score, updated_at FROM leads WHERE id=$1`, id).Scan(&company, &city, &score, &updated)
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return
}

func (e rowsEnv) updateLead(ws string, id int64, raw string) (keys []string, guard *wbcrud.GuardError, err error) {
	e.t.Helper()
	err = e.s.WithTx(e.ctx, ws, func(tx pgx.Tx) (err error) {
		keys, guard, err = e.s.UpdateLead(e.ctx, tx, ws, id, patchBody(e.t, raw))
		return err
	})
	return
}

func TestUpdateLeadFieldAllowlist(t *testing.T) {
	e := newRowsEnv(t)
	e.lead(wsA, 981001, "Acme", "Berlin")
	e.lead(wsB, 981002, "Other", "Rome")
	keys, g, err := e.updateLead(wsA, 981001, `{"city":"Oslo","bogus":1,"id":5,"workspace_id":"x","updated_at":"y","company":"Acme Ltd","score":7}`)
	if err != nil || g != nil || !reflect.DeepEqual(keys, []string{"city", "company", "score"}) {
		t.Fatalf("%v %+v %v", keys, g, err)
	}
	co, city, score, upd := e.leadState(wsA, 981001)
	if co != "Acme Ltd" || city != "Oslo" || score == nil || *score != 7 {
		t.Fatalf("%s %s %v", co, city, score)
	}
	if !regexpISO.MatchString(upd) {
		t.Fatalf("lead updated_at must be python isoformat(utc): %q", upd)
	}
	for _, raw := range []string{`{}`, `{"bogus":1}`, `{"id":3,"created_at":"x"}`} {
		if _, g, err := e.updateLead(wsA, 981001, raw); err != nil || g == nil || g.Status != 400 || g.Detail != "No valid lead fields to update" {
			t.Fatalf("%s: %+v %v", raw, g, err)
		}
	}
	if _, g, err := e.updateLead(wsA, 981999, `{"city":"x"}`); err != nil || g == nil || g.Status != 404 || g.Detail != "Lead not found" {
		t.Fatalf("%+v %v", g, err)
	}
	// another workspace's lead: RLS + predicate => 404, untouched
	if _, g, err := e.updateLead(wsA, 981002, `{"city":"Hacked"}`); err != nil || g == nil || g.Status != 404 {
		t.Fatalf("%+v %v", g, err)
	}
	if _, city, _, _ := e.leadState(wsB, 981002); city != "Rome" {
		t.Fatalf("cross-workspace write: %s", city)
	}
	// no valid fields is checked before existence (python order)
	if _, g, _ := e.updateLead(wsA, 981999, `{"bogus":1}`); g == nil || g.Status != 400 {
		t.Fatalf("%+v", g)
	}
}

func TestUpdateLeadBadValuesAreClientErrors(t *testing.T) {
	e := newRowsEnv(t)
	e.lead(wsA, 981010, "Acme", "Berlin")
	for _, raw := range []string{`{"score":"abc"}`, `{"score":[1]}`, `{"score":99999999999}`, `{"company":null}`, `{"notes":{"a":1}}`, `{"city":["a"]}`} {
		_, g, err := e.updateLead(wsA, 981010, raw)
		if err != nil || g == nil || g.Status != 400 {
			t.Fatalf("%s: want a 400 guard (python answers 500): %+v %v", raw, g, err)
		}
	}
	co, city, score, _ := e.leadState(wsA, 981010)
	if co != "Acme" || city != "Berlin" || score != nil {
		t.Fatal("a refused update must not write")
	}
	// values python's driver coerces still work
	if _, g, err := e.updateLead(wsA, 981010, `{"score":"7","city":5,"notes":null,"phone":true}`); err != nil || g != nil {
		t.Fatalf("%+v %v", g, err)
	}
	_, city, score, _ = e.leadState(wsA, 981010)
	if city != "5" || score == nil || *score != 7 {
		t.Fatalf("%s %v", city, score)
	}
	if _, _, err := e.updateLead(wsA, 981010, `{"score":7.5}`); err != nil {
		t.Fatal(err)
	}
	if _, _, score, _ = e.leadState(wsA, 981010); score == nil || *score != 8 {
		t.Fatalf("postgres rounds 7.5 to 8: %v", score)
	}
}

func TestRowsTenantIsolation(t *testing.T) {
	e := newRowsEnv(t)
	wb := e.workbook(wsA, "iso", patchCols)
	id := e.row(wsA, wb, 1, `{"city":"A"}`)
	// ws B cannot even load the workbook, so no row write can start
	err := e.s.WithTx(e.ctx, wsB, func(tx pgx.Tx) error {
		if _, err := e.s.Owned(e.ctx, tx, wsB, wb, true); !errors.Is(err, wbcrud.ErrNotFound) {
			t.Errorf("workbook visible across workspaces: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// even handed the workbook (as if the ownership check were bypassed), RLS hides rows from B's tx
	loaded := wbcrud.Workbook{ID: wb, Columns: nil}
	err = e.s.WithTx(e.ctx, wsB, func(tx pgx.Tx) error {
		if n, err := e.s.DeleteRows(e.ctx, tx, wsB, loaded, []int64{id}); err != nil || n != 0 {
			t.Errorf("delete across workspaces: %d %v", n, err)
		}
		_, g, err := e.s.PatchRow(e.ctx, tx, wsB, loaded, id, patchBody(t, `{"city":"X"}`), false)
		if err != nil || g == nil || g.Status != 404 {
			t.Errorf("patch across workspaces: %+v %v", g, err)
		}
		_, g, err = e.s.BulkPatchRows(e.ctx, tx, wsB, loaded, bulkReq(t, fmt.Sprintf(`{"updates":[{"row_id":%d,"fields":{"city":"X"}}]}`, id)), false)
		if err != nil || g == nil || g.Status != 404 {
			t.Errorf("bulk across workspaces: %+v %v", g, err)
		}
		if _, err := e.s.AddRows(e.ctx, tx, wsB, loaded, addReq(t, `{"rows":[{"a":1}]}`)); !errors.Is(err, wbcrud.ErrNotFound) {
			t.Errorf("insert of a row for another workspace's workbook must be refused: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := e.data(wsA, id); d != `{"city":"A"}` || e.count(wsA, wb) != 1 {
		t.Fatalf("ws A rows changed: %s", d)
	}
}

var regexpISO = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d{6})?\+00:00$`)

// A sorted view binds sort-column parameters after the where parameters; the
// count and delete statements must only receive the ones they reference.
func TestDeleteQueryWithSortedViewAndSearch(t *testing.T) {
	e := newRowsEnv(t)
	wb := e.workbook(wsA, "dq-sorted", viewCols)
	e.row(wsA, wb, 1, `{"company":"Zed","city":"Berlin"}`)
	e.row(wsA, wb, 2, `{"company":"Abe","city":"Berlin"}`)
	e.row(wsA, wb, 3, `{"company":"Mid","city":"Rome"}`)
	e.view(wsA, wb, "v-dq-sorted", `{"sort":[{"column":"company","dir":"asc"}],"filters":[{"column":"city","op":"equals","value":"Berlin"}]}`)
	_, _, err := e.deleteQuery(wsA, wb, 1, "DELETE 1 ROWS", strp("v-dq-sorted"), strp("nomatch"))
	var drift *wbcrud.CountDrift
	if !errors.As(err, &drift) || drift.Actual != 0 {
		t.Fatalf("want drift to 0, got %v", err)
	}
	res, guard, err := e.deleteQuery(wsA, wb, 2, "DELETE 2 ROWS", strp("v-dq-sorted"), nil)
	if err != nil || guard != nil || res.Deleted != 2 || e.count(wsA, wb) != 1 {
		t.Fatalf("%+v %+v %v", res, guard, err)
	}
}
