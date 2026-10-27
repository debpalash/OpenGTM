//go:build integration

package autoengine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autoactions"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autoschedule"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/billing"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

func pl(fk string, extra map[string]any) map[string]any {
	m := map[string]any{"fire_source": "row_added", "fire_key": fk}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func withResearchCost(t *testing.T) {
	prev := billing.ResearchVendorCost
	billing.ResearchVendorCost = func() bool { return true }
	t.Cleanup(func() { billing.ResearchVendorCost = prev })
}

func (e *env) must(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) job(id int64) (executor, status string) {
	e.must(e.pool.QueryRow(e.ctx, `SELECT executor, status FROM jobs WHERE id=$1`, id).Scan(&executor, &status))
	return
}

func TestReplayExistingResultIsSkipped(t *testing.T) {
	e := newEnv(t)
	e.defaultSeed(trig{})
	rec := &recorder{}
	e.d.Execute = rec.exec
	e.must(e.run(pl("fk1", nil)))
	first := len(rec.calls)
	e.must(e.run(pl("fk1", nil)))
	if len(rec.calls) != first {
		t.Fatalf("second delivery re-executed: %d calls", len(rec.calls))
	}
	rs := e.runs(ws1)
	if len(rs) != 2 || rs[1].Skipped != 3 || rs[1].Attempted != 0 || rs[1].Status != "completed" {
		t.Fatalf("%+v", rs)
	}
	if n := e.count(ws1, "trigger_action_results"); n != 3 {
		t.Fatalf("results %d", n)
	}
}

func TestInFlightNonIdempotentIsReplaySkipped(t *testing.T) {
	e := newEnv(t)
	e.defaultSeed(trig{})
	rec := &recorder{}
	e.d.Execute = rec.exec
	e.inWS(ws1, func(tx pgx.Tx) error {
		_, err := tx.Exec(e.ctx, `INSERT INTO trigger_action_results (workspace_id, run_id, trigger_id, workbook_id, row_id, action_index, action_type, idempotency_key, status)
			VALUES ($1,'old','trg-1','wbA',$2,0,'webhook',$3,'in_flight')`, ws1, fmt.Sprint(e.rowIDs[0]), fmtKey(ws1, "trg-1", "wbA", e.rowIDs[0], 0, "fk1"))
		return err
	})
	e.must(e.run(pl("fk1", map[string]any{"targets": []any{e.targetOf("wbA", 0)}})))
	if len(rec.calls) != 0 {
		t.Fatalf("webhook re-sent: %v", rec.calls)
	}
	r := e.results(ws1)[0]
	if r.Status != "skipped" || r.Skip == nil || *r.Skip != "replay" {
		t.Fatalf("%+v", r)
	}
}

func TestInFlightIdempotentReattempts(t *testing.T) {
	e := newEnv(t)
	e.defaultSeed(trig{Actions: "[" + actFree + "]"})
	rec := &recorder{}
	e.d.Execute = rec.exec
	e.inWS(ws1, func(tx pgx.Tx) error {
		_, err := tx.Exec(e.ctx, `INSERT INTO trigger_action_results (workspace_id, run_id, trigger_id, workbook_id, row_id, action_index, action_type, idempotency_key, status, charged_usd)
			VALUES ($1,'old','trg-1','wbA',$2,0,'re_enrich',$3,'in_flight',0.5)`, ws1, fmt.Sprint(e.rowIDs[0]), fmtKey(ws1, "trg-1", "wbA", e.rowIDs[0], 0, "fk1"))
		return err
	})
	e.must(e.run(pl("fk1", map[string]any{"targets": []any{e.targetOf("wbA", 0)}})))
	if len(rec.calls) != 1 {
		t.Fatalf("calls %v", rec.calls)
	}
	rs := e.results(ws1)
	if len(rs) != 1 || rs[0].Status != "success" || rs[0].RunID != "old" {
		t.Fatalf("marker not reused: %+v", rs)
	}
}

func TestWebhookSentAtMostOnceAcrossRetries(t *testing.T) {
	e := newEnv(t)
	e.defaultSeed(trig{})
	var sent atomic.Int32
	crash := true
	e.d.Execute = func(ctx context.Context, ws string, a map[string]any, wb, row string, lead *int64, ld map[string]any, cols []autoactions.Column, idem string) autoactions.Result {
		sent.Add(1) // the send happens, then the process dies before the terminal update
		if crash {
			panic("simulated crash after the in_flight marker commit")
		}
		return okResult()
	}
	tg := map[string]any{"targets": []any{e.targetOf("wbA", 0)}}
	e.must(e.run(pl("fkc", tg)))
	if r := e.results(ws1)[0]; r.Status != "in_flight" {
		t.Fatalf("marker status %q", r.Status)
	}
	crash = false
	e.must(e.run(pl("fkc", tg))) // the retried delivery
	e.must(e.run(pl("fkc", tg)))
	if sent.Load() != 1 {
		t.Fatalf("webhook sent %d times", sent.Load())
	}
	if r := e.results(ws1)[0]; r.Status != "skipped" || r.Skip == nil || *r.Skip != "replay" {
		t.Fatalf("%+v", r)
	}
}

func TestConcurrentSameKeyInsertsOneResult(t *testing.T) {
	e := newEnv(t)
	e.defaultSeed(trig{})
	var sent atomic.Int32
	e.d.Execute = func(ctx context.Context, ws string, a map[string]any, wb, row string, lead *int64, ld map[string]any, cols []autoactions.Column, idem string) autoactions.Result {
		sent.Add(1)
		time.Sleep(30 * time.Millisecond)
		return okResult()
	}
	tg := map[string]any{"targets": []any{e.targetOf("wbA", 0)}}
	var claims []*queue.Claim
	for i := 0; i < 8; i++ {
		claims = append(claims, e.claim(pl("same", tg)))
	}
	var wg sync.WaitGroup
	for _, c := range claims {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := Handle(e.ctx, e.d, c); err != nil {
				t.Errorf("handle: %v", err)
			}
		}()
	}
	wg.Wait()
	if n := len(e.results(ws1)); n != 1 {
		t.Fatalf("%d result rows", n)
	}
	if sent.Load() != 1 {
		t.Fatalf("effect ran %d times", sent.Load())
	}
	if rs := e.runs(ws1); len(rs) != 8 {
		t.Fatalf("%d runs", len(rs))
	}
}

func TestCapRefusalIsSkippedCap(t *testing.T) {
	withResearchCost(t)
	e := newEnv(t)
	one := 1
	e.defaultSeed(trig{Actions: "[" + actPaid + "]", MaxActions: &one})
	rec := &recorder{}
	e.d.Execute = rec.exec
	e.must(e.run(pl("fk1", nil)))
	rs := e.results(ws1)
	if len(rs) != 3 || rs[0].Status != "success" {
		t.Fatalf("%+v", rs)
	}
	for _, r := range rs[1:] {
		if r.Status != "skipped" || *r.Skip != "cap" || *r.Summary != "daily cap reached" || r.Charged != 0 {
			t.Fatalf("%+v", r)
		}
	}
	if len(rec.calls) != 1 {
		t.Fatalf("executed %d", len(rec.calls))
	}
}

func TestCreditRefusalReleasesReservation(t *testing.T) {
	withResearchCost(t)
	e := newEnv(t)
	e.defaultSeed(trig{Actions: "[" + actPaid + "]"})
	e.credits(ws1, 0.07) // one 0.05 action fits, the second does not
	e.d.Execute = (&recorder{}).exec
	e.must(e.run(pl("fk1", nil)))
	rs := e.results(ws1)
	if rs[0].Status != "success" || rs[1].Status != "skipped" || *rs[1].Skip != "credits" || *rs[1].Summary != "insufficient credits" {
		t.Fatalf("%+v", rs)
	}
	// nothing stays held: only the first action's settled reservation exists
	var held, settled int
	e.inWS(ws1, func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, `SELECT count(*) FILTER (WHERE state='held'), count(*) FILTER (WHERE state='settled') FROM trigger_cap_reservations`).Scan(&held, &settled)
	})
	if held != 0 || settled != 1 {
		t.Fatalf("held %d settled %d", held, settled)
	}
}

func TestReleaseOnEveryNonSuccessPath(t *testing.T) {
	withResearchCost(t)
	e := newEnv(t)
	e.defaultSeed(trig{Actions: "[" + actPaid + "]"})
	n := 0
	e.d.Execute = func(ctx context.Context, ws string, a map[string]any, wb, row string, lead *int64, ld map[string]any, cols []autoactions.Column, idem string) autoactions.Result {
		n++
		switch n {
		case 1:
			return autoactions.Result{Status: "failed", Error: "boom"}
		case 2:
			return autoactions.Result{Status: "skipped", SkipReason: "no_target"}
		}
		return okResult()
	}
	e.must(e.run(pl("fk1", nil)))
	states := map[string]int{}
	e.inWS(ws1, func(tx pgx.Tx) error {
		rs, err := tx.Query(e.ctx, `SELECT state FROM trigger_cap_reservations`)
		if err != nil {
			return err
		}
		defer rs.Close()
		for rs.Next() {
			var s string
			if err := rs.Scan(&s); err != nil {
				return err
			}
			states[s]++
		}
		return rs.Err()
	})
	if states["released"] != 2 || states["settled"] != 1 || states["held"] != 0 {
		t.Fatalf("%v", states)
	}
}

func TestDebitChargedOncePerIdempotencyKey(t *testing.T) {
	withResearchCost(t)
	e := newEnv(t)
	e.defaultSeed(trig{Actions: "[" + actPaid + "]"})
	e.d.Execute = (&recorder{}).exec
	e.must(e.run(pl("fk1", nil)))
	e.must(e.run(pl("fk1", nil)))
	var n int
	var sum float64
	e.inWS(ws1, func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, `SELECT count(*), COALESCE(sum(amount_usd),0) FROM credit_ledger_entries WHERE reason LIKE 'automation_%'`).Scan(&n, &sum)
	})
	if n != 3 || sum > -0.1499 || sum < -0.1501 {
		t.Fatalf("ledger rows %d sum %v", n, sum)
	}
	if b := e.balance(ws1); b < 9.8499 || b > 9.8501 {
		t.Fatalf("balance %v", b)
	}
}

func TestDebitReservationAndMarkerAtomic(t *testing.T) {
	withResearchCost(t)
	e := newEnv(t)
	e.defaultSeed(trig{Actions: "[" + actPaid + "]"})
	rec := &recorder{}
	e.d.Execute = rec.exec
	eng := &engine{d: e.d, afterDebit: func() error { return errors.New("killed between the debit and the marker") }}
	if err := eng.handle(e.ctx, e.claim(pl("fk1", map[string]any{"targets": []any{e.targetOf("wbA", 0)}}))); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 0 {
		t.Fatal("effect ran")
	}
	var led, resv int
	e.inWS(ws1, func(tx pgx.Tx) error {
		if err := tx.QueryRow(e.ctx, `SELECT count(*) FROM credit_ledger_entries`).Scan(&led); err != nil {
			return err
		}
		return tx.QueryRow(e.ctx, `SELECT count(*) FROM trigger_cap_reservations`).Scan(&resv)
	})
	if led != 0 || resv != 0 || e.count(ws1, "trigger_action_results") != 0 || e.balance(ws1) != 10 {
		t.Fatalf("half-written state: ledger %d reservations %d results %d balance %v", led, resv, e.count(ws1, "trigger_action_results"), e.balance(ws1))
	}
	if r := e.runs(ws1)[0]; r.Failed != 1 || r.Status != "failed" {
		t.Fatalf("%+v", r)
	}
	// the same key then runs normally: charged exactly once
	e.must(e.run(pl("fk1", map[string]any{"targets": []any{e.targetOf("wbA", 0)}})))
	if e.balance(ws1) < 9.9499 || e.balance(ws1) > 9.9501 {
		t.Fatalf("balance %v", e.balance(ws1))
	}
}

func TestDryRunIsSideEffectFree(t *testing.T) {
	withResearchCost(t)
	e := newEnv(t)
	e.defaultSeed(trig{Actions: "[" + actWH + "," + actCRM + "," + actPaid + "]"})
	rec := &recorder{}
	e.d.Execute = rec.exec
	e.must(e.run(pl("fk1", map[string]any{"dry_run": true})))
	if len(rec.calls) != 0 {
		t.Fatalf("a dry run executed %d actions", len(rec.calls))
	}
	if e.count(ws1, "trigger_cap_reservations") != 0 || e.count(ws1, "credit_ledger_entries") != 0 || e.balance(ws1) != 10 {
		t.Fatal("a dry run reserved or charged")
	}
	rs := e.results(ws1)
	if len(rs) != 9 {
		t.Fatalf("%d results", len(rs))
	}
	for _, r := range rs {
		if r.Status != "skipped" || r.Skip == nil || *r.Skip != "dry_run" || r.Charged != 0 {
			t.Fatalf("%+v", r)
		}
	}
	runs := e.runs(ws1)
	if len(runs) != 1 || runs[0].Status != "dry_run" {
		t.Fatalf("%+v", runs)
	}
	var fired bool
	e.inWS(ws1, func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, `SELECT last_fired_at IS NOT NULL FROM triggers WHERE id='trg-1'`).Scan(&fired)
	})
	if fired {
		t.Fatal("last_fired_at set by a dry run")
	}
}

func TestMaxActionsCapsUnpaidActions(t *testing.T) {
	e := newEnv(t)
	two := 2
	e.defaultSeed(trig{Actions: "[" + actWH + "]", MaxActions: &two})
	for i := 0; i < 2; i++ { // 5 rows in total
		e.row(ws1, "wbA", `{"company":"Extra","email":"x@x.com","score":"50"}`, "", nil)
	}
	rec := &recorder{}
	e.d.Execute = rec.exec
	e.must(e.run(pl("fk1", nil)))
	if len(rec.calls) != 2 {
		t.Fatalf("executed %d, want 2", len(rec.calls))
	}
	ok, capped := 0, 0
	for _, r := range e.results(ws1) {
		switch {
		case r.Status == "success":
			ok++
		case r.Status == "skipped" && r.Skip != nil && *r.Skip == "cap":
			capped++
		default:
			t.Fatalf("%+v", r)
		}
	}
	if ok != 2 || capped != 3 {
		t.Fatalf("success %d capped %d", ok, capped)
	}
}

func TestExplicitTargetsLimitedToRuleScope(t *testing.T) {
	e := newEnv(t)
	e.defaultSeed(trig{Scope: `["wbA"]`})
	e.row(ws1, "wbB", `{"company":"Out","email":"o@o.com","score":"50"}`, "", nil)
	rec := &recorder{}
	e.d.Execute = rec.exec
	e.must(e.run(pl("fk1", map[string]any{"targets": []any{e.targetOf("wbA", 0), e.targetOf("wbB", 3)}})))
	rs := e.results(ws1)
	if len(rs) != 1 || rs[0].WB != "wbA" || len(rec.calls) != 1 {
		t.Fatalf("%+v calls %d", rs, len(rec.calls))
	}
}

func TestStopOnErrorBreaksBothLoops(t *testing.T) {
	e := newEnv(t)
	e.defaultSeed(trig{StopOnError: true, Actions: "[" + actWH + "," + actCRM + "]"})
	rec := &recorder{fn: func(idem string, a map[string]any) autoactions.Result {
		return autoactions.Result{Status: "failed", Error: "e"}
	}}
	e.d.Execute = rec.exec
	e.must(e.run(pl("fk1", nil)))
	if len(rec.calls) != 1 {
		t.Fatalf("%d calls after the first failure", len(rec.calls))
	}
	if r := e.runs(ws1)[0]; r.Status != "failed" || r.Failed != 1 || r.Matched != 1 {
		t.Fatalf("%+v", r)
	}
}

// Since the plan 15 cutover the legacy outreach actions always run in Go: no hand-back, the job stays on Go.
func TestLegacyOutreachActionsNeverHandBack(t *testing.T) {
	e := newEnv(t)
	for _, typ := range []string{"send_email", "sequencer"} {
		e.clean()
		e.rowIDs = nil
		e.defaultSeed(trig{Actions: `[` + actWH + `,{"type":"` + typ + `","config":{}}]`})
		rec := &recorder{}
		e.d.Execute = rec.exec
		c := e.claim(pl("fk1", nil))
		if err := Handle(e.ctx, e.d, c); err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
		if ex, _ := e.job(c.ID); ex != "go" {
			t.Fatalf("%s: job executor %s", typ, ex)
		}
		if len(rec.calls) == 0 || e.count(ws1, "trigger_runs") != 1 {
			t.Fatalf("%s: the eval did not run in Go (calls %d)", typ, len(rec.calls))
		}
	}
}

// C2 / R-dry: a dry run never hands back to Python (whose worker executes dry runs): rules with
// legacy outreach actions or Python-only re_enrich columns are previewed in Go, nothing runs.
func TestDryRunNeverHandsBack(t *testing.T) {
	e := newEnv(t)
	for i, acts := range []string{
		`[` + actWH + `,{"type":"send_email","config":{}}]`,
		`[{"type":"sequencer","config":{}}]`,
		`[{"type":"re_enrich","config":{"column_ids":["ag"]}}]`,
	} {
		e.clean()
		e.rowIDs = nil
		e.defaultSeed(trig{Actions: acts})
		e.workbook(ws1, "wbPy", `[{"id":"ag","name":"Agent","type":"agent"}]`)
		rec := &recorder{}
		e.d.Execute = rec.exec
		c := e.claim(pl(fmt.Sprintf("fk-dry-%d", i), map[string]any{"dry_run": true}))
		if err := Handle(e.ctx, e.d, c); err != nil {
			t.Fatalf("%s: %v", acts, err)
		}
		if ex, _ := e.job(c.ID); ex == "python" {
			t.Fatalf("%s: dry run handed back to python", acts)
		}
		if len(rec.calls) != 0 {
			t.Fatalf("%s: dry run executed %d actions", acts, len(rec.calls))
		}
		runs := e.runs(ws1)
		if len(runs) != 1 || runs[0].Status != "dry_run" {
			t.Fatalf("%s: runs %+v", acts, runs)
		}
		for _, r := range e.results(ws1) {
			if r.Status != "skipped" || r.Skip == nil || *r.Skip != "dry_run" {
				t.Fatalf("%s: %+v", acts, r)
			}
		}
	}
}

// Since the plan 15 cutover a re_enrich of any column type runs in Go (the workbook runner fails the cells it
// cannot run); nothing is handed back.
func TestReEnrichOfAnyColumnRunsInGo(t *testing.T) {
	e := newEnv(t)
	cols := `[{"id":"ag","name":"Agent","type":"agent"},{"id":"ok","type":"waterfall","target_field":"description","waterfall":["research"]},
	 {"id":"em","type":"enrichment","target_field":"email"},{"id":"cond","type":"enrichment","target_field":"x","condition":"{a} == 1"},
	 {"id":"af","type":"ai_formula"}]`
	e.workbook(ws1, "wbA", cols)
	e.row(ws1, "wbA", `{"a":1}`, "", nil)
	for _, ids := range []string{`["ag"]`, `["ok"]`, `["ok","ag"]`, `["em"]`, `["cond"]`, `["af"]`, `["missing"]`} {
		e.inWS(ws1, func(tx pgx.Tx) error { _, err := tx.Exec(e.ctx, `DELETE FROM triggers`); return err })
		e.trigger(ws1, "trg-1", trig{Actions: `[{"type":"re_enrich","config":{"column_ids":` + ids + `}}]`})
		rec := &recorder{}
		e.d.Execute = rec.exec
		before := e.count(ws1, "trigger_runs")
		c := e.claim(pl("fk-"+ids, nil))
		if err := Handle(e.ctx, e.d, c); err != nil || e.count(ws1, "trigger_runs") != before+1 {
			t.Fatalf("%s: err %v", ids, err)
		}
		if ex, _ := e.job(c.ID); ex != "go" {
			t.Fatalf("%s: executor %s", ids, ex)
		}
	}
}

// The tenant is the job row's workspace only (plan 15 cutover): a claim without one is refused permanently,
// whatever the payload names.
func TestEmptyJobWorkspaceIsRefused(t *testing.T) {
	e := newEnv(t)
	e.defaultSeed(trig{})
	rec := &recorder{}
	e.d.Execute = rec.exec
	c := e.claim(pl("fk-nows", nil))
	c.WorkspaceID = ""
	if err := Handle(e.ctx, e.d, c); !queue.IsPermanent(err) {
		t.Fatalf("want a permanent refusal, got %v", err)
	}
	if len(rec.calls) != 0 || e.count(ws1, "trigger_runs") != 0 {
		t.Fatal("ran without a job workspace")
	}
}

func TestMissingRuleWritesSkippedRun(t *testing.T) {
	e := newEnv(t)
	e.defaultSeed(trig{})
	rec := &recorder{}
	e.d.Execute = rec.exec
	e.must(e.run(pl("fk1", map[string]any{"trigger_id": "nope"})))
	rs := e.runs(ws1)
	if len(rs) != 1 || rs[0].Status != "skipped" || rs[0].Name != nil || !rs[0].Finished || len(rec.calls) != 0 {
		t.Fatalf("%+v", rs)
	}
	// a paused rule is recorded with its name
	e.inWS(ws1, func(tx pgx.Tx) error { _, err := tx.Exec(e.ctx, `UPDATE triggers SET enabled=false`); return err })
	e.must(e.run(pl("fk2", nil)))
	rs = e.runs(ws1)
	if len(rs) != 2 || rs[1].Status != "skipped" || rs[1].Name == nil || *rs[1].Name != "rule" {
		t.Fatalf("%+v", rs)
	}
}

func TestExplicitTargetsOtherWorkbookIgnored(t *testing.T) {
	e := newEnv(t)
	e.defaultSeed(trig{})
	rec := &recorder{}
	e.d.Execute = rec.exec
	e.row(ws1, "wbB", `{"company":"B"}`, "", nil) // ordinal 3
	e.must(e.run(pl("fk1", map[string]any{"targets": []any{
		map[string]any{"workbook_id": "wbB", "row_id": fmt.Sprint(e.rowIDs[0])}, // row of wbA named under wbB
		e.targetOf("wbB", 3),
		map[string]any{"workbook_id": "wbA", "row_id": "abc"},
		"junk",
	}})))
	if len(rec.calls) != 1 || !strings.Contains(rec.calls[0], ":wbB:") {
		t.Fatalf("%v", rec.calls)
	}
}

func TestSweepCapsAtMaxRows(t *testing.T) {
	e := newEnv(t)
	e.defaultSeed(trig{})
	e.row(ws1, "wbB", `{"company":"B1"}`, "", nil)
	e.row(ws1, "wbB", `{"company":"B2"}`, "", nil)
	rec := &recorder{}
	e.d.Execute = rec.exec
	e.d.MaxRowsPerEval = 4
	e.must(e.run(pl("fk1", nil)))
	if len(rec.calls) != 4 {
		t.Fatalf("%d calls", len(rec.calls))
	}
	// wbA's three rows first (workbook order), then one of wbB (created_at, id)
	if !strings.Contains(rec.calls[3], ":wbB:"+fmt.Sprint(e.rowIDs[3])+":") {
		t.Fatalf("%v", rec.calls)
	}
}

func TestCancelStopsBeforeNextAction(t *testing.T) {
	e := newEnv(t)
	e.defaultSeed(trig{})
	c := e.claim(pl("fk1", nil))
	n := 0
	e.d.Execute = func(ctx context.Context, ws string, a map[string]any, wb, row string, lead *int64, ld map[string]any, cols []autoactions.Column, idem string) autoactions.Result {
		n++
		_, err := e.pool.Exec(e.ctx, `UPDATE jobs SET status='cancelled' WHERE id=$1`, c.ID)
		e.must(err)
		return okResult()
	}
	if err := Handle(e.ctx, e.d, c); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("%d actions ran after the cancel", n)
	}
	if r := e.runs(ws1)[0]; r.Status != "failed" || !r.Finished {
		t.Fatalf("a cancelled run must be finalised (no reaper exists): %+v", r)
	}
	if _, st := e.job(c.ID); st != "cancelled" {
		t.Fatalf("job %s", st)
	}
	if e.count(ws1, "workbook_activity") != 0 {
		t.Fatal("finalised a cancelled run")
	}
}

func TestLeaseLossStopsAndPreservesState(t *testing.T) {
	e := newEnv(t)
	e.defaultSeed(trig{})
	c := e.claim(pl("fk1", nil))
	n := 0
	e.d.Execute = func(ctx context.Context, ws string, a map[string]any, wb, row string, lead *int64, ld map[string]any, cols []autoactions.Column, idem string) autoactions.Result {
		n++
		_, err := e.pool.Exec(e.ctx, `UPDATE jobs SET worker_id='someone-else', lease_token=lease_token+1 WHERE id=$1`, c.ID)
		e.must(err)
		return okResult()
	}
	err := Handle(e.ctx, e.d, c)
	if !errors.Is(err, workbook.ErrLeaseLost) {
		t.Fatalf("%v", err)
	}
	if n != 1 {
		t.Fatalf("%d actions", n)
	}
	r := e.runs(ws1)[0]
	if r.Status != "running" || r.Finished {
		t.Fatalf("%+v", r)
	}
	if rs := e.results(ws1); len(rs) != 1 || rs[0].Status != "success" {
		t.Fatalf("committed work must stay: %+v", rs)
	}
}

func TestContextCancelAfterMarkerIsNotTerminal(t *testing.T) {
	e := newEnv(t)
	e.defaultSeed(trig{})
	ctx, cancel := context.WithCancel(e.ctx)
	e.d.Execute = func(c context.Context, ws string, a map[string]any, wb, row string, lead *int64, ld map[string]any, cols []autoactions.Column, idem string) autoactions.Result {
		cancel()
		return autoactions.Result{Status: "failed", Error: "context canceled"}
	}
	c := e.claim(pl("fk1", map[string]any{"targets": []any{e.targetOf("wbA", 0)}}))
	err := Handle(ctx, e.d, c)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
	rs := e.results(ws1)
	if len(rs) != 1 || rs[0].Status != "in_flight" {
		t.Fatalf("a cancel must not become a terminal failure: %+v", rs)
	}
	if r := e.runs(ws1)[0]; r.Status != "failed" || !r.Finished {
		t.Fatalf("an aborted run must be finalised on a detached context: %+v", r)
	}
}

func TestScheduleReschedulesOnce(t *testing.T) {
	e := newEnv(t)
	e.d.Now = nil // real time: the rescheduled job must stay in the future, not claimable
	anchor := time.Now().UTC().Add(-36 * time.Hour).Truncate(time.Second)
	wantKey := autoschedule.FireKey("trg-1", autoschedule.NextRun(anchor, "daily", time.Now().UTC()))
	e.defaultSeed(trig{Type: "on_schedule", TriggerConfig: `{"interval":"daily"}`, Anchor: &anchor})
	e.d.Execute = (&recorder{}).exec
	pending := func() (n int, key string) {
		e.must(e.pool.QueryRow(e.ctx, `SELECT count(*), COALESCE(max(fire_key),'') FROM jobs WHERE type='trigger_eval' AND status='pending'`).Scan(&n, &key))
		return
	}
	e.must(e.run(pl("sched:x", map[string]any{"fire_source": "schedule", "dry_run": true})))
	if n, _ := pending(); n != 0 {
		t.Fatal("a dry run rescheduled")
	}
	e.must(e.run(pl("manual:x", map[string]any{"fire_source": "manual"})))
	if n, _ := pending(); n != 0 {
		t.Fatal("a manual run rescheduled")
	}
	e.must(e.run(pl("sched:1", map[string]any{"fire_source": "schedule"})))
	n, key := pending()
	if n != 1 || key != wantKey {
		t.Fatalf("pending %d %q", n, key)
	}
	e.must(e.run(pl("sched:2", map[string]any{"fire_source": "schedule"})))
	if n, _ := pending(); n != 1 {
		t.Fatalf("not single-flight: %d pending", n)
	}
	var ex string
	e.must(e.pool.QueryRow(e.ctx, `SELECT executor FROM jobs WHERE fire_key=$1`, key).Scan(&ex))
	if ex != "go" {
		t.Fatalf("executor %s", ex)
	}
	var mirrors int
	e.must(e.pool.QueryRow(e.ctx, `SELECT count(*) FROM scheduled_triggers WHERE trigger_id='trg-1' AND enabled`).Scan(&mirrors))
	if mirrors != 1 {
		t.Fatal("mirror row missing")
	}
}

func TestTenantIsolation(t *testing.T) {
	e := newEnv(t)
	e.defaultSeed(trig{})
	e.trigger(ws2, "trg-2", trig{Name: "other"})
	e.row(ws2, "wbX", `{"company":"Foreign"}`, "", nil) // ordinal 3
	rec := &recorder{}
	e.d.Execute = rec.exec
	// another workspace's rule id: the rule is missing in ws-1
	e.must(e.run(pl("fk1", map[string]any{"trigger_id": "trg-2"})))
	if rs := e.runs(ws1); len(rs) != 1 || rs[0].Status != "skipped" || rs[0].Name != nil {
		t.Fatalf("%+v", rs)
	}
	// another workspace's row named as a target: invisible
	e.must(e.run(pl("fk2", map[string]any{"targets": []any{map[string]any{"workbook_id": "wbX", "row_id": fmt.Sprint(e.rowIDs[3])}}})))
	if len(rec.calls) != 0 {
		t.Fatalf("executed a foreign row: %v", rec.calls)
	}
	if e.count(ws2, "trigger_runs") != 0 || e.count(ws2, "trigger_action_results") != 0 {
		t.Fatal("wrote into the other workspace")
	}
	// the sweep sees only its own workspace's workbooks
	e.must(e.run(pl("fk3", nil)))
	for _, c := range rec.calls {
		if strings.Contains(c, ":wbX:") {
			t.Fatalf("swept a foreign workbook: %s", c)
		}
	}
}

func TestFlagOffAndMissingIDsAreNoOps(t *testing.T) {
	e := newEnv(t)
	e.defaultSeed(trig{})
	e.d.Execute = (&recorder{}).exec
	e.d.Enabled = false
	e.must(e.run(pl("fk1", nil)))
	e.d.Enabled = true
	e.must(Handle(e.ctx, e.d, e.claimRaw(map[string]any{"workspace_id": ws1})))
	e.must(Handle(e.ctx, e.d, e.claimRaw(map[string]any{"trigger_id": "trg-1"})))
	if e.count(ws1, "trigger_runs") != 0 {
		t.Fatal("wrote a run")
	}
}

func TestPaidRowsReserveProjectCostOnly(t *testing.T) {
	withResearchCost(t)
	e := newEnv(t)
	e.defaultSeed(trig{Actions: "[" + actWH + "," + actPaid + "]"})
	e.d.Execute = (&recorder{}).exec
	e.must(e.run(pl("fk1", nil)))
	var n int
	e.inWS(ws1, func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, `SELECT count(*) FROM trigger_cap_reservations WHERE idempotency_key LIKE '%:0:fk1'`).Scan(&n)
	})
	if n != 0 {
		t.Fatalf("a free webhook reserved against the caps: %d", n)
	}
}

func TestCancelAfterSuccessStillSettles(t *testing.T) {
	withResearchCost(t)
	e := newEnv(t)
	e.defaultSeed(trig{Actions: "[" + actPaid + "]"})
	ctx, cancel := context.WithCancel(e.ctx)
	e.d.Execute = func(c context.Context, ws string, a map[string]any, wb, row string, lead *int64, ld map[string]any, cols []autoactions.Column, idem string) autoactions.Result {
		cancel() // the effect happened, then the job context died
		return okResult()
	}
	err := Handle(ctx, e.d, e.claim(pl("fk1", map[string]any{"targets": []any{e.targetOf("wbA", 0)}})))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
	rs := e.results(ws1)
	if len(rs) != 1 || rs[0].Status != "success" || rs[0].Charged < 0.0499 {
		t.Fatalf("marker not settled: %+v", rs)
	}
	var held int
	e.inWS(ws1, func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, `SELECT count(*) FROM trigger_cap_reservations WHERE state='held'`).Scan(&held)
	})
	if held != 0 {
		t.Fatalf("%d reservations still held", held)
	}
}

func TestIdempotentReattemptKeepsPriorChargeAndCreditRefusalUpdatesMarker(t *testing.T) {
	withResearchCost(t)
	e := newEnv(t)
	e.defaultSeed(trig{Actions: "[" + actPaid + "]"})
	e.d.Execute = (&recorder{}).exec
	tg := map[string]any{"targets": []any{e.targetOf("wbA", 0)}}
	// first delivery charges, then "crashes" leaving an in_flight marker
	e.d.Execute = func(context.Context, string, map[string]any, string, string, *int64, map[string]any, []autoactions.Column, string) autoactions.Result {
		panic("crash")
	}
	e.must(e.run(pl("fk1", tg)))
	e.d.Execute = (&recorder{}).exec
	e.must(e.run(pl("fk1", tg)))
	r := e.results(ws1)[0]
	if r.Status != "success" || r.Charged < 0.0499 || r.Charged > 0.0501 {
		t.Fatalf("prior charge lost: %+v", r)
	}
	// credit refusal on a re-attempt turns the marker into skipped/credits
	e2 := "fk2"
	e.d.Execute = func(context.Context, string, map[string]any, string, string, *int64, map[string]any, []autoactions.Column, string) autoactions.Result {
		panic("crash")
	}
	e.inWS(ws1, func(tx pgx.Tx) error {
		_, err := tx.Exec(e.ctx, `INSERT INTO trigger_action_results (workspace_id, run_id, trigger_id, workbook_id, row_id, action_index, action_type, idempotency_key, status)
			VALUES ($1,'old','trg-1','wbA',$2,0,'re_enrich',$3,'in_flight')`, ws1, fmt.Sprint(e.rowIDs[1]), fmtKey(ws1, "trg-1", "wbA", e.rowIDs[1], 0, e2))
		return err
	})
	e.credits(ws1, 0)
	e.must(e.run(pl(e2, map[string]any{"targets": []any{e.targetOf("wbA", 1)}})))
	for _, x := range e.results(ws1) {
		if x.Key == fmtKey(ws1, "trg-1", "wbA", e.rowIDs[1], 0, e2) && (x.Status != "skipped" || *x.Skip != "credits") {
			t.Fatalf("marker left in_flight: %+v", x)
		}
	}
}

// Python expires its ORM rows at every action commit, so a long sweep
// evaluates each row from its current data: a row edited (or deleted) after
// the sweep began is judged on what it holds when it is reached.
func TestSweepReadsRowFreshBeforeEvaluating(t *testing.T) {
	e := newEnv(t)
	e.defaultSeed(trig{Condition: "{score} > 50"})
	rec := &recorder{}
	e.d.Execute = func(ctx context.Context, ws string, a map[string]any, wb, row string, lead *int64, ld map[string]any, cols []autoactions.Column, idem string) autoactions.Result {
		if len(rec.calls) == 0 { // while the first row's action runs, a user edits the others
			e.inWS(ws1, func(tx pgx.Tx) error {
				if _, err := tx.Exec(e.ctx, `UPDATE workbook_rows SET data='{"company":"Beta","score":"80"}'::json WHERE id=$1`, e.rowIDs[1]); err != nil {
					return err
				}
				_, err := tx.Exec(e.ctx, `UPDATE workbook_rows SET data='{"company":"Gamma","score":"10"}'::json, enrichments=NULL WHERE id=$1`, e.rowIDs[2])
				return err
			})
		}
		return rec.exec(ctx, ws, a, wb, row, lead, ld, cols, idem)
	}
	e.must(e.run(pl("fk-fresh", nil)))
	if len(rec.calls) != 2 ||
		!strings.Contains(rec.calls[0], fmt.Sprintf(":%d:", e.rowIDs[0])) ||
		!strings.Contains(rec.calls[1], fmt.Sprintf(":%d:", e.rowIDs[1])) {
		t.Fatalf("stale snapshot evaluated: %v", rec.calls)
	}
}

func TestSweepSkipsRowDeletedMidSweep(t *testing.T) {
	e := newEnv(t)
	e.defaultSeed(trig{})
	rec := &recorder{}
	e.d.Execute = func(ctx context.Context, ws string, a map[string]any, wb, row string, lead *int64, ld map[string]any, cols []autoactions.Column, idem string) autoactions.Result {
		if len(rec.calls) == 0 {
			e.inWS(ws1, func(tx pgx.Tx) error {
				_, err := tx.Exec(e.ctx, `DELETE FROM workbook_rows WHERE id=$1`, e.rowIDs[1])
				return err
			})
		}
		return rec.exec(ctx, ws, a, wb, row, lead, ld, cols, idem)
	}
	e.must(e.run(pl("fk-del", nil)))
	if len(rec.calls) != 2 {
		t.Fatalf("deleted row still acted on: %v", rec.calls)
	}
}

// Final review (security low): the payload's workspace must equal the job
// row's, or a copied/crafted job would run inside another tenant.
func TestPayloadWorkspaceMustMatchJobRow(t *testing.T) {
	e := newEnv(t)
	e.defaultSeed(trig{})
	e.trigger(ws2, "trg-2", trig{})
	rec := &recorder{}
	e.d.Execute = rec.exec
	c := e.claimRaw(map[string]any{"workspace_id": ws2, "trigger_id": "trg-2", "fire_source": "row_added", "fire_key": "fk-x"}) // job row is ws1
	if err := Handle(e.ctx, e.d, c); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 0 || len(e.runs(ws2)) != 0 || len(e.runs(ws1)) != 0 {
		t.Fatalf("ran across workspaces: calls=%v ws2 runs=%d ws1 runs=%d", rec.calls, len(e.runs(ws2)), len(e.runs(ws1)))
	}
}
