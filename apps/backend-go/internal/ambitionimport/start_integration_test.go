//go:build integration

package ambitionimport_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ambitionimport"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

func sp(s string) *string { return &s }

func orderedKeys(t *testing.T, raw string) []string {
	t.Helper()
	o, err := wbcrud.ParseObject([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return o.Keys()
}

func (e *env) text(ws, sql string, args ...any) string {
	e.t.Helper()
	var s string
	e.in(ws, func(tx pgx.Tx) error { return tx.QueryRow(e.ctx, sql, args...).Scan(&s) })
	return s
}

// TestStartMatchesVectors replays the recorded POST /imports outcomes
// (testdata/contract/ambitionbox/import_*.json) at the Start level.
func TestStartMatchesVectors(t *testing.T) {
	e := newEnv(t)
	e.seedWorkbook(wsA, "wb-alpha-1", "Manual", "manual", nil)
	e.seedWorkbook(wsA, "wb-alpha-ab", "AB", "ambitionbox", nil)
	cases := []struct {
		name          string
		p             ambitionimport.StartParams
		wantName      string
		wantLimit     int
		wantMaxPages  int
		wantWorkbook  string // "" = new
		wantReplace   bool
		wantIndustry  any
		wantRating    any
		wantSortBy    string
		wantDescIsNew string
	}{
		{"defaults", ambitionimport.StartParams{WorkspaceID: wsA, Name: "", SortBy: "popular", Limit: 100}, "AmbitionBox Companies", 100, 10, "", false, nil, nil, "popular", "Durable AmbitionBox company import"},
		{"blank name limit 1", ambitionimport.StartParams{WorkspaceID: wsA, Name: "   ", SortBy: "popular", Limit: 1}, "AmbitionBox Companies", 1, 6, "", false, nil, nil, "popular", "Durable AmbitionBox company import"},
		{"filters", ambitionimport.StartParams{WorkspaceID: wsA, Name: "  Banks India  ", Industry: sp("Banking"), Rating: sp("4"), SortBy: "rating", Limit: 45}, "Banks India", 45, 8, "", false, "Banking", "4", "rating", "Durable AmbitionBox company import for Banking"},
		{"existing ambitionbox renamed", ambitionimport.StartParams{WorkspaceID: wsA, Name: "Renamed AB", SortBy: "popular", Limit: 500, WorkbookID: sp("wb-alpha-ab")}, "Renamed AB", 500, 25, "wb-alpha-ab", false, nil, nil, "popular", ""},
		{"non ambitionbox with replace", ambitionimport.StartParams{WorkspaceID: wsA, Name: "", SortBy: "popular", Limit: 20, WorkbookID: sp("wb-alpha-1"), ReplaceExisting: true}, "Manual", 20, 6, "wb-alpha-1", true, nil, nil, "popular", ""},
		{"limit clamped low", ambitionimport.StartParams{WorkspaceID: wsA, SortBy: "popular", Limit: 0}, "AmbitionBox Companies", 1, 6, "", false, nil, nil, "popular", ""},
		{"limit clamped high", ambitionimport.StartParams{WorkspaceID: wsA, SortBy: "popular", Limit: 9999}, "AmbitionBox Companies", 500, 25, "", false, nil, nil, "popular", ""},
		{"limit 21", ambitionimport.StartParams{WorkspaceID: wsA, SortBy: "popular", Limit: 21}, "AmbitionBox Companies", 21, 7, "", false, nil, nil, "popular", ""},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			if c.p.WorkbookID != nil {
				// A finished earlier run must not block a new start.
				e.in(wsA, func(tx pgx.Tx) error {
					_, err := tx.Exec(e.ctx, `DELETE FROM connector_runs WHERE workbook_id=$1`, *c.p.WorkbookID)
					return err
				})
			}
			res, err := ambitionimport.Start(e.ctx, e.d, c.p)
			if err != nil {
				t.Fatal(err)
			}
			if res.Name != c.wantName || res.RequestedLimit != c.wantLimit || res.Status != "pending" || res.Source != "ambitionbox" {
				t.Fatalf("result %+v", res)
			}
			if c.wantWorkbook != "" && res.WorkbookID != c.wantWorkbook {
				t.Fatalf("workbook %s", res.WorkbookID)
			}
			if res.URL != "/workbooks/"+res.WorkbookID || res.JobID <= 0 || res.RunID == "" {
				t.Fatalf("result %+v", res)
			}
			// Run row.
			run := e.runView(wsA, res.RunID)
			assertEq(t, "query", run["query"], map[string]any{"industry": c.wantIndustry, "rating": c.wantRating, "sort_by": c.wantSortBy,
				"max_pages": c.wantMaxPages, "replace_existing": c.wantReplace})
			assertEq(t, "cursor", run["cursor"], map[string]any{"next_page": 1, "reset_done": false, "seen_record_ids": []any{}})
			if run["status"] != "pending" || run["connector"] != "ambitionbox" || run["requested_count"].(float64) != float64(c.wantLimit) ||
				run["started_at"] != false || run["target_met"] != false {
				t.Fatalf("run %+v", run)
			}
			// Key order of the stored JSON documents.
			qtxt := e.text(wsA, `SELECT query::text FROM connector_runs WHERE id=$1`, res.RunID)
			if got := strings.Join(orderedKeys(t, qtxt), ","); got != "industry,rating,sort_by,max_pages,replace_existing" {
				t.Fatalf("query key order %s", got)
			}
			ctxt := e.text(wsA, `SELECT cursor::text FROM connector_runs WHERE id=$1`, res.RunID)
			if got := strings.Join(orderedKeys(t, ctxt), ","); got != "next_page,reset_done,seen_record_ids" {
				t.Fatalf("cursor key order %s", got)
			}
			// Workbook.
			wbv, _ := e.wbView(wsA, res.WorkbookID, res.RunID)
			if wbv["status"] != "running" || wbv["source_type"] != "ambitionbox" || wbv["name"] != c.wantName {
				t.Fatalf("workbook %+v", wbv)
			}
			assertEq(t, "source_config", wbv["source_config"], map[string]any{"provider": "ambitionbox", "industry": c.wantIndustry, "rating": c.wantRating,
				"sort_by": c.wantSortBy, "max_pages": c.wantMaxPages, "replace_existing": c.wantReplace, "requested_limit": c.wantLimit, "active_run_id": "<run_id>"})
			sctxt := e.text(wsA, `SELECT source_config::text FROM workbooks WHERE id=$1`, res.WorkbookID)
			if got := strings.Join(orderedKeys(t, sctxt), ","); got != "provider,industry,rating,sort_by,max_pages,replace_existing,requested_limit,active_run_id" {
				t.Fatalf("source_config key order %s", got)
			}
			assertEq(t, "columns", wbv["columns_config_ids"], []string{"company", "industry", "ambitionbox_rating", "review_count", "company_size", "city", "jobs_count", "company_type", "is_verified", "ambitionbox_url"})
			colTxt := e.text(wsA, `SELECT columns_config::text FROM workbooks WHERE id=$1`, res.WorkbookID)
			if !contains(colTxt, `"id":"company","name":"Company","type":"lead_field","lead_field":"company","width":220`) &&
				!contains(colTxt, `"id": "company", "name": "Company", "type": "lead_field", "lead_field": "company", "width": 220`) {
				t.Fatalf("columns_config bytes %.200s", colTxt)
			}
			if c.wantDescIsNew != "" {
				if d := e.text(wsA, `SELECT description FROM workbooks WHERE id=$1`, res.WorkbookID); d != c.wantDescIsNew {
					t.Fatalf("description %q", d)
				}
			}
			// Job.
			var typ, exec, payload string
			var prio int
			if err := e.owner.QueryRow(e.ctx, `SELECT type, executor, priority, payload::text FROM jobs WHERE id=$1`, res.JobID).Scan(&typ, &exec, &prio, &payload); err != nil {
				t.Fatal(err)
			}
			if typ != "ambitionbox_import" || exec != "go" || prio != 2 {
				t.Fatalf("job %s %s %d", typ, exec, prio)
			}
			if got := strings.Join(orderedKeys(t, payload), ","); got != "workspace_id,workbook_id,run_id" {
				t.Fatalf("payload key order %s", got)
			}
		})
	}
}

func TestStartResultKeyOrder(t *testing.T) {
	e := newEnv(t)
	res, err := ambitionimport.Start(e.ctx, e.d, ambitionimport.StartParams{WorkspaceID: wsA, SortBy: "popular", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res)
	if got := strings.Join(orderedKeys(t, string(b)), ","); got != "workbook_id,name,run_id,job_id,status,requested_limit,source,url" {
		t.Fatalf("key order %s", got)
	}
}

func TestStartRowLocksAndRejectsActive(t *testing.T) {
	e := newEnv(t)
	e.seedWorkbook(wsA, "wb-lock", "AB", "ambitionbox", nil)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, active, other := 0, 0, 0
	var runIDs []string
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := ambitionimport.Start(e.ctx, e.d, ambitionimport.StartParams{WorkspaceID: wsA, SortBy: "popular", Limit: 10, WorkbookID: sp("wb-lock")})
			mu.Lock()
			defer mu.Unlock()
			var ar *ambitionimport.AlreadyRunningError
			switch {
			case err == nil:
				ok++
				runIDs = append(runIDs, res.RunID)
			case errors.As(err, &ar):
				active++
				if ar.RunID == "" || ar.Error() != fmt.Sprintf("AmbitionBox import %s is already active", ar.RunID) {
					t.Errorf("bad AlreadyRunningError %v", err)
				}
			default:
				other++
				t.Errorf("unexpected error %v", err)
			}
		}()
	}
	wg.Wait()
	if ok != 1 || active != 7 || other != 0 {
		t.Fatalf("ok=%d active=%d other=%d", ok, active, other)
	}
	var n int
	e.in(wsA, func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, `SELECT count(*) FROM connector_runs WHERE workbook_id='wb-lock'`).Scan(&n)
	})
	if n != 1 {
		t.Fatalf("%d runs", n)
	}
	var jobs int
	_ = e.owner.QueryRow(e.ctx, `SELECT count(*) FROM jobs WHERE type='ambitionbox_import'`).Scan(&jobs)
	if jobs != 1 {
		t.Fatalf("%d jobs", jobs)
	}
	// The seeded-pending-run case: the error names that run.
	_, err := ambitionimport.Start(e.ctx, e.d, ambitionimport.StartParams{WorkspaceID: wsA, SortBy: "popular", Limit: 10, WorkbookID: sp("wb-lock")})
	var ar *ambitionimport.AlreadyRunningError
	if !errors.As(err, &ar) || ar.RunID != runIDs[0] {
		t.Fatalf("got %v want run %s", err, runIDs[0])
	}
}

func TestStartUniqueIndexConflictReturnsActiveRun(t *testing.T) {
	// Defence in depth: even without the row lock the partial unique index
	// yields the same AlreadyRunningError.
	e := newEnv(t)
	e.seedWorkbook(wsA, "wb-uq", "AB", "ambitionbox", nil)
	e.in(wsA, func(tx pgx.Tx) error {
		_, err := tx.Exec(e.ctx, `INSERT INTO connector_runs (id, workspace_id, workbook_id, connector, status, query, cursor, requested_count,
			fetched_count, added_count, updated_count, skipped_count, pages_fetched, target_met, exhausted)
			VALUES ('run-seeded',$1,'wb-uq','ambitionbox','running','{}','{}',5,0,0,0,0,0,false,false)`, wsA)
		return err
	})
	_, err := ambitionimport.Start(e.ctx, e.d, ambitionimport.StartParams{WorkspaceID: wsA, SortBy: "popular", Limit: 10, WorkbookID: sp("wb-uq")})
	var ar *ambitionimport.AlreadyRunningError
	if !errors.As(err, &ar) || ar.RunID != "run-seeded" || ar.Error() != "AmbitionBox import run-seeded is already active" {
		t.Fatalf("got %v", err)
	}
}

func TestStartNonAmbitionBoxNeedsReplace(t *testing.T) {
	e := newEnv(t)
	e.seedWorkbook(wsA, "wb-manual", "Manual", "manual", [][3]any{{map[string]any{"a": 1}, nil, nil}})
	_, err := ambitionimport.Start(e.ctx, e.d, ambitionimport.StartParams{WorkspaceID: wsA, SortBy: "popular", Limit: 10, WorkbookID: sp("wb-manual")})
	if !errors.Is(err, ambitionimport.ErrNeedsReplace) {
		t.Fatalf("got %v", err)
	}
	if err.Error() != "existing non-AmbitionBox workbooks require replace_existing=true" {
		t.Fatalf("text %q", err)
	}
	// Nothing changed.
	if st := e.text(wsA, `SELECT source_type FROM workbooks WHERE id='wb-manual'`); st != "manual" {
		t.Fatalf("source_type %s", st)
	}
	var n int
	e.in(wsA, func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, `SELECT count(*) FROM connector_runs WHERE workbook_id='wb-manual'`).Scan(&n)
	})
	if n != 0 {
		t.Fatalf("%d runs", n)
	}
}

func TestStartForeignWorkbookNotFound(t *testing.T) {
	e := newEnv(t)
	e.seedWorkbook(wsB, "wb-foreign", "B", "ambitionbox", nil)
	for _, id := range []string{"wb-foreign", "wb-does-not-exist"} {
		_, err := ambitionimport.Start(e.ctx, e.d, ambitionimport.StartParams{WorkspaceID: wsA, SortBy: "popular", Limit: 10, WorkbookID: sp(id)})
		if !errors.Is(err, ambitionimport.ErrWorkbookNotFound) {
			t.Fatalf("%s: got %v", id, err)
		}
		if err.Error() != "workbook not found in the active workspace" {
			t.Fatalf("text %q", err)
		}
	}
	var n int
	e.in(wsB, func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, `SELECT count(*) FROM connector_runs`).Scan(&n)
	})
	if n != 0 {
		t.Fatal("foreign workspace was touched")
	}
}

func TestStartEnqueuesWithExecutorFlag(t *testing.T) {
	e := newEnv(t)
	for _, want := range []string{"go"} {
		want := want
		e.d.Executor = func(ws string) string {
			if ws != wsA {
				t.Errorf("executor asked for %s", ws)
			}
			return want
		}
		res, err := ambitionimport.Start(e.ctx, e.d, ambitionimport.StartParams{WorkspaceID: wsA, SortBy: "popular", Limit: 5})
		if err != nil {
			t.Fatal(err)
		}
		var exec, status string
		var maxRetries, retries int
		if err := e.owner.QueryRow(e.ctx, `SELECT executor, status, max_retries, retry_count FROM jobs WHERE id=$1`, res.JobID).Scan(&exec, &status, &maxRetries, &retries); err != nil {
			t.Fatal(err)
		}
		if exec != want || status != "pending" || maxRetries != 3 || retries != 0 {
			t.Fatalf("job %s %s %d %d", exec, status, maxRetries, retries)
		}
	}
}

// The flags are retired (plan 15 cutover): every ambitionbox_import job is go.
func TestExecutorFor(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	cases := []struct {
		name string
		env  map[string]string
		ws   string
		want string
	}{
		{"all off", nil, "w1", "go"},
		{"flag only", map[string]string{"GO_EXECUTOR_AMBITIONBOX": "true"}, "w1", "go"},
		{"enabled and flag, no cohort", map[string]string{"GO_EXECUTOR_ENABLED": "true", "GO_EXECUTOR_AMBITIONBOX": "true"}, "w1", "go"},
		{"cohort member", map[string]string{"GO_EXECUTOR_ENABLED": "true", "GO_EXECUTOR_AMBITIONBOX": "1", "GO_EXECUTOR_WORKSPACES": "w0, w1 ,w2"}, "w1", "go"},
		{"cohort non member", map[string]string{"GO_EXECUTOR_ENABLED": "true", "GO_EXECUTOR_AMBITIONBOX": "true", "GO_EXECUTOR_WORKSPACES": "w0"}, "w1", "go"},
		{"ambitionbox flag off", map[string]string{"GO_EXECUTOR_ENABLED": "true", "GO_EXECUTOR_WORKSPACES": "w1"}, "w1", "go"},
	}
	for _, c := range cases {
		if got := ambitionimport.ExecutorFor(env(c.env))(c.ws); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
}

func TestStartRollsBackJobOnFailure(t *testing.T) {
	e := newEnv(t)
	for _, s := range []string{
		`CREATE OR REPLACE FUNCTION ai_test_fail_job() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'job insert refused'; END $$ LANGUAGE plpgsql`,
		`CREATE TRIGGER ai_test_fail_job BEFORE INSERT ON jobs FOR EACH ROW WHEN (NEW.type = 'ambitionbox_import') EXECUTE FUNCTION ai_test_fail_job()`,
	} {
		if _, err := e.owner.Exec(e.ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	e.seedWorkbook(wsA, "wb-rb", "AB", "ambitionbox", nil)
	for _, wbid := range []*string{nil, sp("wb-rb")} {
		_, err := ambitionimport.Start(e.ctx, e.d, ambitionimport.StartParams{WorkspaceID: wsA, Name: "Rolled", SortBy: "popular", Limit: 10, WorkbookID: wbid})
		if err == nil || !contains(err.Error(), "job insert refused") {
			t.Fatalf("got %v", err)
		}
	}
	var runs, newWb int
	e.in(wsA, func(tx pgx.Tx) error {
		if err := tx.QueryRow(e.ctx, `SELECT count(*) FROM connector_runs`).Scan(&runs); err != nil {
			return err
		}
		return tx.QueryRow(e.ctx, `SELECT count(*) FROM workbooks WHERE name='Rolled'`).Scan(&newWb)
	})
	if runs != 0 || newWb != 0 {
		t.Fatalf("not rolled back: runs=%d workbooks=%d", runs, newWb)
	}
	if st := e.text(wsA, `SELECT status FROM workbooks WHERE id='wb-rb'`); st != "draft" {
		t.Fatalf("workbook status %s", st)
	}
}
