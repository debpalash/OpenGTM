//go:build integration

package wbcrud_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/automationhook"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

type fakeHook struct {
	calls [][]int64
	ws    []string
	wb    []string
	err   error
}

func (f *fakeHook) RowsAdded(_ context.Context, _ pgx.Tx, ws, wb string, ids []int64) (int, error) {
	f.calls = append(f.calls, append([]int64(nil), ids...))
	f.ws, f.wb = append(f.ws, ws), append(f.wb, wb)
	return len(ids), f.err
}

func (f *fakeHook) SignalsInserted(context.Context, pgx.Tx, string, []automationhook.SignalRef) (int, error) {
	return 0, nil
}

func mustBody(t *testing.T, raw string) wbcrud.CreateBody {
	t.Helper()
	b, errs := wbcrud.ParseCreateBody([]byte(raw))
	if errs != nil {
		t.Fatalf("parse: %+v", errs)
	}
	return b
}

func seedLeads(t *testing.T, s *wbcrud.Service, ctx context.Context, ws string, n int, city, job string) {
	t.Helper()
	err := s.WithTx(ctx, ws, func(tx pgx.Tx) error {
		for i := 0; i < n; i++ {
			if _, err := tx.Exec(ctx, `INSERT INTO leads (workspace_id, company, city, collection_job_id) VALUES ($1,$2,$3,NULLIF($4,''))`,
				ws, fmt.Sprintf("%s-%s-%d", city, job, i), city, job); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

type rowInfo struct {
	Position int
	LeadID   *int64
	Data     string
	Enrich   string
}

func rowsOf(t *testing.T, s *wbcrud.Service, ctx context.Context, ws, wb string) []rowInfo {
	t.Helper()
	var out []rowInfo
	err := s.WithTx(ctx, ws, func(tx pgx.Tx) error {
		rs, err := tx.Query(ctx, `SELECT position, lead_id, data::text, enrichments::text FROM workbook_rows WHERE workbook_id=$1 ORDER BY id`, wb)
		if err != nil {
			return err
		}
		defer rs.Close()
		for rs.Next() {
			var r rowInfo
			if err := rs.Scan(&r.Position, &r.LeadID, &r.Data, &r.Enrich); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rs.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func scalar[T any](t *testing.T, s *wbcrud.Service, ctx context.Context, ws, q string, args ...any) T {
	t.Helper()
	var v T
	if err := s.WithTx(ctx, ws, func(tx pgx.Tx) error { return tx.QueryRow(ctx, q, args...).Scan(&v) }); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestCreateEmptyAndRowStorageVersion2(t *testing.T) {
	s, ctx := setup(t)
	wb, err := s.Create(ctx, wsA, mustBody(t, `{"name":"E","source_config":{"k":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(wb)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	sc := m["source_config"].(map[string]any)
	if m["source_type"] != "empty" || sc["row_storage_version"] != float64(2) || sc["k"] != float64(1) || m["total_rows"] != float64(0) || m["status"] != "draft" {
		t.Fatalf("%s", b)
	}
	if wb.UsesLegacyLeads() {
		t.Fatal("version 2 must not be a legacy leads workbook")
	}
	if got := scalar[string](t, s, ctx, wsA, `SELECT source_config::text FROM workbooks WHERE id=$1`, wb.ID); got != `{"k":1,"row_storage_version":2}` {
		t.Fatalf("stored source_config: %s", got)
	}
}

func TestCreateLeadsFilterSnapshotsRows(t *testing.T) {
	s, ctx := setup(t)
	seedLeads(t, s, ctx, wsA, 3, "Berlin", "")
	seedLeads(t, s, ctx, wsA, 2, "Oslo", "")
	seedLeads(t, s, ctx, wsB, 2, "Berlin", "")
	wb, err := s.Create(ctx, wsA, mustBody(t, `{"name":"B","source":"leads_filter","filter_criteria":{"city":"Berlin"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if wb.TotalRows != 3 || wb.SourceType != "leads_filter" {
		t.Fatalf("%+v", wb)
	}
	rows := rowsOf(t, s, ctx, wsA, wb.ID)
	if len(rows) != 3 {
		t.Fatalf("rows: %d", len(rows))
	}
	for i, r := range rows {
		var lead map[string]any
		if err := json.Unmarshal([]byte(r.Data), &lead); err != nil {
			t.Fatal(err)
		}
		if r.Position != i || r.LeadID == nil || float64(*r.LeadID) != lead["id"] || lead["city"] != "Berlin" || r.Enrich != "{}" {
			t.Errorf("row %d: %+v", i, r)
		}
	}
}

func TestCreateMaxRowsLimitsSnapshot(t *testing.T) {
	s, ctx := setup(t)
	seedLeads(t, s, ctx, wsA, 5, "Rome", "")
	wb, err := s.Create(ctx, wsA, mustBody(t, `{"name":"R","source":"leads_filter","max_rows":2}`))
	if err != nil || wb.TotalRows != 2 {
		t.Fatalf("%+v %v", wb, err)
	}
}

func TestCreateCsvTruncatesToMaxRows(t *testing.T) {
	s, ctx := setup(t)
	wb, err := s.Create(ctx, wsA, mustBody(t, `{"name":"C","source":"csv","max_rows":2,"source_config":{"rows":[{"a":1},{"a":2},{"a":3}]}}`))
	if err != nil || wb.TotalRows != 2 {
		t.Fatalf("%+v %v", wb, err)
	}
	rows := rowsOf(t, s, ctx, wsA, wb.ID)
	if len(rows) != 2 || rows[1].Position != 1 || rows[1].LeadID != nil || rows[1].Data != `{"a":2}` {
		t.Fatalf("%+v", rows)
	}
	// a non-list `rows` snapshots nothing instead of crashing
	wb, err = s.Create(ctx, wsA, mustBody(t, `{"name":"C2","source":"csv","source_config":{"rows":{"a":1}}}`))
	if err != nil || wb.TotalRows != 0 {
		t.Fatalf("%+v %v", wb, err)
	}
}

func TestCreateJobResultsSnapshot(t *testing.T) {
	s, ctx := setup(t)
	seedLeads(t, s, ctx, wsA, 2, "X", "job-1")
	seedLeads(t, s, ctx, wsA, 2, "Y", "job-2")
	wb, err := s.Create(ctx, wsA, mustBody(t, `{"name":"J","source":"job_results","source_config":{"job_ids":["job-1"]}}`))
	if err != nil || wb.TotalRows != 2 {
		t.Fatalf("%+v %v", wb, err)
	}
	wb, err = s.Create(ctx, wsA, mustBody(t, `{"name":"J0","source":"job_results","source_config":{"job_ids":[]}}`))
	if err != nil || wb.TotalRows != 0 {
		t.Fatalf("empty job ids must snapshot nothing: %+v %v", wb, err)
	}
}

func TestCreateDuplicateColumnIDs422(t *testing.T) {
	s, ctx := setup(t)
	_, err := s.Create(ctx, wsA, mustBody(t, `{"name":"D","columns_config":[{"id":"a","name":"A"},{"id":"a","name":"B"}]}`))
	var g *wbcrud.GuardError
	if !errors.As(err, &g) || g.Status != 422 || g.Detail != "Column IDs must be unique" {
		t.Fatalf("%v", err)
	}
	if n := scalar[int](t, s, ctx, wsA, `SELECT count(*) FROM workbooks`); n != 0 {
		t.Fatalf("nothing may be stored: %d", n)
	}
}

func TestCreateCycle422(t *testing.T) {
	s, ctx := setup(t)
	_, err := s.Create(ctx, wsA, mustBody(t, `{"name":"Y","columns_config":[
	 {"id":"a","name":"A","type":"ai_formula","prompt":"{B}","input_columns":["b"]},
	 {"id":"b","name":"B","type":"ai_formula","prompt":"{A}","input_columns":["a"]}]}`))
	var g *wbcrud.GuardError
	if !errors.As(err, &g) || g.Status != 422 || g.Detail != "Column changes introduce circular dependencies affecting: a, b. Break the circular references before saving." {
		t.Fatalf("%v", err)
	}
}

func TestCreateLegacyFilterCompat(t *testing.T) {
	s, ctx := setup(t)
	seedLeads(t, s, ctx, wsA, 2, "Pune", "")
	wb, err := s.Create(ctx, wsA, mustBody(t, `{"name":"L","filter_criteria":{"city":"Pune"}}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(wb)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	sc := m["source_config"].(map[string]any)
	if m["source_type"] != "leads_filter" || sc["city"] != "Pune" || sc["row_storage_version"] != float64(2) || m["total_rows"] != float64(2) {
		t.Fatalf("%s", b)
	}
	// an empty filter object does not trigger the compat switch
	wb, err = s.Create(ctx, wsA, mustBody(t, `{"name":"L2","filter_criteria":{}}`))
	if err != nil || wb.SourceType != "empty" {
		t.Fatalf("%+v %v", wb, err)
	}
}

func TestCreateFiresRowAddedHook(t *testing.T) {
	s, ctx := setup(t)
	h := &fakeHook{}
	s.Hook = h
	wb, err := s.Create(ctx, wsA, mustBody(t, `{"name":"H","source":"csv","source_config":{"rows":[{"a":1},{"a":2},{"a":3}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(h.calls) != 1 || len(h.calls[0]) != 3 || h.ws[0] != wsA || h.wb[0] != wb.ID {
		t.Fatalf("hook calls: %+v %+v", h.calls, h.wb)
	}
	ids := scalar[int](t, s, ctx, wsA, `SELECT count(*) FROM workbook_rows WHERE workbook_id=$1 AND id = ANY($2)`, wb.ID, h.calls[0])
	if ids != 3 {
		t.Fatalf("hook must receive the new row ids, matched %d", ids)
	}
	// an empty workbook adds no rows and does not call the hook
	h.calls = nil
	if _, err := s.Create(ctx, wsA, mustBody(t, `{"name":"H0"}`)); err != nil || len(h.calls) != 0 {
		t.Fatalf("%v %+v", err, h.calls)
	}
}

func TestHookFailureDoesNotFailCreate(t *testing.T) {
	s, ctx := setup(t)
	s.Hook = &fakeHook{err: errors.New("boom")}
	wb, err := s.Create(ctx, wsA, mustBody(t, `{"name":"HF","source":"csv","source_config":{"rows":[{"a":1}]}}`))
	if err != nil || wb.TotalRows != 1 {
		t.Fatalf("%+v %v", wb, err)
	}
}

func updateBody(t *testing.T, raw string) wbcrud.UpdateBody {
	t.Helper()
	b, errs := wbcrud.ParseUpdateBody([]byte(raw))
	if errs != nil {
		t.Fatalf("parse: %+v", errs)
	}
	return b
}

func guard(t *testing.T, err error, status int, detail string) {
	t.Helper()
	var g *wbcrud.GuardError
	if !errors.As(err, &g) || g.Status != status || g.Detail != detail {
		t.Fatalf("want %d %q, got %v", status, detail, err)
	}
}

func TestUpdateReplacementGuards(t *testing.T) {
	s, ctx := setup(t)
	wb, err := s.Create(ctx, wsA, mustBody(t, `{"name":"U","columns_config":[
	 {"id":"company","name":"Company","lead_field":"company"},
	 {"id":"city","name":"City","lead_field":"city"},
	 {"id":"sum","name":"Sum","type":"ai_formula","prompt":"About {Company}","input_columns":["company"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	// saved view on city
	err = s.WithTx(ctx, wsA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO workbook_views (id, workspace_id, workbook_id, name, config) VALUES ('v-t9', $1, $2, 'Only Berlin', '{"filters":[{"column":"city","op":"equals","value":"Berlin"}],"sort":[],"hidden_columns":[]}'::json)`, wsA, wb.ID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Update(ctx, wsA, wb.ID, updateBody(t, `{"columns_config":[{"id":"a","name":"A"},{"id":"a","name":"B"}]}`))
	guard(t, err, 422, "Column IDs must be unique")
	_, err = s.Update(ctx, wsA, wb.ID, updateBody(t, `{"columns_config":[{"id":"city","name":"City","lead_field":"city"},{"id":"sum","name":"Sum","type":"ai_formula","prompt":"About {Company}","input_columns":["company"]}]}`))
	guard(t, err, 409, "Replacement removes a referenced column. Update dependent columns first.")
	_, err = s.Update(ctx, wsA, wb.ID, updateBody(t, `{"columns_config":[{"id":"company","name":"Company","lead_field":"company"},{"id":"sum","name":"Sum","type":"ai_formula","prompt":"About {Company}","input_columns":["company"]}]}`))
	guard(t, err, 409, "Replacement removes a column referenced by saved view: Only Berlin. Update the view first.")
	_, err = s.Update(ctx, wsA, wb.ID, updateBody(t, `{"columns_config":[{"id":"a","name":"A","type":"ai_formula","input_columns":["b"]},{"id":"b","name":"B","type":"ai_formula","input_columns":["a"]}]}`))
	guard(t, err, 422, "Column changes introduce circular dependencies affecting: a, b. Break the circular references before saving.")
	// nothing was written by the refused attempts
	if n := scalar[string](t, s, ctx, wsA, `SELECT columns_config::text FROM workbooks WHERE id=$1`, wb.ID); len(n) == 0 {
		t.Fatal("columns vanished")
	}
	// a valid replacement succeeds
	got, err := s.Update(ctx, wsA, wb.ID, updateBody(t, `{"columns_config":[{"id":"company","name":"Company","lead_field":"company"},{"id":"city","name":"City","lead_field":"city"},{"id":"extra","name":"Extra"}]}`))
	if err != nil || len(got.Columns) != 3 {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := s.Update(ctx, wsA, "missing", updateBody(t, `{"name":"x"}`)); !errors.Is(err, wbcrud.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if _, err := s.Update(ctx, wsB, wb.ID, updateBody(t, `{"name":"x"}`)); !errors.Is(err, wbcrud.ErrNotFound) {
		t.Fatalf("other workspace must be 404: %v", err)
	}
}

func TestDeleteCascadesRowsViewsAndOtherTenantUntouched(t *testing.T) {
	s, ctx := setup(t)
	a, err := s.Create(ctx, wsA, mustBody(t, `{"name":"A","source":"csv","source_config":{"rows":[{"a":1},{"a":2}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Create(ctx, wsB, mustBody(t, `{"name":"B","source":"csv","source_config":{"rows":[{"b":1}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	err = s.WithTx(ctx, wsA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO workbook_views (id, workspace_id, workbook_id, name, config) VALUES ('v-del', $1, $2, 'V', '{}'::json)`, wsA, a.ID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, wsB, a.ID); !errors.Is(err, wbcrud.ErrNotFound) {
		t.Fatalf("foreign delete must be 404: %v", err)
	}
	if err := s.Delete(ctx, wsA, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, wsA, a.ID); !errors.Is(err, wbcrud.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	for _, q := range []string{`SELECT count(*) FROM workbook_rows WHERE workbook_id=$1`, `SELECT count(*) FROM workbook_views WHERE workbook_id=$1`} {
		if n := scalar[int](t, s, ctx, wsA, q, a.ID); n != 0 {
			t.Errorf("%s left %d", q, n)
		}
	}
	if n := scalar[int](t, s, ctx, wsB, `SELECT count(*) FROM workbook_rows WHERE workbook_id=$1`, b.ID); n != 1 {
		t.Fatalf("other tenant rows: %d", n)
	}
}

func TestMutationsAdvanceUpdatedAt(t *testing.T) {
	s, ctx := setup(t)
	wb, err := s.Create(ctx, wsA, mustBody(t, `{"name":"T"}`))
	if err != nil {
		t.Fatal(err)
	}
	stamp := func() time.Time {
		return scalar[time.Time](t, s, ctx, wsA, `SELECT updated_at FROM workbooks WHERE id=$1`, wb.ID)
	}
	set := func(at string) {
		err := s.WithTx(ctx, wsA, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE workbooks SET updated_at=$2::timestamp WHERE id=$1`, wb.ID, at)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	old := "2026-01-01 00:00:00"
	for _, raw := range []string{`{"name":"T2"}`, `{"description":"d"}`, `{"status":"paused"}`, `{"filter_criteria":{"city":"X"}}`, `{"columns_config":[{"id":"n","name":"N"}]}`} {
		set(old)
		if _, err := s.Update(ctx, wsA, wb.ID, updateBody(t, raw)); err != nil {
			t.Fatal(err)
		}
		if !stamp().After(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)) {
			t.Errorf("%s did not advance updated_at", raw)
		}
	}
	// a no-op update (same values) must not stamp
	set(old)
	if _, err := s.Update(ctx, wsA, wb.ID, updateBody(t, `{"name":"T2","status":"paused"}`)); err != nil {
		t.Fatal(err)
	}
	if stamp().After(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)) {
		t.Error("no-op update stamped updated_at")
	}
}

func TestFromJobsCreateSnapshotAndNames(t *testing.T) {
	s, ctx := setup(t)
	seedLeads(t, s, ctx, wsA, 2, "X", "job-1")
	seedLeads(t, s, ctx, wsA, 1, "Y", "job-2")
	seedLeads(t, s, ctx, wsB, 1, "Z", "job-1")
	name := "Named"
	wb, err := s.FromJobs(ctx, wsA, wbcrud.FromJobsBody{JobIDs: []string{"job-1", "job-2"}, Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	if wb.Name != "Named" || wb.TotalRows != 3 || wb.SourceType != "job_results" || wb.Description != "Created from 2 chat task(s)" || len(wb.Columns) != 18 {
		t.Fatalf("%+v", wb)
	}
	if rows := rowsOf(t, s, ctx, wsA, wb.ID); len(rows) != 3 || rows[2].Position != 2 || rows[0].LeadID == nil {
		t.Fatalf("%+v", rows)
	}
	auto, err := s.FromJobs(ctx, wsA, wbcrud.FromJobsBody{JobIDs: []string{"job-1"}})
	if err != nil || auto.Name != "Task Workbook (1 jobs)" || auto.TotalRows != 2 {
		t.Fatalf("%+v %v", auto, err)
	}
}

func TestFromJobsMergePersistsUnion(t *testing.T) {
	s, ctx := setup(t)
	seedLeads(t, s, ctx, wsA, 2, "X", "job-1")
	seedLeads(t, s, ctx, wsA, 1, "Y", "job-2")
	// empty stored filter: merged ids ARE persisted (sorted) and rows snapshotted
	empty := insert(t, s, ctx, wsA, "Empty", "job_results", `{}`, `{}`)
	wb, err := s.FromJobs(ctx, wsA, wbcrud.FromJobsBody{JobIDs: []string{"job-2", "job-1"}, WorkbookID: &empty})
	if err != nil {
		t.Fatal(err)
	}
	if got := scalar[string](t, s, ctx, wsA, `SELECT filter_criteria::text FROM workbooks WHERE id=$1`, empty); got != `{"job_ids":["job-1","job-2"]}` || wb.TotalRows != 3 {
		t.Fatalf("%s %+v", got, wb)
	}
	// non-empty stored filter: the union is persisted too (F3), other keys are kept, rows are snapshotted.
	kept := insert(t, s, ctx, wsA, "Kept", "job_results", `{}`, `{"job_ids":["job-1"],"keep":1}`)
	err = s.WithTx(ctx, wsA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE workbooks SET updated_at='2026-01-01 00:00:00' WHERE id=$1`, kept)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	wb, err = s.FromJobs(ctx, wsA, wbcrud.FromJobsBody{JobIDs: []string{"job-2"}, WorkbookID: &kept})
	if err != nil {
		t.Fatal(err)
	}
	if got := scalar[string](t, s, ctx, wsA, `SELECT filter_criteria::jsonb::text FROM workbooks WHERE id=$1`, kept); got != `{"keep": 1, "job_ids": ["job-1", "job-2"]}` || wb.TotalRows != 3 {
		t.Fatalf("%s %+v", got, wb)
	}
	if !scalar[time.Time](t, s, ctx, wsA, `SELECT updated_at FROM workbooks WHERE id=$1`, kept).After(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)) {
		t.Error("merge must stamp updated_at")
	}
	// merging again does not duplicate leads already rows
	if _, err := s.FromJobs(ctx, wsA, wbcrud.FromJobsBody{JobIDs: []string{"job-1"}, WorkbookID: &kept}); err != nil {
		t.Fatal(err)
	}
	if n := scalar[int](t, s, ctx, wsA, `SELECT count(*) FROM workbook_rows WHERE workbook_id=$1`, kept); n != 3 {
		t.Fatalf("dedup by lead_id: %d", n)
	}
	// unknown or foreign workbook -> not found
	for _, ws := range []string{wsA, wsB} {
		id := "nope"
		if ws == wsB {
			id = kept
		}
		if _, err := s.FromJobs(ctx, ws, wbcrud.FromJobsBody{JobIDs: []string{"j"}, WorkbookID: &id}); !errors.Is(err, wbcrud.ErrNotFound) {
			t.Fatalf("%s: %v", ws, err)
		}
	}
}
