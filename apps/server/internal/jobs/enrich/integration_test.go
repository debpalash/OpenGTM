package enrich

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/server/internal/progress"
	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
)

// Cross-language behavioural parity is proven by tests/test_enrichment_go_parity_pg.py.
// These tests cover what a parity run cannot: the lease, cancellation, crash
// recovery, atomicity, bounded concurrency, tenant isolation, egress and routing.

func TestEnrichesCellsAndPersistsEvidenceWithoutLeakingSecrets(t *testing.T) {
	e := newEnv(t)
	ws := e.ws("happy")
	cols := []colCfg{enrichCol("email", "it_free", "email"), chainCol("phone", []string{"it_phone"}, "phone"),
		enrichCol("keyed", "it_key", "email"), enrichCol("qk", "it_query", "email")}
	rows := e.workbook(ws, "wb-"+ws, 0, cols, "https://www.acme.example/about")
	job := e.job(ws, e.payload(ws, "wb-"+ws, cols, rows, nil))

	events := listen(t, e)
	if err := e.w.Handle(context.Background(), job); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct{ col, want string }{
		{"email", "info@acme.example"}, {"phone", "+15550100"}, {"keyed", "info@acme.example"}, {"qk", "info@acme.example"},
	} {
		v, st, prov, er := e.cell("wb-"+ws, rows[0], c.col)
		if v != c.want || st != "complete" || er != "<nil>" || prov == "<nil>" {
			t.Errorf("%s = %q %s %s %s", c.col, v, st, prov, er)
		}
	}
	// the row mirror carries the same cells (what the rows endpoint reads)
	if got := e.scalar(`SELECT enrichments::jsonb->'email'->>'value' FROM workbook_rows WHERE id = $1`, rows[0]); got != "info@acme.example" {
		t.Errorf("row mirror email = %s", got)
	}
	if st := e.scalar(`SELECT status FROM workbooks WHERE id = $1`, "wb-"+ws); st != "complete" {
		t.Errorf("workbook status = %s", st)
	}
	if got := e.scalar(`SELECT total_rows || '/' || completed_rows FROM workbooks WHERE id = $1`, "wb-"+ws); got != "1/1" {
		t.Errorf("progress = %s", got)
	}

	// evidence: where the winning value came from, with the credential redacted
	raw := e.scalar(`SELECT cell_metadata->'evidence' FROM workbook_enrichments WHERE workbook_id = $1 AND column_id = 'qk'`, "wb-"+ws)
	var ev map[string]any
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("evidence %q: %v", raw, err)
	}
	if !strings.Contains(ev["source_url"].(string), "key=REDACTED") || ev["sha256"] == "" || ev["status"].(float64) != 200 {
		t.Errorf("evidence = %v", ev)
	}
	whole := e.scalar(`SELECT string_agg(cell_metadata::text, ' ') FROM workbook_enrichments WHERE workbook_id = $1`, "wb-"+ws)
	for _, table := range []string{"workbook_enrichments", "workbook_rows"} {
		if strings.Contains(e.scalar(`SELECT string_agg(t::text, ' ') FROM `+table+` t WHERE workspace_id = $1`, ws), itKey) {
			t.Errorf("the secret reached %s", table)
		}
	}
	if strings.Contains(whole, itKey) {
		t.Error("the secret reached cell_metadata")
	}
	// ...but the provider did receive it (header and query)
	var sawHeader, sawQuery bool
	for _, r := range e.sim.requests() {
		sawHeader = sawHeader || r.Header.Get("X-Api-Key") == itKey
		sawQuery = sawQuery || r.Key == itKey
	}
	if !sawHeader || !sawQuery {
		t.Errorf("credentials were not sent: header=%v query=%v", sawHeader, sawQuery)
	}

	// progress is published inside the committing transactions, per workspace
	names := events.drain(2 * time.Second)
	if names["workbook_cell_update"] < 4 || names["workbook_status"] < 2 {
		t.Errorf("progress events = %v", names)
	}
}

// listener collects progress events for one workspace-agnostic test run.
type listener struct {
	ch chan progress.Event
}

func listen(t *testing.T, e *env) *listener {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	conn, err := e.app.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "LISTEN "+progress.Channel); err != nil {
		t.Fatal(err)
	}
	l := &listener{ch: make(chan progress.Event, 4096)}
	go func() {
		defer conn.Release()
		for {
			n, err := conn.Conn().WaitForNotification(ctx)
			if err != nil {
				return
			}
			var ev progress.Event
			if json.Unmarshal([]byte(n.Payload), &ev) == nil {
				var data map[string]any
				_ = json.Unmarshal(ev.Data, &data)
				if data["workbook_id"] != nil && strings.HasPrefix(fmt.Sprint(data["workbook_id"]), "wb-it-") {
					l.ch <- ev
				}
			}
		}
	}()
	return l
}

func (l *listener) drain(wait time.Duration) map[string]int {
	out := map[string]int{}
	deadline := time.After(wait)
	for {
		select {
		case ev := <-l.ch:
			out[ev.Event]++
		case <-deadline:
			return out
		}
	}
}

func TestTenantIsolation(t *testing.T) {
	e := newEnv(t)
	a, b := e.ws("a"), e.ws("b")
	cols := []colCfg{enrichCol("email", "it_free", "email")}
	rowsA := e.workbook(a, "wb-"+a, 0, cols, "a.example")
	rowsB := e.workbook(b, "wb-"+b, 0, cols, "b.example")

	// A job for workspace B that names workspace A's workbook finds nothing.
	job := e.job(b, e.payload(b, "wb-"+a, cols, rowsA, nil))
	before := e.sim.count("")
	if err := e.w.Handle(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if e.sim.count("") != before {
		t.Error("a provider was called for another tenant's workbook")
	}
	if n := e.scalar(`SELECT count(*) FROM workbook_enrichments WHERE workspace_id IN ($1, $2)`, a, b); n != "0" {
		t.Errorf("cells were written: %s", n)
	}

	// Each workspace's own run writes only its own rows.
	for _, c := range []struct {
		ws, wb string
		rows   []int64
	}{{a, "wb-" + a, rowsA}, {b, "wb-" + b, rowsB}} {
		if err := e.w.Handle(context.Background(), e.job(c.ws, e.payload(c.ws, c.wb, cols, c.rows, nil))); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.scalar(`SELECT count(*) FROM workbook_enrichments WHERE workspace_id = $1 AND workbook_id = $2`, a, "wb-"+a); n != "1" {
		t.Errorf("workspace a cells = %s", n)
	}
	if n := e.scalar(`SELECT count(*) FROM workbook_enrichments WHERE workspace_id = $1 AND workbook_id = $2`, a, "wb-"+b); n != "0" {
		t.Errorf("workspace b's workbook was written under workspace a: %s", n)
	}

	// A payload workspace that disagrees with the job's is refused outright.
	bad := e.job(a, e.payload(b, "wb-"+b, cols, rowsB, nil))
	bad.WorkspaceID = a
	if err := e.w.Handle(context.Background(), bad); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Errorf("mismatched workspace: %v", err)
	}
}

func TestUnsupportedRunsFailBeforeAnyWork(t *testing.T) {
	e := newEnv(t)
	ws := e.ws("unsup")
	linked := e.ws("linked")
	good := enrichCol("email", "it_free", "email")
	cases := map[string]colCfg{
		"ai column":        {"id": "ai", "type": "ai_formula", "prompt": "hi"},
		"default chain":    {"id": "email", "type": "enrichment", "target_field": "email", "verify": false},
		"unknown provider": enrichCol("email", "nobody", "email"),
		"email verify":     {"id": "email", "type": "enrichment", "provider": "it_free", "target_field": "email"},
		"condition":        {"id": "email", "type": "enrichment", "provider": "it_free", "target_field": "email", "verify": false, "condition": "x"},
		"column reference": {"id": "email", "type": "enrichment", "provider": "it_free", "target_field": "email", "verify": false, "input_columns": []string{"other"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			wb := "wb-" + ws + "-" + strings.ReplaceAll(name, " ", "")
			cols := []colCfg{c}
			rows := e.workbook(ws, wb, 0, cols, "x.example")
			before := e.sim.count("")
			err := e.w.Handle(context.Background(), e.job(ws, e.payload(ws, wb, cols, rows, nil)))
			var unsupported *UnsupportedError
			if !errors.As(err, &unsupported) {
				t.Fatalf("want UnsupportedError, got %v", err)
			}
			if e.sim.count("") != before {
				t.Error("a provider was called")
			}
			if st := e.scalar(`SELECT status FROM workbooks WHERE id = $1`, wb); st != "draft" {
				t.Errorf("workbook status changed to %s", st)
			}
		})
	}
	t.Run("linked rows", func(t *testing.T) {
		wb := "wb-" + linked
		cols := []colCfg{good}
		rows := e.workbook(linked, wb, 0, cols, "x.example")
		e.exec(`UPDATE workbook_rows SET lead_id = 41 WHERE id = $1`, rows[0])
		err := e.w.Handle(context.Background(), e.job(linked, e.payload(linked, wb, cols, rows, nil)))
		var unsupported *UnsupportedError
		if !errors.As(err, &unsupported) || !strings.Contains(err.Error(), "linked to leads") {
			t.Fatalf("want a linked-rows refusal, got %v", err)
		}
	})
}

func TestProviderConcurrencyIsBounded(t *testing.T) {
	e := newEnv(t)
	ws := e.ws("bound")
	e.sim.setDelay(120 * time.Millisecond)
	cols := []colCfg{enrichCol("email", "it_free", "email")}
	var sites []string
	for i := range 24 {
		sites = append(sites, fmt.Sprintf("row%d.example", i))
	}
	rows := e.workbook(ws, "wb-"+ws, 0, cols, sites...)
	payload := e.payload(ws, "wb-"+ws, cols, rows, map[string]any{"concurrency": 12, "provider_workers": 3})
	if err := e.w.Handle(context.Background(), e.job(ws, payload)); err != nil {
		t.Fatal(err)
	}
	if got := e.sim.maxInflight.Load(); got > 3 || got < 2 {
		t.Errorf("max in-flight provider calls = %d, want 2..3 (provider_workers)", got)
	}
	if n := e.scalar(`SELECT count(*) FROM workbook_enrichments WHERE workbook_id = $1 AND status = 'complete'`, "wb-"+ws); n != "24" {
		t.Errorf("complete cells = %s", n)
	}
}

func TestEgressGuardBlocksPrivateDestinations(t *testing.T) {
	e := newEnv(t)
	ws := e.ws("egress")
	// A client without the test override refuses loopback, like the Python url_guard.
	strict := NewWorker(e.app, loadConnectors(t, e.dir), mustStrictClient(t), nil, 5000)
	cols := []colCfg{enrichCol("email", "it_free", "email")}
	rows := e.workbook(ws, "wb-"+ws, 0, cols, "x.example")
	if err := strict.Handle(context.Background(), e.job(ws, e.payload(ws, "wb-"+ws, cols, rows, nil))); err != nil {
		t.Fatal(err)
	}
	if e.sim.count("") != 0 {
		t.Error("the guard let a loopback request through")
	}
	if _, st, _, er := e.cell("wb-"+ws, rows[0], "email"); st != "error" || er != "no_data" {
		t.Errorf("cell = %s %s", st, er)
	}
}

func TestPreflightLeaseAndPausedWorkbook(t *testing.T) {
	e := newEnv(t)
	ws := e.ws("paused")
	cols := []colCfg{enrichCol("email", "it_free", "email")}
	rows := e.workbook(ws, "wb-"+ws, 0, cols, "x.example", "y.example")
	e.exec(`UPDATE workbooks SET status = 'paused' WHERE id = $1`, "wb-"+ws)
	if err := e.w.Handle(context.Background(), e.job(ws, e.payload(ws, "wb-"+ws, cols, rows, nil))); err != nil {
		t.Fatal(err)
	}
	if e.sim.count("") != 0 {
		t.Error("a paused workbook made provider calls")
	}
	if st := e.scalar(`SELECT status FROM workbooks WHERE id = $1`, "wb-"+ws); st != "paused" {
		t.Errorf("status = %s", st)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(e.scalar(`SELECT payload->'execution_result' FROM jobs WHERE id = (SELECT max(id) FROM jobs WHERE workspace_id = $1)`, ws)), &res); err != nil {
		t.Fatal(err)
	}
	if res["stopped"] != true || res["completed"].(float64) != 0 || res["rows"].(float64) != 2 {
		t.Errorf("receipt = %v", res)
	}

	// A workbook paused while a run is in flight stops admission within the poll interval.
	ws2 := e.ws("pausing")
	e.sim.setDelay(150 * time.Millisecond)
	var sites []string
	for i := range 60 {
		sites = append(sites, fmt.Sprintf("p%d.example", i))
	}
	rows2 := e.workbook(ws2, "wb-"+ws2, 0, cols, sites...)
	done := make(chan error, 1)
	go func() {
		done <- e.w.Handle(context.Background(), e.job(ws2, e.payload(ws2, "wb-"+ws2, cols, rows2, map[string]any{"concurrency": 1})))
	}()
	e.eventually("the run to start", func() bool { return e.sim.count("") > 3 })
	e.exec(`UPDATE workbooks SET status = 'paused' WHERE id = $1`, "wb-"+ws2)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the run ignored the paused workbook")
	}
	if n := e.sim.count(""); n >= 60 {
		t.Errorf("all %d rows ran despite the pause", n)
	}
	if st := e.scalar(`SELECT status FROM workbooks WHERE id = $1`, "wb-"+ws2); st != "paused" {
		t.Errorf("a paused workbook was overwritten with %s", st)
	}
}

// --- lease loss, cancellation and crash recovery ----------------------------

// A job cancelled while a provider call is in flight must not commit that
// cell. A free provider has nothing to account, so nothing is written at all.
func TestCancelledJobWritesNothingAndStopsAdmission(t *testing.T) {
	e := newEnv(t)
	ws := e.ws("cancel")
	cols := []colCfg{enrichCol("email", "it_free", "email")}
	sites := []string{"block.example"}
	for i := range 30 {
		sites = append(sites, fmt.Sprintf("later%d.example", i))
	}
	rows := e.workbook(ws, "wb-"+ws, 0, cols, sites...)
	job := e.job(ws, e.payload(ws, "wb-"+ws, cols, rows, map[string]any{"concurrency": 1}))

	done := make(chan error, 1)
	go func() { done <- e.w.Handle(context.Background(), job) }()
	<-e.sim.started // the first provider call is in flight
	e.exec(`UPDATE jobs SET status = 'cancelled', error = 'stopped by user' WHERE id = $1`, job.ID)
	close(e.sim.release)
	select {
	case err := <-done:
		if !errors.Is(err, queue.ErrLeaseLost) {
			t.Errorf("handler returned %v, want ErrLeaseLost", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the handler ignored the cancellation")
	}
	if n := e.scalar(`SELECT count(*) FROM workbook_enrichments WHERE workbook_id = $1`, "wb-"+ws); n != "0" {
		t.Errorf("a cancelled run committed %s cells", n)
	}
	if n := e.sim.count(""); n != 1 {
		t.Errorf("admission continued after the cancellation: %d provider calls", n)
	}
	if st := e.scalar(`SELECT status FROM jobs WHERE id = $1`, job.ID); st != "cancelled" {
		t.Errorf("job status = %s", st)
	}
	if st := e.scalar(`SELECT status FROM workbooks WHERE id = $1`, "wb-"+ws); st != "running" {
		t.Errorf("a cancelled attempt must leave the workbook as it found it (running), got %s", st)
	}
}

// The lease is lost while a paid call is in flight. The answer was paid for, so
// it is recorded (settled, budget advanced), but no result is written.
func TestLeaseLostMidPaidCallStillAccountsTheAnswer(t *testing.T) {
	e := newEnv(t)
	ws := e.ws("paidlost")
	cols := []colCfg{enrichCol("paid", "it_paid", "email")}
	rows := e.workbook(ws, "wb-"+ws, 0, cols, "block.example")
	job := e.job(ws, e.payload(ws, "wb-"+ws, cols, rows, nil))

	done := make(chan error, 1)
	go func() { done <- e.w.Handle(context.Background(), job) }()
	<-e.sim.started
	e.exec(`UPDATE jobs SET status = 'cancelled' WHERE id = $1`, job.ID)
	close(e.sim.release)
	if err := <-done; !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("handler returned %v", err)
	}
	if st := e.scalar(`SELECT status FROM workbook_spend_attempts WHERE workbook_id = $1`, "wb-"+ws); st != "settled" {
		t.Errorf("the paid answer was not accounted: attempt %s", st)
	}
	if spent := e.scalar(`SELECT budget_spent_usd FROM workbooks WHERE id = $1`, "wb-"+ws); spent != "0.05" {
		t.Errorf("budget_spent_usd = %s", spent)
	}
	if n := e.scalar(`SELECT count(*) FROM workbook_enrichments WHERE workbook_id = $1`, "wb-"+ws); n != "0" {
		t.Errorf("a result was written without the lease: %s", n)
	}
}

// A crash (here: the attempt's context ends) while the vendor is being called
// leaves the attempt uncertain. The retry must not call the vendor again.
func TestCrashDuringPaidCallIsNeverRetried(t *testing.T) {
	e := newEnv(t)
	ws := e.ws("crash")
	cols := []colCfg{enrichCol("paid", "it_paid", "email")}
	rows := e.workbook(ws, "wb-"+ws, 0, cols, "block.example")
	job := e.job(ws, e.payload(ws, "wb-"+ws, cols, rows, nil))

	ctx, crash := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.w.Handle(ctx, job) }()
	<-e.sim.started
	crash()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("handler returned %v", err)
	}
	if st := e.scalar(`SELECT status FROM workbook_spend_attempts WHERE workbook_id = $1`, "wb-"+ws); st != "uncertain" {
		t.Fatalf("attempt after the crash = %s, want uncertain", st)
	}

	// The queue retries the job: no second vendor call, an explicit error, no charge.
	close(e.sim.release)
	if err := e.w.Handle(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if n := e.sim.count("block.example"); n != 1 {
		t.Errorf("the vendor was called %d times", n)
	}
	if _, st, _, er := e.cell("wb-"+ws, rows[0], "paid"); st != "error" || er != "attempt_not_dispatchable" {
		t.Errorf("cell = %s %s", st, er)
	}
	if spent := e.scalar(`SELECT budget_spent_usd FROM workbooks WHERE id = $1`, "wb-"+ws); spent != "0" {
		t.Errorf("budget_spent_usd = %s", spent)
	}
}

// A job re-run after a worker died between settling and committing the cell
// reuses the settled answer: no vendor call, no second charge.
func TestRerunAfterSettledAttemptReusesTheReceipt(t *testing.T) {
	e := newEnv(t)
	ws := e.ws("reuse")
	cols := []colCfg{enrichCol("paid", "it_paid", "email")}
	rows := e.workbook(ws, "wb-"+ws, 0, cols, "acme.example")
	job := e.job(ws, e.payload(ws, "wb-"+ws, cols, rows, nil))
	if err := e.w.Handle(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if e.sim.count("acme.example") != 1 || e.scalar(`SELECT budget_spent_usd FROM workbooks WHERE id = $1`, "wb-"+ws) != "0.05" {
		t.Fatal("setup: the first run did not settle")
	}
	// Simulate the lost commit: the settlement survived, the cell did not.
	e.exec(`DELETE FROM workbook_enrichments WHERE workbook_id = $1`, "wb-"+ws)
	e.exec(`UPDATE workbook_rows SET enrichments = '{}'::json WHERE id = $1`, rows[0])

	if err := e.w.Handle(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if n := e.sim.count("acme.example"); n != 1 {
		t.Errorf("the vendor was called again: %d", n)
	}
	if v, st, _, _ := e.cell("wb-"+ws, rows[0], "paid"); v != "info@acme.example" || st != "complete" {
		t.Errorf("cell = %q %s", v, st)
	}
	if spent := e.scalar(`SELECT budget_spent_usd FROM workbooks WHERE id = $1`, "wb-"+ws); spent != "0.05" {
		t.Errorf("charged twice: budget_spent_usd = %s", spent)
	}
	if n := e.scalar(`SELECT count(*) FROM workbook_spend_attempts WHERE workbook_id = $1`, "wb-"+ws); n != "1" {
		t.Errorf("attempts = %s", n)
	}
}

// The settlement, the budget and the cell result are one transaction: when the
// commit fails nothing of it survives, and the paid answer is then accounted
// exactly once on its own.
func TestResultAndSettlementCommitAtomically(t *testing.T) {
	e := newEnv(t)
	ws := e.ws("atomic")
	cols := []colCfg{enrichCol("paid", "it_paid", "email")}
	rows := e.workbook(ws, "wb-"+ws, 0, cols, "acme.example")
	job := e.job(ws, e.payload(ws, "wb-"+ws, cols, rows, nil))

	var inTx atomic.Int64
	e.w.afterCellWrite = func() error {
		if inTx.Add(1) == 1 {
			// Inside the transaction, after the settlement and the cell were written.
			return errors.New("injected commit failure")
		}
		return nil
	}
	if err := e.w.Handle(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	// The failed transaction left no complete cell and no half-written state; the
	// cell reports the failure, and the charge is recorded once.
	if _, st, _, er := e.cell("wb-"+ws, rows[0], "paid"); st != "error" || er != "cell_execution_failed" {
		t.Errorf("cell = %s %s", st, er)
	}
	if st := e.scalar(`SELECT status FROM workbook_spend_attempts WHERE workbook_id = $1`, "wb-"+ws); st != "settled" {
		t.Errorf("attempt = %s", st)
	}
	if spent := e.scalar(`SELECT budget_spent_usd FROM workbooks WHERE id = $1`, "wb-"+ws); spent != "0.05" {
		t.Errorf("budget_spent_usd = %s (settled twice or lost)", spent)
	}

	// And in the success case the three are visible together.
	ws2 := e.ws("atomic2")
	rows2 := e.workbook(ws2, "wb-"+ws2, 0, cols, "acme.example")
	e.w.afterCellWrite = nil
	if err := e.w.Handle(context.Background(), e.job(ws2, e.payload(ws2, "wb-"+ws2, cols, rows2, nil))); err != nil {
		t.Fatal(err)
	}
	got := e.scalar(`SELECT (SELECT status FROM workbook_enrichments WHERE workbook_id = $1) || '/' ||
		(SELECT status FROM workbook_spend_attempts WHERE workbook_id = $1) || '/' ||
		(SELECT budget_spent_usd FROM workbooks WHERE id = $1)`, "wb-"+ws2)
	if got != "complete/settled/0.05" {
		t.Errorf("committed state = %s", got)
	}
}

// The reservation is idempotent and conflicting inputs are refused, never
// minted as a second billable attempt.
func TestSpendLedgerIsIdempotentAndRejectsConflictingContracts(t *testing.T) {
	e := newEnv(t)
	ws := e.ws("ledger")
	cols := []colCfg{enrichCol("paid", "it_paid", "email")}
	rows := e.workbook(ws, "wb-"+ws, 0, cols, "acme.example")
	l := &ledger{pool: e.app, job: queue.Job{ID: 1}}
	key, _ := attemptKey(ws, "wb-"+ws, "job:1", fmt.Sprintf("row:%d", rows[0]), "paid", "it_paid")
	r := reservation{
		workspaceID: ws, workbookID: "wb-" + ws, runID: "job:1", rowIdentity: fmt.Sprintf("row:%d", rows[0]),
		columnID: "paid", provider: "it_paid", attemptKey: key, exposure: 50000, cellLimit: 50000,
		costBasis: obj("kind", "catalog_estimate"), operation: obj("inputs", obj("a", int64(1))),
	}
	first, err := l.reserve(context.Background(), r)
	if err != nil || !first.OK || first.Status != "reserved" {
		t.Fatalf("first reserve: %+v %v", first, err)
	}
	again, err := l.reserve(context.Background(), r)
	if err != nil || !again.OK || again.ID != first.ID {
		t.Fatalf("a retry must return the same attempt: %+v %v", again, err)
	}
	r.operation = obj("inputs", obj("a", int64(2)))
	if conflict, _ := l.reserve(context.Background(), r); conflict.OK || conflict.Reason != "contract_conflict" {
		t.Errorf("changed inputs = %+v", conflict)
	}
	r.exposure = 60000
	r.attemptKey = "different-key"
	if over, _ := l.reserve(context.Background(), r); over.OK || over.Reason != "cell_budget" {
		t.Errorf("the cell limit must bound the exposure: %+v", over)
	}
	// The ledger table is tenant-scoped under forced RLS: another workspace sees nothing.
	var n int
	if err := pgxTenantCount(e, "someone-else", &n); err != nil || n != 0 {
		t.Errorf("another tenant sees %d attempts (%v)", n, err)
	}
}

func pgxTenantCount(e *env, ws string, n *int) error {
	ctx := context.Background()
	tx, err := e.app.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, "SELECT set_config('app.workspace_id', $1, true)", ws); err != nil {
		return err
	}
	return tx.QueryRow(ctx, "SELECT count(*) FROM workbook_spend_attempts").Scan(n)
}

var _ pgx.Tx

// --- the real queue: routing, cutover and rollback -------------------------

func startQueue(t *testing.T, e *env) {
	t.Helper()
	reg := queue.NewRegistry()
	e.w.Register(reg)
	q := queue.New(e.app, reg, queue.Options{
		Concurrency: 1, IdlePoll: 20 * time.Millisecond, ClaimCheckInterval: 50 * time.Millisecond,
		ReapInterval: time.Hour, HandlerStopGrace: 3 * time.Second,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = q.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

func (e *env) setRoute(executor string) {
	e.t.Helper()
	e.exec(`INSERT INTO job_executor_routes (job_type, executor) VALUES ($1, $2)
		ON CONFLICT (job_type) DO UPDATE SET executor = EXCLUDED.executor`, JobType, executor)
}

func (e *env) enqueuePending(ws string, payload map[string]any) int64 {
	e.t.Helper()
	raw, _ := json.Marshal(payload)
	var id int64
	err := e.owner.QueryRow(context.Background(), `INSERT INTO jobs
		(type, payload, workspace_id, priority, status, created_at, next_run_at, max_retries, retry_count)
		VALUES ($1, $2::json, $3, 1, 'pending', LOCALTIMESTAMP, LOCALTIMESTAMP, 3, 0) RETURNING id`, JobType, string(raw), ws).Scan(&id)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

func TestRoutingCutoverAndRollbackThroughTheRealQueue(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	_, _ = e.owner.Exec(ctx, `DELETE FROM job_executor_routes WHERE job_type = $1`, JobType)
	t.Cleanup(func() { _, _ = e.owner.Exec(ctx, `DELETE FROM job_executor_routes WHERE job_type = $1`, JobType) })

	// The registrar contributes the handler and the type is listed as a switchable python default.
	reg, err := queue.Build(queue.Env{Pool: e.app})
	if err != nil {
		t.Fatal(err)
	}
	var listed bool
	for _, ty := range reg.Types() {
		listed = listed || ty == JobType
	}
	routes, err := queue.ListRoutes(ctx, e.app)
	if err != nil {
		t.Fatal(err)
	}
	var def bool
	for _, r := range routes {
		if r.JobType == JobType && r.Executor == "python" && r.Default {
			def = true
		}
	}
	if !listed || !def {
		t.Fatalf("registered=%v switchable python default=%v", listed, def)
	}
	if got := queue.JobTimeouts[JobType]; got != 1800*time.Second {
		t.Errorf("timeout = %v", got)
	}

	ws := e.ws("route")
	cols := []colCfg{enrichCol("email", "it_free", "email")}
	rows := e.workbook(ws, "wb-"+ws, 0, cols, "acme.example")
	jobID := e.enqueuePending(ws, e.payload(ws, "wb-"+ws, cols, rows, nil))
	startQueue(t, e)

	time.Sleep(400 * time.Millisecond)
	if st := e.scalar(`SELECT status FROM jobs WHERE id = $1`, jobID); st != "pending" {
		t.Fatalf("an unrouted job was claimed by the go worker: %s", st)
	}

	e.setRoute("go") // cut over
	e.eventually("the go worker to complete the job", func() bool {
		return e.scalar(`SELECT status FROM jobs WHERE id = $1`, jobID) == "completed"
	})
	if v, st, _, _ := e.cell("wb-"+ws, rows[0], "email"); v != "info@acme.example" || st != "complete" {
		t.Errorf("cell = %q %s", v, st)
	}
	if st := e.scalar(`SELECT status FROM workbooks WHERE id = $1`, "wb-"+ws); st != "complete" {
		t.Errorf("workbook = %s", st)
	}
	if got := e.scalar(`SELECT payload->'execution_result'->>'completed' FROM jobs WHERE id = $1`, jobID); got != "1" {
		t.Errorf("run receipt = %s", got)
	}

	e.setRoute("python") // roll back
	ws2 := e.ws("route2")
	rows2 := e.workbook(ws2, "wb-"+ws2, 0, cols, "later.example")
	job2 := e.enqueuePending(ws2, e.payload(ws2, "wb-"+ws2, cols, rows2, nil))
	time.Sleep(400 * time.Millisecond)
	if st := e.scalar(`SELECT status FROM jobs WHERE id = $1`, job2); st != "pending" {
		t.Fatalf("after rollback the go worker still claimed the job: %s", st)
	}
	if e.sim.count("later.example") != 0 {
		t.Error("a provider was called after the rollback")
	}
}

// A real cancellation through the queue: the lease monitor cancels the
// handler's context, so admission stops, and the cancelled status is kept.
func TestQueueCancellationStopsTheRun(t *testing.T) {
	e := newEnv(t)
	e.setRoute("go")
	t.Cleanup(func() { e.exec(`DELETE FROM job_executor_routes WHERE job_type = $1`, JobType) })
	ws := e.ws("qcancel")
	cols := []colCfg{enrichCol("email", "it_free", "email")}
	sites := []string{"block.example"}
	for i := range 20 {
		sites = append(sites, fmt.Sprintf("q%d.example", i))
	}
	rows := e.workbook(ws, "wb-"+ws, 0, cols, sites...)
	jobID := e.enqueuePending(ws, e.payload(ws, "wb-"+ws, cols, rows, map[string]any{"concurrency": 1}))
	startQueue(t, e)

	<-e.sim.started
	e.exec(`UPDATE jobs SET status = 'cancelled', error = 'stopped by user' WHERE id = $1`, jobID)
	// the monitor notices within ClaimCheckInterval and cancels the handler's context
	e.eventually("the in-flight provider call to be hung up", func() bool { return e.sim.inflight.Load() == 0 })
	time.Sleep(300 * time.Millisecond)
	if n := e.sim.count(""); n != 1 {
		t.Errorf("provider calls after the cancellation: %d", n)
	}
	if st := e.scalar(`SELECT status FROM jobs WHERE id = $1`, jobID); st != "cancelled" {
		t.Errorf("job = %s", st)
	}
	if n := e.scalar(`SELECT count(*) FROM workbook_enrichments WHERE workbook_id = $1`, "wb-"+ws); n != "0" {
		t.Errorf("cells written after the cancellation: %s", n)
	}
}

// Shutdown mid-run: the attempt is left for heartbeat recovery with the
// workbook still `running`, and the next attempt runs everything again.
func TestInterruptedAttemptLeavesRunningAndRetryCompletes(t *testing.T) {
	e := newEnv(t)
	ws := e.ws("resume")
	cols := []colCfg{enrichCol("email", "it_free", "email")}
	sites := []string{}
	for i := range 40 {
		sites = append(sites, fmt.Sprintf("r%d.example", i))
	}
	e.sim.setDelay(60 * time.Millisecond)
	rows := e.workbook(ws, "wb-"+ws, 0, cols, sites...)
	job := e.job(ws, e.payload(ws, "wb-"+ws, cols, rows, map[string]any{"concurrency": 2}))

	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.w.Handle(ctx, job) }()
	e.eventually("some cells to commit", func() bool {
		return e.scalar(`SELECT count(*) FROM workbook_enrichments WHERE workbook_id = $1`, "wb-"+ws) != "0"
	})
	stop()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("handler returned %v", err)
	}
	if st := e.scalar(`SELECT status FROM workbooks WHERE id = $1`, "wb-"+ws); st != "running" {
		t.Errorf("an interrupted attempt must not finalize the workbook: %s", st)
	}

	e.sim.setDelay(0)
	if err := e.w.Handle(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if n := e.scalar(`SELECT count(*) FROM workbook_enrichments WHERE workbook_id = $1 AND status = 'complete'`, "wb-"+ws); n != "40" {
		t.Errorf("complete cells after the retry = %s", n)
	}
	if st := e.scalar(`SELECT status FROM workbooks WHERE id = $1`, "wb-"+ws); st != "complete" {
		t.Errorf("workbook = %s", st)
	}
}

func TestRetryPassRerunsOnlyErrorCells(t *testing.T) {
	e := newEnv(t)
	ws := e.ws("retry")
	cols := []colCfg{enrichCol("email", "it_free", "email")}
	rows := e.workbook(ws, "wb-"+ws, 0, cols, "err500.example", "ok.example")
	payload := e.payload(ws, "wb-"+ws, cols, rows, map[string]any{"retry_passes": 2})
	if err := e.w.Handle(context.Background(), e.job(ws, payload)); err != nil {
		t.Fatal(err)
	}
	if n := e.sim.count("ok.example"); n != 1 {
		t.Errorf("a completed cell was run again: %d", n)
	}
	// 1 first pass + 2 retry passes
	if n := e.sim.count("err500.example"); n != 3 {
		t.Errorf("error cell attempts = %d, want 3", n)
	}
	if st := e.scalar(`SELECT status FROM workbooks WHERE id = $1`, "wb-"+ws); st != "failed" {
		t.Errorf("a run with error cells must end failed, got %s", st)
	}
}
