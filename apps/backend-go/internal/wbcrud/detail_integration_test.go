//go:build integration

package wbcrud_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

const dtColumns = `[{"id":"company","name":"Company","type":"lead_field","width":200,"lead_field":"company"},
{"id":"city","name":"City","type":"lead_field","width":150,"lead_field":"city"},
{"id":"note","name":"Note","type":"lead_field","width":150,"lead_field":"note"}]`

func dtWorkbook(t *testing.T, s *wbcrud.Service, ctx context.Context, ws string) string {
	t.Helper()
	id := insert(t, s, ctx, ws, "Walk", "manual", `{}`, `{}`)
	dtExec(t, s, ctx, ws, `UPDATE workbooks SET columns_config=$1::json WHERE id=$2`, dtColumns, id)
	return id
}

func dtExec(t *testing.T, s *wbcrud.Service, ctx context.Context, ws, sql string, args ...any) {
	t.Helper()
	if err := s.WithTx(ctx, ws, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, sql, args...); return err }); err != nil {
		t.Fatal(err)
	}
}

func dtRowIDs(t *testing.T, res *wbcrud.Obj) []int64 {
	t.Helper()
	rows, _ := res.Get("rows")
	var ids []int64
	for _, r := range rows.([]any) {
		id, _ := r.(*wbcrud.Obj).Get("row_id")
		ids = append(ids, id.(int64))
	}
	return ids
}

func dtStr(res *wbcrud.Obj, key string) (string, bool) {
	v, _ := res.Get(key)
	s, ok := v.(string)
	return s, ok
}

// 47 rows: cities repeat, some are NULL or empty (coalesce ties), a few positions repeat.
func dtSeedWalk(t *testing.T, s *wbcrud.Service, ctx context.Context, ws, wb string) {
	t.Helper()
	dtExec(t, s, ctx, ws, `INSERT INTO workbook_rows (workbook_id, position, data, enrichments, workspace_id)
		SELECT $1, g / 2, CASE WHEN g % 7 = 0 THEN json_build_object('company','C'||g)
		                       WHEN g % 5 = 0 THEN json_build_object('company','C'||g,'city','')
		                       ELSE json_build_object('company','C'||g,'city',(ARRAY['Oslo','Lyon','Pune'])[g % 3 + 1]) END,
		       '{}'::json, $2 FROM generate_series(1,47) g`, wb, ws)
	dtExec(t, s, ctx, ws, `INSERT INTO workbook_views (id, workspace_id, workbook_id, name, config)
		VALUES ('v-city', $1, $2, 'city desc', '{"sort":[{"column":"city","dir":"desc"}]}'::json)`, ws, wb)
}

func TestDetailCursorWalkCoversAllRowsOnce(t *testing.T) {
	s, ctx := setup(t)
	wb := dtWorkbook(t, s, ctx, wsA)
	dtSeedWalk(t, s, ctx, wsA, wb)
	for _, view := range []string{"", "v-city"} {
		// Reference order: one offset page with everything.
		all, err := s.Detail(ctx, wsA, wb, wbcrud.DetailParams{Page: 1, PageSize: 100, ViewID: view})
		if err != nil {
			t.Fatal(err)
		}
		want := dtRowIDs(t, all)
		if len(want) != 47 {
			t.Fatalf("view %q: %d rows", view, len(want))
		}
		for _, size := range []int64{1, 2, 5, 9, 46, 47, 48} {
			var got []int64
			var cursor *string
			for page := 0; ; page++ {
				if page > 100 {
					t.Fatalf("view %q size %d: walk does not end", view, size)
				}
				res, err := s.Detail(ctx, wsA, wb, wbcrud.DetailParams{Page: 1, PageSize: size, ViewID: view, CursorMode: true, Cursor: cursor})
				if err != nil {
					t.Fatalf("view %q size %d page %d: %v", view, size, page, err)
				}
				got = append(got, dtRowIDs(t, res)...)
				next, ok := dtStr(res, "next_cursor")
				more, _ := res.Get("has_more")
				if !ok {
					if more.(bool) {
						t.Fatalf("has_more without next_cursor")
					}
					break
				}
				cursor = &next
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("view %q size %d: keyset walk differs from offset order\n got %v\nwant %v", view, size, got, want)
			}
		}
		// Offset paging covers the same rows once.
		var viaOffset []int64
		for page := int64(1); page <= 10; page++ {
			res, err := s.Detail(ctx, wsA, wb, wbcrud.DetailParams{Page: page, PageSize: 5, ViewID: view})
			if err != nil {
				t.Fatal(err)
			}
			viaOffset = append(viaOffset, dtRowIDs(t, res)...)
		}
		if !reflect.DeepEqual(viaOffset, want) {
			t.Fatalf("view %q offset walk differs", view)
		}
	}
}

func TestDetailCursorGuards(t *testing.T) {
	s, ctx := setup(t)
	wb := dtWorkbook(t, s, ctx, wsA)
	dtSeedWalk(t, s, ctx, wsA, wb)
	bad := "!!!"
	_, err := s.Detail(ctx, wsA, wb, wbcrud.DetailParams{Page: 1, PageSize: 5, Cursor: &bad})
	var g *wbcrud.GuardError
	if !errors.As(err, &g) || g.Status != 400 || g.Detail != "Invalid workbook row cursor" {
		t.Fatalf("bad cursor: %v", err)
	}
	if _, err = s.Detail(ctx, wsA, wb, wbcrud.DetailParams{Page: 1, PageSize: 5, ViewID: "nope"}); !errors.As(err, &g) || g.Status != 404 || g.Detail != "Workbook view not found" {
		t.Fatalf("unknown view: %v", err)
	}
	if _, err = s.Detail(ctx, wsB, wb, wbcrud.DetailParams{Page: 1, PageSize: 5}); !errors.Is(err, wbcrud.ErrNotFound) {
		t.Fatalf("other workspace: %v", err)
	}
	// A cursor of the plain order applied to the sorted view: same shape check as Python (3 terms vs 2).
	first, err := s.Detail(ctx, wsA, wb, wbcrud.DetailParams{Page: 1, PageSize: 3, CursorMode: true})
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := dtStr(first, "next_cursor")
	if _, err = s.Detail(ctx, wsA, wb, wbcrud.DetailParams{Page: 1, PageSize: 3, ViewID: "v-city", Cursor: &plain}); !errors.As(err, &g) || g.Status != 400 {
		t.Fatalf("row cursor on sorted view: %v", err)
	}
}

// failAfter writes through to dst and fails once more than limit bytes were accepted.
type failAfter struct {
	dst      bytes.Buffer
	limit    int
	maxWrite int
	writes   int
}

var errClientGone = errors.New("client gone")

func (f *failAfter) Write(p []byte) (int, error) {
	if f.limit > 0 && f.dst.Len() >= f.limit {
		return 0, errClientGone
	}
	f.writes++
	if len(p) > f.maxWrite {
		f.maxWrite = len(p)
	}
	return f.dst.Write(p)
}

// countingWriter drops the bytes (so the test itself holds nothing) and records chunk sizes.
type countingWriter struct {
	n, maxWrite, writes, lines int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += len(p)
	c.writes++
	c.lines += bytes.Count(p, []byte("\r\n"))
	if len(p) > c.maxWrite {
		c.maxWrite = len(p)
	}
	return len(p), nil
}

func TestExportStreamsWithoutBufferingAllRows(t *testing.T) {
	s, ctx := setup(t)
	wb := dtWorkbook(t, s, ctx, wsA)
	dtExec(t, s, ctx, wsA, `INSERT INTO workbook_rows (workbook_id, position, data, enrichments, workspace_id)
		SELECT $1, g, json_build_object('company','Company '||g,'city','Oslo','note',repeat('x', 200)), '{}'::json, $2
		FROM generate_series(0,9999) g`, wb, wsA)

	open := func(w io.Writer) func(string) io.Writer { return func(string) io.Writer { return w } }

	// A client that disappears after ~100 KB: the export stops there instead of materialising ~2.5 MB.
	f := &failAfter{limit: 100 << 10}
	err := s.ExportCSV(ctx, wsA, wb, wbcrud.ExportParams{}, open(f))
	if !errors.Is(err, errClientGone) {
		t.Fatalf("expected the write error, got %v", err)
	}
	if f.dst.Len() > 300<<10 {
		t.Fatalf("export wrote %d bytes after the client had gone: it is not streaming", f.dst.Len())
	}

	cw := &countingWriter{}
	if err := s.ExportCSV(ctx, wsA, wb, wbcrud.ExportParams{}, open(cw)); err != nil {
		t.Fatal(err)
	}
	if cw.lines != 10001 {
		t.Fatalf("lines = %d, want header + 10000", cw.lines)
	}
	if cw.n < 2<<20 {
		t.Fatalf("only %d bytes for 10k rows", cw.n)
	}
	if cw.maxWrite > 64<<10 || cw.writes < cw.n/(64<<10) {
		t.Fatalf("output is not chunked: %d writes, largest %d bytes for %d total", cw.writes, cw.maxWrite, cw.n)
	}
}

func TestExportGuardsAndFilename(t *testing.T) {
	s, ctx := setup(t)
	wb := dtWorkbook(t, s, ctx, wsA)
	dtSeedWalk(t, s, ctx, wsA, wb)
	var out bytes.Buffer
	var name string
	open := func(n string) io.Writer { name = n; return &out }
	var g *wbcrud.GuardError
	if err := s.ExportCSV(ctx, wsA, wb, wbcrud.ExportParams{ColumnIDs: []string{"company", "x1", "x2"}}, open); !errors.As(err, &g) || g.Status != 400 || g.Detail != "Unknown columns: x1, x2" {
		t.Fatalf("unknown columns: %v", err)
	}
	if name != "" || out.Len() != 0 {
		t.Fatal("a rejected export must not open the response")
	}
	ids := make([]int64, 1001)
	if err := s.ExportCSV(ctx, wsA, wb, wbcrud.ExportParams{RowIDs: ids}, open); !errors.As(err, &g) || g.Status != 400 || g.Detail != "Selected export is limited to 1,000 rows" {
		t.Fatalf("1001 ids: %v", err)
	}
	if err := s.ExportCSV(ctx, wsB, wb, wbcrud.ExportParams{}, open); !errors.Is(err, wbcrud.ErrNotFound) {
		t.Fatalf("other workspace: %v", err)
	}
	if err := s.ExportCSV(ctx, wsA, wb, wbcrud.ExportParams{ColumnIDs: []string{"company"}, RowIDs: []int64{0}}, open); err != nil {
		t.Fatal(err)
	}
	if name != "Walk" || out.String() != "﻿Company\r\n" {
		t.Fatalf("name %q body %q", name, out.String())
	}
}

func TestConnectorRunsPaginateAndRejectBadCursor(t *testing.T) {
	s, ctx := setup(t)
	wb := dtWorkbook(t, s, ctx, wsA)
	other := insert(t, s, ctx, wsA, "Other", "manual", `{}`, `{}`)
	for i, ts := range []string{"2026-08-29 10:00:00+00", "2026-08-29 10:00:00+00", "2026-08-29 10:00:00.250000+00", "2026-08-30 09:00:00+00", "2026-08-28 09:00:00+00", "2026-08-29 10:00:00+00", "2026-09-01 00:00:00+00"} {
		dtExec(t, s, ctx, wsA, `INSERT INTO connector_runs (id, workspace_id, workbook_id, connector, status, query, cursor, requested_count,
			fetched_count, added_count, updated_count, skipped_count, pages_fetched, target_met, exhausted, created_at, updated_at)
			VALUES ($1,$2,$3,$4,'completed',NULL,NULL,1,1,1,0,0,1,true,false,$5,$5)`, fmt.Sprintf("run-%d", i), wsA, wb, fmt.Sprintf("c%d", i), ts)
	}
	dtExec(t, s, ctx, wsA, `INSERT INTO connector_runs (id, workspace_id, workbook_id, connector, status, requested_count, fetched_count, added_count,
		updated_count, skipped_count, pages_fetched, target_met, exhausted) VALUES ('run-other',$1,$2,'x','pending',0,0,0,0,0,0,false,false)`, wsA, other)

	ids := func(res *wbcrud.Obj) []string {
		runs, _ := res.Get("runs")
		var out []string
		for _, r := range runs.([]any) {
			id, _ := r.(*wbcrud.Obj).Get("id")
			out = append(out, id.(string))
		}
		return out
	}
	all, err := s.ConnectorRuns(ctx, wsA, wb, wbcrud.ConnectorRunsParams{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"run-6", "run-3", "run-2", "run-5", "run-1", "run-0", "run-4"}
	if got := ids(all); !reflect.DeepEqual(got, want) {
		t.Fatalf("order %v, want created_at desc, id desc: %v", got, want)
	}
	var got []string
	cursor := ""
	for page := 0; page < 10; page++ {
		res, err := s.ConnectorRuns(ctx, wsA, wb, wbcrud.ConnectorRunsParams{Limit: 2, Offset: 3, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		if off, _ := res.Get("offset"); (cursor == "") != (off != nil) {
			t.Fatalf("offset %v with cursor %q", off, cursor)
		}
		got = append(got, ids(res)...)
		next, ok := dtStr(res, "next_cursor")
		if !ok {
			break
		}
		cursor = next
		if page == 0 && !reflect.DeepEqual(got, want[3:5]) {
			t.Fatalf("first page with offset 3: %v", got)
		}
	}
	// The first page skipped 3 rows (offset); every later page follows the cursor, so nothing is dropped or repeated.
	if !reflect.DeepEqual(got, want[3:]) {
		t.Fatalf("walk %v, want %v", got, want[3:])
	}
	for _, bad := range []string{"!!!", "YWJj", "WzEsMl0", "eyJ2IjoxLCJ2YWx1ZXMiOlsibm90LWEtZGF0ZSIsInJ1biJdfQ", "eyJ2IjoyLCJyb3ciOjQsInNjb3BlIjoieCIsImRpZ2VzdCI6InkifQ"} {
		_, err := s.ConnectorRuns(ctx, wsA, wb, wbcrud.ConnectorRunsParams{Limit: 5, Cursor: bad})
		var g *wbcrud.GuardError
		if !errors.As(err, &g) || g.Status != 400 || g.Detail != "Invalid connector run cursor" {
			t.Errorf("cursor %q: %v", bad, err)
		}
	}
	if _, err := s.ConnectorRuns(ctx, wsB, wb, wbcrud.ConnectorRunsParams{Limit: 5}); !errors.Is(err, wbcrud.ErrNotFound) {
		t.Fatalf("other workspace: %v", err)
	}
	// A run of another workbook never shows up, and a payload with NULL query/cursor reads as {}.
	one, err := s.ConnectorRuns(ctx, wsA, other, wbcrud.ConnectorRunsParams{Limit: 5})
	if err != nil || !reflect.DeepEqual(ids(one), []string{"run-other"}) {
		t.Fatalf("other workbook runs: %v %v", ids(one), err)
	}
	b, _ := one.MarshalJSON()
	if !strings.Contains(string(b), `"query":{},"cursor":{}`) {
		t.Fatalf("null query/cursor must read as {}: %s", b)
	}
}
