//go:build integration

package ambitionimport_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ambitionimport"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/progress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
)

type vecFile struct {
	Scenarios []struct {
		Name  string `json:"name"`
		Start struct {
			RequestedLimit  int            `json:"requested_limit"`
			ReplaceExisting bool           `json:"replace_existing"`
			Query           map[string]any `json:"query"`
			Existing        *struct {
				Name       string   `json:"name"`
				SourceType string   `json:"source_type"`
				Rows       [][3]any `json:"rows"`
			} `json:"existing_workbook"`
			MaxPages int `json:"max_pages"`
		} `json:"start"`
		FetchCalls []map[string]any `json:"fetch_calls"`
		Steps      []struct {
			Raised       string         `json:"raised"`
			Run          map[string]any `json:"run"`
			Workbook     map[string]any `json:"workbook"`
			Rows         any            `json:"rows"`
			Events       []any          `json:"events"`
			RowAddedEmit []int          `json:"row_added_emits"`
		} `json:"steps"`
	} `json:"scenarios"`
	Reconcile []struct {
		Name            string         `json:"name"`
		WillRetry       bool           `json:"will_retry"`
		InitialStatus   string         `json:"initial_status"`
		PayloadOverride map[string]any `json:"payload_override"`
		ErrorLength     int            `json:"error_length"`
		Run             map[string]any `json:"run"`
		Workbook        map[string]any `json:"workbook"`
		Rows            any            `json:"rows"`
	} `json:"reconcile"`
}

func loadVectors(t *testing.T) vecFile {
	t.Helper()
	raw, err := os.ReadFile("testdata/ambitionbox_cursor.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vecFile
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// scenarioScript is the page script of the Python generator (gen_ambitionimport_cursor).
func scenarioScript(name string, e *env) {
	pg := func(p int, idl []int, tp, st *int, hm *bool) { e.fetch.pages[p] = pageOf(p, idl, tp, st, hm) }
	two := func() { pg(1, ids(1, 20), ip(5), ip(100), nil); pg(2, ids(21, 40), ip(5), ip(100), nil) }
	tru := true
	switch name {
	case "target_met_on_page_two", "replace_existing_resets_rows_on_page_one_only", "existing_ambitionbox_workbook_keeps_rows_without_replace":
		two()
	case "exhausted_before_target_is_partial":
		pg(1, ids(1, 20), ip(2), ip(35), nil)
		pg(2, ids(21, 35), ip(2), ip(35), nil)
	case "target_met_on_first_page", "rerun_after_completion_returns_stored_state":
		pg(1, ids(1, 20), ip(5), ip(100), nil)
	case "max_pages_budget_reached_partial":
		for p := 1; p <= 6; p++ {
			pg(p, []int{p}, nil, nil, &tru)
		}
	case "filters_forwarded_to_each_page":
		pg(1, ids(1, 20), ip(9), ip(99), nil)
		pg(2, ids(21, 40), ip(9), ip(99), nil)
	case "seen_ids_carry_over_and_are_skipped":
		pg(1, ids(1, 20), ip(5), ip(100), nil)
		pg(2, ids(15, 34), ip(5), ip(100), nil)
	case "crash_on_page_two_then_resume":
		two()
		e.fetch.fail[2] = "AmbitionBox request failed after 3 attempts: boom"
	case "crash_on_page_one_keeps_cursor_at_one":
		pg(1, ids(1, 20), ip(5), ip(100), nil)
		e.fetch.fail[1] = strings.Repeat("x", 1200)
	case "empty_first_page_is_exhausted_partial":
		pg(1, nil, ip(0), ip(0), nil)
	case "cursor_advanced_by_another_worker_refetches_next_page":
		two()
	}
}

func (e *env) bumpCursor(runID string) {
	_, err := e.owner.Exec(e.ctx, `UPDATE connector_runs SET cursor='{"next_page": 2, "reset_done": false, "seen_record_ids": ["1","2","3","4","5","6","7","8","9","10","11","12","13","14","15","16","17","18","19","20"]}'::json,
		pages_fetched=1, fetched_count=20 WHERE id=$1`, runID)
	if err != nil {
		e.t.Error(err)
	}
}

func TestHandleCursorSequenceMatchesVectors(t *testing.T) {
	vec := loadVectors(t)
	if len(vec.Scenarios) != 13 {
		t.Fatalf("expected 13 recorded scenarios, got %d", len(vec.Scenarios))
	}
	for _, sc := range vec.Scenarios {
		sc := sc
		t.Run(sc.Name, func(t *testing.T) {
			e := newEnv(t)
			scenarioScript(sc.Name, e)
			p := ambitionimport.StartParams{WorkspaceID: wsA, Name: "Vec Import", SortBy: "popular", Limit: sc.Start.RequestedLimit, ReplaceExisting: sc.Start.ReplaceExisting}
			if s, ok := sc.Start.Query["industry"].(string); ok {
				p.Industry = &s
			}
			if s, ok := sc.Start.Query["rating"].(string); ok {
				p.Rating = &s
			}
			if s, ok := sc.Start.Query["sort_by"].(string); ok {
				p.SortBy = s
			}
			if x := sc.Start.Existing; x != nil {
				e.seedWorkbook(wsA, "vec-existing", x.Name, x.SourceType, x.Rows)
				p.WorkbookID = sp("vec-existing")
			}
			res, err := ambitionimport.Start(e.ctx, e.d, p)
			if err != nil {
				t.Fatal(err)
			}
			if sc.Name == "cursor_advanced_by_another_worker_refetches_next_page" {
				e.fetch.before[1] = func() { e.bumpCursor(res.RunID) }
			}
			c := e.claimFor(wsA, res.WorkbookID, res.RunID)
			for i, st := range sc.Steps {
				e.pub.msgs, e.hook.emits = nil, nil
				err := ambitionimport.Handle(e.ctx, e.d, c)
				if st.Raised != "" {
					want := st.Raised[strings.Index(st.Raised, ": ")+2:]
					if err == nil || err.Error() != want {
						t.Fatalf("step %d: error %v want %q", i+1, err, want)
					}
				} else if err != nil {
					t.Fatalf("step %d: %v", i+1, err)
				}
				assertEq(t, fmt.Sprintf("step %d run", i+1), e.runView(wsA, res.RunID), st.Run)
				wbv, rows := e.wbView(wsA, res.WorkbookID, res.RunID)
				assertEq(t, fmt.Sprintf("step %d workbook", i+1), wbv, st.Workbook)
				assertEq(t, fmt.Sprintf("step %d rows", i+1), rows, st.Rows)
				evs := []any{}
				for _, m := range e.pub.msgs {
					m["runId"] = "<run_id>"
					evs = append(evs, m)
				}
				assertEq(t, fmt.Sprintf("step %d events", i+1), evs, st.Events)
				var emits []int
				for _, ids := range e.hook.emits {
					emits = append(emits, len(ids))
				}
				if emits == nil {
					emits = []int{}
				}
				assertEq(t, fmt.Sprintf("step %d row_added_emits", i+1), emits, st.RowAddedEmit)
			}
			var calls []any
			for _, pc := range e.fetch.calls {
				var ind any
				if len(pc.Industry) > 0 {
					ind = pc.Industry
				}
				var rat any
				if pc.Rating != "" {
					rat = pc.Rating
				}
				calls = append(calls, map[string]any{"page": pc.Page, "industry": ind, "sort_by": pc.SortBy, "rating": rat})
			}
			assertEq(t, "fetch calls", calls, sc.FetchCalls)
		})
	}
}

func TestHandleRejectsBadPayloadAndUnknownRun(t *testing.T) {
	e := newEnv(t)
	for _, pl := range []string{`{}`, `{"workspace_id":"` + wsA + `","workbook_id":"w"}`, `{"workspace_id":" ","workbook_id":"w","run_id":"r"}`} {
		err := ambitionimport.Handle(e.ctx, e.d, e.claimPayload(wsA, pl))
		if err == nil || err.Error() != "ambitionbox_import requires workspace_id, workbook_id, run_id" {
			t.Fatalf("%s: %v", pl, err)
		}
	}
	err := ambitionimport.Handle(e.ctx, e.d, e.claimPayload(wsA, `{"workspace_id":"`+wsA+`","workbook_id":"nope","run_id":"nope"}`))
	var pe *queue.PermanentError
	if err == nil || !errors.As(err, &pe) || err.Error() != "connector run or workbook not found" {
		t.Fatalf("got %v", err)
	}
	if len(e.fetch.callPages()) != 0 {
		t.Fatal("fetched for an unknown run")
	}
}

func startRun(e *env, limit int) (ambitionimport.StartResult, *queue.Claim) {
	e.t.Helper()
	res, err := ambitionimport.Start(e.ctx, e.d, ambitionimport.StartParams{WorkspaceID: wsA, Name: "T", SortBy: "popular", Limit: limit})
	if err != nil {
		e.t.Fatal(err)
	}
	return res, e.claimFor(wsA, res.WorkbookID, res.RunID)
}

func countRows(e *env, wb string) (n int) {
	e.in(wsA, func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, `SELECT count(*) FROM workbook_rows WHERE workbook_id=$1`, wb).Scan(&n)
	})
	return
}

// A failure after the page was upserted (the row-added hook errors) rolls the
// page, its rows and its cursor back together; the next attempt re-fetches
// only that page.
func TestHandleCrashBeforeCommitRefetchesPage(t *testing.T) {
	e := newEnv(t)
	e.fetch.pages[1] = pageOf(1, ids(1, 20), ip(5), ip(100), nil)
	e.fetch.pages[2] = pageOf(2, ids(21, 40), ip(5), ip(100), nil)
	e.hook.failOn = 2 // second hook call = page 2
	res, c := startRun(e, 30)
	err := ambitionimport.Handle(e.ctx, e.d, c)
	if err == nil || !contains(err.Error(), "hook boom") {
		t.Fatalf("got %v", err)
	}
	if n := countRows(e, res.WorkbookID); n != 20 {
		t.Fatalf("%d rows after rollback, want 20", n)
	}
	run := e.runView(wsA, res.RunID)
	assertEq(t, "cursor", run["cursor"], map[string]any{"next_page": 2, "reset_done": false, "seen_record_ids": norm(seen(1, 20))})
	if run["status"] != "retrying" || run["pages_fetched"].(float64) != 1 || run["completed_at"] != false {
		t.Fatalf("run %+v", run)
	}
	e.fetch.calls = nil
	if err := ambitionimport.Handle(e.ctx, e.d, c); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(e.fetch.callPages()); got != "[2]" {
		t.Fatalf("refetched pages %s, want only [2]", got)
	}
	if n := countRows(e, res.WorkbookID); n != 30 {
		t.Fatalf("%d rows", n)
	}
	if run := e.runView(wsA, res.RunID); run["status"] != "complete" || run["pages_fetched"].(float64) != 2 || run["added_count"].(float64) != 30 {
		t.Fatalf("run %+v", run)
	}
}

func seen(a, b int) []string {
	var out []string
	for _, i := range ids(a, b) {
		out = append(out, fmt.Sprint(i))
	}
	// sorted as strings, like Python sorted()
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func TestHandleTwoWorkersAdvanceOncePerPage(t *testing.T) {
	e := newEnv(t)
	e.fetch.pages[1] = pageOf(1, ids(1, 20), ip(5), ip(100), nil)
	e.fetch.pages[2] = pageOf(2, ids(21, 40), ip(5), ip(100), nil)
	res, c1 := startRun(e, 30)
	c2 := e.claimFor(wsA, res.WorkbookID, res.RunID)
	// Both workers hold page 1 before either commits it.
	var barrier sync.WaitGroup
	barrier.Add(2)
	var once sync.Once
	var n int
	var mu sync.Mutex
	e.fetch.all = func(page int) {
		mu.Lock()
		n++
		first := n <= 2 && page == 1
		mu.Unlock()
		if first {
			barrier.Done()
			done := make(chan struct{})
			go func() { barrier.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
			}
			once.Do(func() {})
		}
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, c := range []*queue.Claim{c1, c2} {
		wg.Add(1)
		go func() { defer wg.Done(); errs[i] = ambitionimport.Handle(e.ctx, e.d, c) }()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	run := e.runView(wsA, res.RunID)
	if run["status"] != "complete" || run["pages_fetched"].(float64) != 2 || run["added_count"].(float64) != 30 || run["updated_count"].(float64) != 0 {
		t.Fatalf("run %+v", run)
	}
	if n := countRows(e, res.WorkbookID); n != 30 {
		t.Fatalf("%d rows", n)
	}
}

func TestHandleResumesPythonStartedRun(t *testing.T) {
	e := newEnv(t)
	e.fetch.pages[2] = pageOf(2, ids(21, 40), ip(5), ip(100), nil)
	var rows [][3]any
	for _, i := range ids(1, 20) {
		rows = append(rows, [3]any{map[string]any{"company": fmt.Sprintf("Co %d", i)}, "ambitionbox", fmt.Sprint(i)})
	}
	e.seedWorkbook(wsA, "wb-py", "Py", "ambitionbox", rows)
	e.in(wsA, func(tx pgx.Tx) error {
		// Exactly what Python's start + first page leave behind (retrying after a crash).
		if _, err := tx.Exec(e.ctx, `UPDATE workbooks SET status='running', source_config='{"provider": "ambitionbox", "industry": null, "rating": null, "sort_by": "popular", "max_pages": 7, "replace_existing": false, "requested_limit": 30, "active_run_id": "run-py"}'::json WHERE id='wb-py'`); err != nil {
			return err
		}
		cur, _ := json.Marshal(map[string]any{"next_page": 2, "reset_done": false, "seen_record_ids": seen(1, 20)})
		_, err := tx.Exec(e.ctx, `INSERT INTO connector_runs (id, workspace_id, workbook_id, connector, status, query, cursor, requested_count,
			fetched_count, added_count, updated_count, skipped_count, pages_fetched, source_total, target_met, exhausted, error, started_at)
			VALUES ('run-py',$1,'wb-py','ambitionbox','retrying','{"industry": null, "rating": null, "sort_by": "popular", "max_pages": 7, "replace_existing": false}'::json,
			$2::json,30,20,20,0,0,1,100,false,false,'boom',now())`, wsA, string(cur))
		return err
	})
	if err := ambitionimport.Handle(e.ctx, e.d, e.claimFor(wsA, "wb-py", "run-py")); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(e.fetch.callPages()); got != "[2]" {
		t.Fatalf("fetched %s", got)
	}
	run := e.runView(wsA, "run-py")
	if run["status"] != "complete" || run["pages_fetched"].(float64) != 2 || run["added_count"].(float64) != 30 ||
		run["fetched_count"].(float64) != 30 || run["target_met"] != true || run["error"] != nil {
		t.Fatalf("run %+v", run)
	}
	if n := countRows(e, "wb-py"); n != 30 {
		t.Fatalf("%d rows", n)
	}
}

func TestHandleRowsAddedHookAfterCommit(t *testing.T) {
	e := newEnv(t)
	e.fetch.pages[1] = pageOf(1, ids(1, 20), ip(5), ip(100), nil)
	e.fetch.pages[2] = pageOf(2, ids(15, 34), ip(5), ip(100), nil) // 15..20 already seen: 14 new
	res, c := startRun(e, 34)
	e.hook.inTxSee = func(ctx context.Context, tx pgx.Tx, rowIDs []int64) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM workbook_rows WHERE id = ANY($1)`, rowIDs).Scan(&n); err != nil {
			return err
		}
		if n != len(rowIDs) {
			return fmt.Errorf("hook saw %d of %d rows", n, len(rowIDs))
		}
		return nil
	}
	if err := ambitionimport.Handle(e.ctx, e.d, c); err != nil {
		t.Fatal(err)
	}
	if len(e.hook.emits) != 2 || len(e.hook.emits[0]) != 20 || len(e.hook.emits[1]) != 14 {
		t.Fatalf("emits %v", e.hook.emits)
	}
	// Python re-seen records are never re-emitted.
	var distinct int
	e.in(wsA, func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, `SELECT count(DISTINCT id) FROM workbook_rows WHERE workbook_id=$1`, res.WorkbookID).Scan(&distinct)
	})
	if distinct != 34 {
		t.Fatalf("%d rows", distinct)
	}
}

func TestHandleBroadcastsSourcePage(t *testing.T) {
	e := newEnv(t)
	e.fetch.pages[1] = pageOf(1, ids(1, 20), ip(5), ip(100), nil)
	res, c := startRun(e, 5)
	if err := ambitionimport.Handle(e.ctx, e.d, c); err != nil {
		t.Fatal(err)
	}
	assertEq(t, "events", e.pub.msgs, []any{map[string]any{"type": "source_page", "connector": "ambitionbox", "runId": res.RunID, "page": 1, "added": 5, "total": 5, "done": true}})
}

func TestHandleBroadcastsToRedis(t *testing.T) {
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL not set")
	}
	e := newEnv(t)
	pub, err := progress.NewRedis(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()
	e.d.Progress = pub
	e.fetch.pages[1] = pageOf(1, ids(1, 20), ip(5), ip(100), nil)
	res, c := startRun(e, 5)
	opt, _ := redis.ParseURL(url)
	rc := redis.NewClient(opt)
	defer rc.Close()
	sub := rc.Subscribe(e.ctx, "workbook:"+res.WorkbookID)
	defer sub.Close()
	if _, err := sub.Receive(e.ctx); err != nil {
		t.Fatal(err)
	}
	if err := ambitionimport.Handle(e.ctx, e.d, c); err != nil {
		t.Fatal(err)
	}
	rctx, cancel := context.WithTimeout(e.ctx, 5*time.Second)
	defer cancel()
	msg, err := sub.ReceiveMessage(rctx)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal([]byte(msg.Payload), &got)
	if got["type"] != "source_page" || got["connector"] != "ambitionbox" || got["runId"] != res.RunID || got["done"] != true {
		t.Fatalf("event %s", msg.Payload)
	}
}

func TestHandleRedisDownDoesNotFail(t *testing.T) {
	e := newEnv(t)
	pub, err := progress.NewRedis("redis://127.0.0.1:1/0", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()
	e.d.Progress = pub
	e.fetch.pages[1] = pageOf(1, ids(1, 20), ip(5), ip(100), nil)
	res, c := startRun(e, 5)
	if err := ambitionimport.Handle(e.ctx, e.d, c); err != nil {
		t.Fatalf("redis outage failed the job: %v", err)
	}
	if run := e.runView(wsA, res.RunID); run["status"] != "complete" {
		t.Fatalf("run %+v", run)
	}
}

func TestHandleErrorLeavesRetrying(t *testing.T) {
	e := newEnv(t)
	e.fetch.pages[1] = pageOf(1, ids(1, 20), ip(5), ip(100), nil)
	e.fetch.fail[1] = strings.Repeat("é", 1200)
	res, c := startRun(e, 5)
	err := ambitionimport.Handle(e.ctx, e.d, c)
	if err == nil || err.Error() != strings.Repeat("é", 1200) {
		t.Fatalf("returned error %v", err)
	}
	run := e.runView(wsA, res.RunID)
	if run["status"] != "retrying" || run["completed_at"] != false {
		t.Fatalf("run %+v", run)
	}
	if s, _ := run["error"].(string); len([]rune(s)) != 1000 {
		t.Fatalf("error len %d", len([]rune(s)))
	}
	wbv, _ := e.wbView(wsA, res.WorkbookID, res.RunID)
	sc := wbv["source_config"].(map[string]any)
	if wbv["status"] != "running" || sc["run_status"] != "retrying" || sc["target_met"] != false || len([]rune(sc["last_error"].(string))) != 1000 {
		t.Fatalf("workbook %+v", wbv)
	}
	// An active run still blocks a new start (retrying counts as active).
	_, err = ambitionimport.Start(e.ctx, e.d, ambitionimport.StartParams{WorkspaceID: wsA, SortBy: "popular", Limit: 5, WorkbookID: &res.WorkbookID})
	var ar *ambitionimport.AlreadyRunningError
	if !errors.As(err, &ar) {
		t.Fatalf("got %v", err)
	}
}

func TestHandleStopsOnLeaseLoss(t *testing.T) {
	e := newEnv(t)
	e.fetch.pages[1] = pageOf(1, ids(1, 20), ip(5), ip(100), nil)
	e.fetch.pages[2] = pageOf(2, ids(21, 40), ip(5), ip(100), nil)
	res, c := startRun(e, 30)
	// The lease is lost while page 1 is being fetched; page 2 must not be fetched.
	e.fetch.before[1] = func() {
		if _, err := e.owner.Exec(e.ctx, `UPDATE jobs SET lease_token = 99 WHERE id=$1`, c.ID); err != nil {
			t.Error(err)
		}
	}
	err := ambitionimport.Handle(e.ctx, e.d, c)
	if !errors.Is(err, ambitionimport.ErrLeaseLost) {
		t.Fatalf("got %v", err)
	}
	if got := fmt.Sprint(e.fetch.callPages()); got != "[1]" {
		t.Fatalf("fetched %s", got)
	}
	// Page 1 was fetched, committed (it was in flight), and the run stays active, never stuck running.
	run := e.runView(wsA, res.RunID)
	if run["status"] != "retrying" {
		t.Fatalf("run %+v", run)
	}
}

func TestHandleStopsOnCancel(t *testing.T) {
	e2 := newEnv(t)
	e2.fetch.pages[1] = pageOf(1, ids(1, 20), ip(5), ip(100), nil)
	res2, c2 := startRun(e2, 5)
	if _, err := e2.owner.Exec(e2.ctx, `UPDATE jobs SET status='cancelled' WHERE id=$1`, c2.ID); err != nil {
		t.Fatal(err)
	}
	if err := ambitionimport.Handle(e2.ctx, e2.d, c2); !errors.Is(err, ambitionimport.ErrLeaseLost) {
		t.Fatalf("cancel: got %v", err)
	}
	if len(e2.fetch.callPages()) != 0 {
		t.Fatal("fetched after cancel")
	}
	if run := e2.runView(wsA, res2.RunID); run["status"] != "retrying" {
		t.Fatalf("run %+v", run)
	}
}
