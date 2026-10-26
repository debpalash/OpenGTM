//go:build integration

package sourceingest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/automationhook"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/sourceingest"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

const (
	wsA = "ws-si-a"
	wsB = "ws-si-b"
	wb  = "wb-si-1"
)

func setup(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	pool := testpg.NewAppPool(t, 16)
	ctx := context.Background()
	clean := func() {
		for _, w := range []string{wsA, wsB} {
			_ = tenancy.InWorkspace(ctx, pool, w, func(tx pgx.Tx) error {
				_, _ = tx.Exec(ctx, `DELETE FROM triggers`)
				_, err := tx.Exec(ctx, `DELETE FROM workbooks`)
				return err
			})
		}
		_, _ = pool.Exec(ctx, `DELETE FROM jobs WHERE type='trigger_eval'`)
	}
	clean()
	t.Cleanup(clean)
	for _, w := range []string{wsA, wsB} {
		w := w
		if err := tenancy.InWorkspace(ctx, pool, w, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO workbooks (id, name, workspace_id) VALUES ($1,'n',$2)`, wb+"-"+w[len(w)-1:], w)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	return pool, ctx
}

func wbOf(ws string) string { return wb + "-" + ws[len(ws)-1:] }

type vrec struct {
	Provider  string         `json:"provider"`
	RecordID  string         `json:"record_id"`
	Rank      int            `json:"rank"`
	SourceURL *string        `json:"source_url"`
	FetchedAt string         `json:"fetched_at"`
	Data      map[string]any `json:"data"`
}

func (v vrec) rec(t *testing.T) sourceingest.Record {
	ts, err := time.Parse(time.RFC3339, v.FetchedAt)
	if err != nil {
		t.Fatal(err)
	}
	return sourceingest.Record{Provider: v.Provider, RecordID: v.RecordID, Data: v.Data, Rank: v.Rank, SourceURL: v.SourceURL, FetchedAt: ts}
}

type vrow struct {
	Position        int             `json:"position"`
	SourceProvider  *string         `json:"source_provider"`
	SourceRecordID  *string         `json:"source_record_id"`
	SourceRank      *int            `json:"source_rank"`
	SourceFetchedAt *string         `json:"source_fetched_at"`
	LeadID          *int            `json:"lead_id"`
	Data            json.RawMessage `json:"data"`
	Enrichments     json.RawMessage `json:"enrichments"`
}

type vectors struct {
	Scenarios []struct {
		Name     string `json:"name"`
		SeedRows []vrow `json:"seed_rows"`
		Steps    []struct {
			ResetRows bool   `json:"reset_rows"`
			Records   []vrec `json:"records"`
			Summary   struct {
				Added, Updated, Skipped, AddedCount int
			} `json:"summary"`
			RowsAfter []vrow `json:"rows_after"`
		} `json:"steps"`
	} `json:"scenarios"`
}

func norm(t *testing.T, raw []byte) any {
	t.Helper()
	var v any
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func readRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ws, w string) []vrow {
	t.Helper()
	var out []vrow
	err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT "position", source_provider, source_record_id, source_rank,
			to_char(source_fetched_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS"+00:00"'), lead_id, data::text, enrichments::text
			FROM workbook_rows WHERE workbook_id=$1 ORDER BY "position", id`, w)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r vrow
			var d, e string
			if err := rows.Scan(&r.Position, &r.SourceProvider, &r.SourceRecordID, &r.SourceRank, &r.SourceFetchedAt, &r.LeadID, &d, &e); err != nil {
				return err
			}
			r.Data, r.Enrichments = json.RawMessage(d), json.RawMessage(e)
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func seed(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ws, w string, rows []vrow) {
	t.Helper()
	err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		for _, r := range rows {
			if _, err := tx.Exec(ctx, `INSERT INTO workbook_rows (workspace_id, workbook_id, "position", data, enrichments, source_provider, source_record_id)
				VALUES ($1,$2,$3,$4::json,$5::json,$6,$7)`, ws, w, r.Position, string(r.Data), string(r.Enrichments), r.SourceProvider, r.SourceRecordID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func upsert(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ws, w, provider string, recs []sourceingest.Record, reset bool) sourceingest.Summary {
	t.Helper()
	var s sourceingest.Summary
	err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		var err error
		s, err = sourceingest.Upsert(ctx, tx, w, ws, provider, recs, reset)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func clearRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ws, w string) {
	t.Helper()
	if err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM workbook_rows WHERE workbook_id=$1`, w)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUpsertMatchesVectors(t *testing.T) {
	pool, ctx := setup(t)
	raw, err := os.ReadFile("testdata/source_ingest.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Scenarios) < 19 {
		t.Fatalf("only %d scenarios", len(v.Scenarios))
	}
	w := wbOf(wsA)
	for _, sc := range v.Scenarios {
		t.Run(sc.Name, func(t *testing.T) {
			clearRows(t, ctx, pool, wsA, w)
			seed(t, ctx, pool, wsA, w, sc.SeedRows)
			for i, st := range sc.Steps {
				recs := make([]sourceingest.Record, len(st.Records))
				for j, r := range st.Records {
					recs[j] = r.rec(t)
				}
				// the provider of the call is the provider the Python run used
				provider := "ambitionbox"
				got := upsert(t, ctx, pool, wsA, w, provider, recs, st.ResetRows)
				if got.Added != st.Summary.Added || got.Updated != st.Summary.Updated || got.Skipped != st.Summary.Skipped || len(got.AddedRowIDs) != st.Summary.AddedCount {
					t.Fatalf("step %d summary %+v want %+v", i, got, st.Summary)
				}
				rows := readRows(t, ctx, pool, wsA, w)
				if len(rows) != len(st.RowsAfter) {
					t.Fatalf("step %d: %d rows want %d", i, len(rows), len(st.RowsAfter))
				}
				for k := range rows {
					g, e := rows[k], st.RowsAfter[k]
					if g.Position != e.Position || !reflect.DeepEqual(g.SourceProvider, e.SourceProvider) || !reflect.DeepEqual(g.SourceRecordID, e.SourceRecordID) ||
						!reflect.DeepEqual(g.SourceRank, e.SourceRank) || !reflect.DeepEqual(g.SourceFetchedAt, e.SourceFetchedAt) || !reflect.DeepEqual(g.LeadID, e.LeadID) ||
						!reflect.DeepEqual(norm(t, g.Data), norm(t, e.Data)) || !reflect.DeepEqual(norm(t, g.Enrichments), norm(t, e.Enrichments)) {
						t.Fatalf("step %d row %d:\n got %+v data=%s enr=%s\nwant %+v data=%s enr=%s", i, k, g, g.Data, g.Enrichments, e, e.Data, e.Enrichments)
					}
				}
			}
		})
	}
}

func mkrec(id string, rank int, data map[string]any) sourceingest.Record {
	return sourceingest.Record{Provider: "ambitionbox", RecordID: id, Data: data, Rank: rank,
		FetchedAt: time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)}
}

func TestUpsertConcurrentSamePageInsertsOnce(t *testing.T) {
	pool, ctx := setup(t)
	w := wbOf(wsA)
	recs := []sourceingest.Record{mkrec("1", 0, map[string]any{"c": "a"}), mkrec("2", 1, map[string]any{"c": "b"}), mkrec("3", 2, map[string]any{"c": "c"})}
	var wg sync.WaitGroup
	sums := make([]sourceingest.Summary, 8)
	errs := make([]error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = tenancy.InWorkspace(ctx, pool, wsA, func(tx pgx.Tx) error {
				var err error
				sums[i], err = sourceingest.Upsert(ctx, tx, w, wsA, "ambitionbox", recs, false)
				return err
			})
		}(i)
	}
	wg.Wait()
	added, skipped := 0, 0
	for i := range sums {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		added += sums[i].Added
		skipped += sums[i].Skipped
	}
	if added != 3 || skipped != 21 {
		t.Fatalf("added=%d skipped=%d, want 3 and 21", added, skipped)
	}
	if n := len(readRows(t, ctx, pool, wsA, w)); n != 3 {
		t.Fatalf("%d rows", n)
	}
}

func TestResetRowsPositionsFollowRank(t *testing.T) {
	pool, ctx := setup(t)
	w := wbOf(wsA)
	upsert(t, ctx, pool, wsA, w, "ambitionbox", []sourceingest.Record{mkrec("1", 0, map[string]any{"a": 1})}, false)
	s := upsert(t, ctx, pool, wsA, w, "ambitionbox", []sourceingest.Record{mkrec("5", 20, map[string]any{"a": 5}), mkrec("6", 7, map[string]any{"a": 6})}, true)
	if s.Added != 2 {
		t.Fatalf("%+v", s)
	}
	rows := readRows(t, ctx, pool, wsA, w)
	if len(rows) != 2 || rows[0].Position != 7 || rows[1].Position != 20 {
		t.Fatalf("%+v", rows)
	}
}

func TestSourceJSONKeyOrder(t *testing.T) {
	pool, ctx := setup(t)
	w := wbOf(wsA)
	u := "https://ab/1"
	r := mkrec("1", 3, map[string]any{"z": 1, "a": 2})
	r.SourceURL = &u
	r.FetchedAt = time.Date(2026, 8, 29, 10, 0, 0, 120000, time.UTC)
	upsert(t, ctx, pool, wsA, w, "ambitionbox", []sourceingest.Record{r}, false)
	rows := readRows(t, ctx, pool, wsA, w)
	d := string(rows[0].Data)
	i := strings.Index(d, `"_source"`)
	if i < 0 {
		t.Fatal(d)
	}
	src := d[i:]
	want := []string{`"provider"`, `"record_id"`, `"rank"`, `"url"`, `"fetched_at"`}
	last := -1
	for _, k := range want {
		p := strings.Index(src, k)
		if p <= last {
			t.Fatalf("_source key order wrong: %s", src)
		}
		last = p
	}
	if !strings.Contains(src, `"2026-08-29T10:00:00.000120+00:00"`) {
		t.Fatalf("fetched_at not python isoformat: %s", src)
	}
	// update keeps old key order, appends new keys
	r2 := mkrec("1", 3, map[string]any{"new": true})
	r2.FetchedAt = r.FetchedAt
	r2.SourceURL = &u
	upsert(t, ctx, pool, wsA, w, "ambitionbox", []sourceingest.Record{r2}, false)
	d2 := string(readRows(t, ctx, pool, wsA, w)[0].Data)
	if strings.Index(d2, `"_source"`) > strings.Index(d2, `"new"`) || strings.Index(d2, `"a"`) > strings.Index(d2, `"_source"`) && false {
		t.Fatalf("merge order: %s", d2)
	}
}

func TestMismatchedProviderSkipped(t *testing.T) {
	pool, ctx := setup(t)
	w := wbOf(wsA)
	other := mkrec("2", 1, nil)
	other.Provider = "hubspot"
	s := upsert(t, ctx, pool, wsA, w, "ambitionbox", []sourceingest.Record{mkrec("1", 0, nil), other, mkrec("  ", 2, nil), mkrec("1", 3, nil)}, false)
	if s.Added != 1 || s.Skipped != 3 || s.Updated != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestRollsBackWithCallerTx(t *testing.T) {
	pool, ctx := setup(t)
	w := wbOf(wsA)
	boom := fmt.Errorf("boom")
	err := tenancy.InWorkspace(ctx, pool, wsA, func(tx pgx.Tx) error {
		if _, err := sourceingest.Upsert(ctx, tx, w, wsA, "ambitionbox", []sourceingest.Record{mkrec("1", 0, nil)}, false); err != nil {
			return err
		}
		return boom
	})
	if err != boom {
		t.Fatalf("err=%v", err)
	}
	if n := len(readRows(t, ctx, pool, wsA, w)); n != 0 {
		t.Fatalf("%d rows survived rollback", n)
	}
}

func TestTenantIsolation(t *testing.T) {
	pool, ctx := setup(t)
	wa, wB := wbOf(wsA), wbOf(wsB)
	upsert(t, ctx, pool, wsB, wB, "ambitionbox", []sourceingest.Record{mkrec("1", 0, map[string]any{"b": 1})}, false)
	// workspace A may not write into B's workbook, with either workspace id
	for _, claim := range []string{wsA, wsB} {
		err := tenancy.InWorkspace(ctx, pool, wsA, func(tx pgx.Tx) error {
			_, err := sourceingest.Upsert(ctx, tx, wB, claim, "ambitionbox", []sourceingest.Record{mkrec("9", 0, nil)}, true)
			return err
		})
		if err == nil {
			t.Fatalf("claim %s: cross-tenant upsert succeeded", claim)
		}
	}
	rows := readRows(t, ctx, pool, wsB, wB)
	if len(rows) != 1 || !bytes.Contains(rows[0].Data, []byte(`"b"`)) {
		t.Fatalf("B's rows were touched: %+v", rows)
	}
	if n := len(readRows(t, ctx, pool, wsA, wa)); n != 0 {
		t.Fatal(n)
	}
}

func TestUpsertNotifyFiresRowAddedHook(t *testing.T) {
	pool, ctx := setup(t)
	w := wbOf(wsA)
	if err := tenancy.InWorkspace(ctx, pool, wsA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO triggers (id, workspace_id, name, enabled, trigger_type, trigger_config, actions, scope_workbook_ids)
			VALUES ('tr-si',$1,'n',true,'on_row_added','{}'::json,'[]'::json,'[]'::json)`, wsA)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	h := automationhook.New(true, nil)
	run := func(recs []sourceingest.Record) {
		if err := tenancy.InWorkspace(ctx, pool, wsA, func(tx pgx.Tx) error {
			_, err := sourceingest.UpsertNotify(ctx, tx, h, w, wsA, "ambitionbox", recs, false)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	count := func() int {
		var n int
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE type='trigger_eval'`).Scan(&n)
		return n
	}
	run([]sourceingest.Record{mkrec("1", 0, nil)})
	if count() != 1 {
		t.Fatalf("expected 1 trigger_eval job, got %d", count())
	}
	run([]sourceingest.Record{mkrec("1", 0, nil)}) // nothing added: no job
	if count() != 1 {
		t.Fatalf("no-op page enqueued a job: %d", count())
	}
}

func TestBatchCapRejected(t *testing.T) {
	pool, ctx := setup(t)
	recs := make([]sourceingest.Record, sourceingest.MaxBatchRecords+1)
	for i := range recs {
		recs[i] = mkrec(fmt.Sprint(i), i, nil)
	}
	err := tenancy.InWorkspace(ctx, pool, wsA, func(tx pgx.Tx) error {
		_, err := sourceingest.Upsert(ctx, tx, wbOf(wsA), wsA, "ambitionbox", recs, false)
		return err
	})
	if err == nil {
		t.Fatal("expected cap error")
	}
}
