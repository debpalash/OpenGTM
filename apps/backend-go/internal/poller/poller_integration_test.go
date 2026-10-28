//go:build integration

package poller_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/poller"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

const (
	pfx  = "p08t9-"
	wsA  = pfx + "a"
	wsB  = pfx + "b"
	wID  = pfx + "w1"
	tNow = "2026-08-29T10:40:00+00:00"
)

var (
	vNow    = time.Date(2026, 8, 29, 10, 40, 0, 0, time.UTC)
	vAnchor = time.Date(2026, 8, 29, 8, 0, 0, 0, time.UTC)
	cfg     = poller.Config{DefaultInterval: "daily", MaxConsecutiveFailures: 12}
)

func setup(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	pool := testpg.NewAppPool(t, 40)
	ctx := context.Background()
	clean := func() {
		for _, w := range []string{wsA, wsB, pfx + "1", pfx + "2", pfx + "3"} {
			_ = tenancy.InWorkspace(ctx, pool, w, func(tx pgx.Tx) error {
				_, _ = tx.Exec(ctx, `DELETE FROM watch_subscriptions`)
				_, _ = tx.Exec(ctx, `DELETE FROM poll_budget_ledger`)
				_, _ = tx.Exec(ctx, `DELETE FROM jobs WHERE fire_key LIKE 'watch:p08t9-%' OR workspace_id = $1 OR payload->>'workspace_id' = $1`, w)
				_, err := tx.Exec(ctx, `DELETE FROM watch_schedules WHERE watch_id LIKE 'p08t9-%'`)
				return err
			})
		}
	}
	clean()
	t.Cleanup(clean)
	return pool, ctx
}

type seed struct {
	ID, WS   string
	Interval *string
	Anchor   *time.Time
	Enabled  bool
	Fails    int
	LastErr  *string
}

func (s seed) insert(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	err := tenancy.InWorkspace(ctx, pool, s.WS, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO watch_subscriptions (id, workspace_id, kind, target, "interval", schedule_anchor, enabled, consecutive_failures, last_error, cursor)
			VALUES ($1,$2,'funding','Acme',$3,$4,$5,$6,$7,'{"bootstrapped": false}'::json)`,
			s.ID, s.WS, s.Interval, s.Anchor, s.Enabled, s.Fails, s.LastErr)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func sp(s string) *string { return &s }

func inTx(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ws string, fn func(tx pgx.Tx) error) {
	t.Helper()
	if err := tenancy.InWorkspace(ctx, pool, ws, fn); err != nil {
		t.Fatal(err)
	}
}

type jobRow struct {
	ID                              int64
	Executor, Type, FireKey, Status string
	Payload                         string
	Priority, MaxRetries            int
	NextRunAt                       time.Time
}

func jobs(t *testing.T, ctx context.Context, pool *pgxpool.Pool, like string) []jobRow {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT id, executor, type, fire_key, status, payload::text, priority, max_retries, next_run_at
		FROM jobs WHERE fire_key LIKE $1 ORDER BY id`, like)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []jobRow
	for rows.Next() {
		var j jobRow
		if err := rows.Scan(&j.ID, &j.Executor, &j.Type, &j.FireKey, &j.Status, &j.Payload, &j.Priority, &j.MaxRetries, &j.NextRunAt); err != nil {
			t.Fatal(err)
		}
		out = append(out, j)
	}
	return out
}

func keyOrder(t *testing.T, payload string) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(payload))
	if _, err := dec.Token(); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for dec.More() {
		k, _ := dec.Token()
		keys = append(keys, k.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

func TestFireKeyAndPayloadEqualPythonJobRow(t *testing.T) {
	pool, ctx := setup(t)
	next := time.Date(2026, 8, 29, 11, 0, 0, 0, time.UTC)
	key := poller.ScheduledFireKey(wID, next)
	var created bool
	inTx(t, ctx, pool, wsA, func(tx pgx.Tx) (err error) {
		created, err = poller.EnqueuePollIfAbsent(ctx, tx, key, wsA, wID, next, false, "go")
		return
	})
	if !created {
		t.Fatal("not created")
	}
	js := jobs(t, ctx, pool, "watch:"+wID+":%")
	if len(js) != 1 {
		t.Fatalf("jobs = %d", len(js))
	}
	j := js[0]
	if j.Type != "watch_poll" || j.Executor != "go" || j.Status != "pending" || j.Priority != 1 || j.MaxRetries != 3 || j.FireKey != key || !j.NextRunAt.Equal(next) {
		t.Fatalf("job = %+v", j)
	}
	var p map[string]any
	_ = json.Unmarshal([]byte(j.Payload), &p)
	if len(p) != 3 || p["watch_id"] != wID || p["workspace_id"] != wsA || p["fire_key"] != key {
		t.Fatalf("payload = %s", j.Payload)
	}
	if got := strings.Join(keyOrder(t, j.Payload), ","); got != "watch_id,workspace_id,fire_key" {
		t.Fatalf("key order = %s", got)
	}
}

func TestPollNowFlagSetInSameInsert(t *testing.T) {
	pool, ctx := setup(t)
	key := fmt.Sprintf("watch:%s:pollnow:2026-08-29", wID)
	inTx(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		ok, err := poller.EnqueuePollIfAbsent(ctx, tx, key, wsA, wID, vNow, true, "go")
		if err != nil || !ok {
			t.Fatalf("ok=%v err=%v", ok, err)
		}
		// Visible within the very same transaction, with no follow-up write.
		var payload string
		var xmin, xmax string
		if err := tx.QueryRow(ctx, `SELECT payload::text, xmin::text, xmax::text FROM jobs WHERE fire_key=$1`, key).Scan(&payload, &xmin, &xmax); err != nil {
			t.Fatal(err)
		}
		if xmax != "0" {
			t.Errorf("job row was updated after insert (xmax=%s)", xmax)
		}
		var p map[string]any
		_ = json.Unmarshal([]byte(payload), &p)
		if p["poll_now"] != true || p["fire_key"] != key {
			t.Errorf("payload = %s", payload)
		}
		return nil
	})
	js := jobs(t, ctx, pool, key)
	if len(js) != 1 || js[0].Executor != "go" || strings.Join(keyOrder(t, js[0].Payload), ",") != "watch_id,workspace_id,fire_key,poll_now" {
		t.Fatalf("jobs = %+v", js)
	}
}

func TestEnqueuePollSingleFlightAcrossGoAndPython(t *testing.T) {
	pool, ctx := setup(t)
	next := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	key := poller.ScheduledFireKey(wID, next)

	// 1. Many concurrent Go enqueuers: exactly one wins.
	var mu sync.Mutex
	wins := 0
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = tenancy.InWorkspace(ctx, pool, wsA, func(tx pgx.Tx) error {
				ok, err := poller.EnqueuePollIfAbsent(ctx, tx, key, wsA, wID, next, false, "go")
				if err != nil {
					t.Error(err)
				}
				if ok {
					mu.Lock()
					wins++
					mu.Unlock()
				}
				return err
			})
		}()
	}
	wg.Wait()
	if wins != 1 || len(jobs(t, ctx, pool, key)) != 1 {
		t.Fatalf("wins=%d jobs=%d", wins, len(jobs(t, ctx, pool, key)))
	}

	// 2. A Python-style enqueuer holds the same advisory lock text and inserts
	// first; Go must wait for it and then see the active row.
	next2 := next.Add(time.Hour)
	key2 := poller.ScheduledFireKey(wID, next2)
	py, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := py.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "watch_poll:"+key2); err != nil {
		t.Fatal(err)
	}
	done := make(chan bool, 1)
	go func() {
		var ok bool
		_ = tenancy.InWorkspace(ctx, pool, wsA, func(tx pgx.Tx) (e error) {
			ok, e = poller.EnqueuePollIfAbsent(ctx, tx, key2, wsA, wID, next2, false, "go")
			return
		})
		done <- ok
	}()
	select {
	case <-done:
		t.Fatal("Go enqueuer did not wait on the Python advisory lock")
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := py.Exec(ctx, `INSERT INTO jobs (executor, type, payload, status, priority, fire_key, next_run_at, max_retries)
		VALUES ('python','watch_poll','{"watch_id": "x"}'::json,'pending',1,$1,(now() at time zone 'utc'),3)`, key2); err != nil {
		t.Fatal(err)
	}
	if err := py.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if ok := <-done; ok {
		t.Fatal("Go enqueued a second active job for the same fire key")
	}
	if js := jobs(t, ctx, pool, key2); len(js) != 1 || js[0].Executor != "python" {
		t.Fatalf("jobs = %+v", js)
	}
}

func TestMirrorWrittenInSameTx(t *testing.T) {
	pool, ctx := setup(t)
	seed{ID: wID, WS: wsA, Interval: sp("daily"), Anchor: &vAnchor, Enabled: true}.insert(t, ctx, pool)
	mirror := func() (n int, enabled bool) {
		_ = pool.QueryRow(ctx, `SELECT count(*), COALESCE(bool_and(enabled), false) FROM watch_schedules WHERE watch_id=$1`, wID).Scan(&n, &enabled)
		return
	}
	// Rolled-back transaction leaves neither the job, the mirror nor the watch change.
	err := tenancy.InWorkspace(ctx, pool, wsA, func(tx pgx.Tx) error {
		if _, err := poller.RescheduleWatch(ctx, tx, loadWatch(t, ctx, tx, wsA), false, vNow, cfg, "go"); err != nil {
			return err
		}
		return fmt.Errorf("boom")
	})
	if err == nil {
		t.Fatal("expected rollback error")
	}
	if n, _ := mirror(); n != 0 || len(jobs(t, ctx, pool, "watch:"+wID+":%")) != 0 {
		t.Fatal("rolled-back reschedule leaked rows")
	}
	inTx(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		_, err := poller.RescheduleWatch(ctx, tx, loadWatch(t, ctx, tx, wsA), false, vNow, cfg, "go")
		return err
	})
	if n, en := mirror(); n != 1 || !en || len(jobs(t, ctx, pool, "watch:"+wID+":%")) != 1 {
		t.Fatalf("mirror rows=%d enabled=%v", n, en)
	}
	inTx(t, ctx, pool, wsA, func(tx pgx.Tx) error { return poller.MirrorDelete(ctx, tx, wID) })
	if n, _ := mirror(); n != 0 {
		t.Fatal("MirrorDelete left the row")
	}
}

func loadWatch(t *testing.T, ctx context.Context, tx pgx.Tx, ws string) poller.Watch {
	t.Helper()
	w, ok, err := poller.LoadWatch(ctx, tx, wID, ws)
	if err != nil || !ok {
		t.Fatalf("LoadWatch ok=%v err=%v", ok, err)
	}
	return w
}

// ---- budget ----

func TestDebitBudgetConcurrentIncrements(t *testing.T) {
	pool, ctx := setup(t)
	day := time.Date(2026, 8, 29, 23, 59, 59, 0, time.UTC)
	var mu sync.Mutex
	var seen []int
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := tenancy.InWorkspace(ctx, pool, wsA, func(tx pgx.Tx) error {
				n, _, err := poller.DebitBudget(ctx, tx, wsA, false, day)
				if err == nil {
					mu.Lock()
					seen = append(seen, n)
					mu.Unlock()
				}
				return err
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	sort.Ints(seen)
	for i, n := range seen {
		if n != i+1 {
			t.Fatalf("increments not unique 1..32: %v", seen)
		}
	}
	if len(seen) != 32 {
		t.Fatalf("got %d", len(seen))
	}
	var final int
	inTx(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT n FROM poll_budget_ledger WHERE workspace_id=$1`, wsA).Scan(&final)
	})
	if final != 32 {
		t.Fatalf("final n = %d", final)
	}
}

func TestPollNowCountsBothCounters(t *testing.T) {
	pool, ctx := setup(t)
	var v struct {
		Budget []map[string]any `json:"budget"`
	}
	readSched(t, &v)
	today := vNow
	ws := func(s any) string { return pfx + strings.TrimPrefix(s.(string), "ws-") }
	steps := 0
	for _, st := range v.Budget {
		switch {
		case st["day"] != nil:
			d, err := time.Parse(time.RFC3339, st["day"].(string))
			if err != nil {
				t.Fatal(err)
			}
			today = d
		case st["quota"] != nil:
			var used int
			inTx(t, ctx, pool, ws(st["quota"]), func(tx pgx.Tx) (err error) {
				used, err = poller.PollNowQuotaUsed(ctx, tx, ws(st["quota"]), today)
				return
			})
			if float64(used) != st["used"].(float64) {
				t.Errorf("quota %v used = %d want %v", st["quota"], used, st["used"])
			}
		default:
			var n, pn int
			inTx(t, ctx, pool, ws(st["ws"]), func(tx pgx.Tx) (err error) {
				n, pn, err = poller.DebitBudget(ctx, tx, ws(st["ws"]), st["poll_now"].(bool), today)
				return
			})
			if float64(n) != st["n"].(float64) || float64(pn) != st["n_poll_now"].(float64) {
				t.Errorf("step %v: got (%d,%d)", st, n, pn)
			}
			steps++
		}
	}
	if steps != 8 {
		t.Fatalf("steps = %d", steps)
	}
}

func TestDebitSourceGate(t *testing.T) {
	pool, ctx := setup(t)
	c := poller.Config{FundingCostUSD: 0.5, BillingEnabled: true}
	// Free source (cost 0) and billing-disabled never touch credits.
	inTx(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		ok, err := poller.DebitSource(ctx, tx, poller.Config{BillingEnabled: true}, wsA, "funding", "watch:x:1")
		if err != nil || !ok {
			t.Fatalf("free source ok=%v err=%v", ok, err)
		}
		ok, err = poller.DebitSource(ctx, tx, poller.Config{FundingCostUSD: 1}, wsA, "funding", "watch:x:1")
		if err != nil || !ok {
			t.Fatalf("billing disabled ok=%v err=%v", ok, err)
		}
		// no credits: refusal is (false, nil), not an error.
		ok, err = poller.DebitSource(ctx, tx, c, wsA, "funding", "watch:x:1")
		if err != nil || ok {
			t.Fatalf("no credits ok=%v err=%v", ok, err)
		}
		// unknown/free source kinds are never charged
		ok, err = poller.DebitSource(ctx, tx, c, wsA, "web_tech", "watch:x:1")
		if err != nil || !ok {
			t.Fatalf("web_tech ok=%v err=%v", ok, err)
		}
		return nil
	})
	// With a balance: charged once per (fire key, billing source); replay is free.
	inTx(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO workspace_credits (workspace_id, balance_usd) VALUES ($1, 1.0)
			ON CONFLICT (workspace_id) DO UPDATE SET balance_usd = 1.0`, wsA); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 5; i++ {
			ok, err := poller.DebitSource(ctx, tx, c, wsA, "funding", "watch:x:2")
			if err != nil || !ok {
				t.Fatalf("debit %d ok=%v err=%v", i, ok, err)
			}
		}
		// funding, exec and funding_exec share one billing source (and run id) per poll.
		for _, src := range []string{"exec", "funding_exec"} {
			if ok, err := poller.DebitSource(ctx, tx, c, wsA, src, "watch:x:2"); err != nil || !ok {
				t.Fatalf("%s ok=%v err=%v", src, ok, err)
			}
		}
		var bal float64
		_ = tx.QueryRow(ctx, `SELECT balance_usd FROM workspace_credits WHERE workspace_id=$1`, wsA).Scan(&bal)
		if bal != 0.5 {
			t.Errorf("balance = %v, want 0.5 (one charge)", bal)
		}
		// a distinct poll charges again, then the balance is exhausted: refusal, not error.
		if ok, err := poller.DebitSource(ctx, tx, c, wsA, "funding", "watch:x:3"); err != nil || !ok {
			t.Fatalf("second poll ok=%v err=%v", ok, err)
		}
		if ok, err := poller.DebitSource(ctx, tx, c, wsA, "funding", "watch:x:4"); err != nil || ok {
			t.Fatalf("exhausted ok=%v err=%v", ok, err)
		}
		return nil
	})
	inTx(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM credit_ledger_entries WHERE workspace_id=$1`, wsA)
		if err == nil {
			_, err = tx.Exec(ctx, `DELETE FROM workspace_credits WHERE workspace_id=$1`, wsA)
		}
		return err
	})
}

// ---- reschedule + reconcile ----

type reschedVec struct {
	Name        string  `json:"name"`
	MaxFailures int     `json:"max_failures"`
	Interval    *string `json:"interval"`
	Before      *int    `json:"failures_before"`
	Failed      bool    `json:"failed"`
	Fails       int     `json:"consecutive_failures"`
	Enabled     bool    `json:"enabled"`
	LastError   *string `json:"last_error"`
	NextPollAt  *string `json:"next_poll_at"`
	Enqueued    any     `json:"enqueued"`
	Mirror      []struct {
		Next    *string `json:"next_poll_at"`
		Enabled bool    `json:"enabled"`
	} `json:"mirror"`
}

func readSched(t *testing.T, v any) {
	t.Helper()
	b, err := readFile("testdata/schedule_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

func isoOrNil(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format("2006-01-02T15:04:05") + "+00:00"
	return &s
}

func TestRescheduleBackoffTable(t *testing.T) {
	pool, ctx := setup(t)
	var v struct {
		Reschedule []reschedVec `json:"reschedule"`
	}
	readSched(t, &v)
	if len(v.Reschedule) != 89 {
		t.Fatalf("vectors = %d", len(v.Reschedule))
	}
	for i, c := range v.Reschedule {
		if c.Name == "no_anchor_uses_now" {
			c = shiftNoAnchorVector(t, c)
		}
		anchor := &vAnchor
		enabled := true
		before := 0
		if c.Before != nil {
			before = *c.Before
		}
		maxF := 12
		if c.MaxFailures > 0 {
			maxF = c.MaxFailures
			before = maxF - 1
		}
		switch c.Name {
		case "no_anchor_uses_now":
			anchor = nil
		case "naive_anchor":
			a := time.Date(2026, 8, 29, 8, 20, 0, 0, time.UTC)
			anchor = &a
		case "disabled_watch_not_enqueued":
			enabled = false
		}
		tag := fmt.Sprintf("%d %s %v/%d", i, c.Name, c.Interval, before)
		cleanAll(t, ctx, pool)
		seed{ID: wID, WS: wsA, Interval: c.Interval, Anchor: anchor, Enabled: enabled, Fails: before, LastErr: sp("http_500")}.insert(t, ctx, pool)
		cf := cfg
		cf.MaxConsecutiveFailures = maxF
		var got poller.Watch
		inTx(t, ctx, pool, wsA, func(tx pgx.Tx) (err error) {
			got, err = poller.RescheduleWatch(ctx, tx, loadWatch(t, ctx, tx, wsA), c.Failed, vNow, cf, "go")
			return
		})
		if got.ConsecutiveFailures != c.Fails || got.Enabled != c.Enabled {
			t.Errorf("%s: got fails=%d enabled=%v", tag, got.ConsecutiveFailures, got.Enabled)
		}
		var db poller.Watch
		inTx(t, ctx, pool, wsA, func(tx pgx.Tx) error { db = loadWatch(t, ctx, tx, wsA); return nil })
		gn, wn := isoOrNil(db.NextPollAt), c.NextPollAt
		if (gn == nil) != (wn == nil) || (gn != nil && *gn != *wn) {
			t.Errorf("%s: next_poll_at got %v want %v", tag, deref(gn), deref(wn))
		}
		if c.LastError != nil && (db.LastError == nil || *db.LastError != *c.LastError) {
			t.Errorf("%s: last_error = %v want %s", tag, db.LastError, *c.LastError)
		}
		if db.ConsecutiveFailures != c.Fails || db.Enabled != c.Enabled {
			t.Errorf("%s: persisted fails=%d enabled=%v", tag, db.ConsecutiveFailures, db.Enabled)
		}
		js := jobs(t, ctx, pool, "watch:"+wID+":%")
		switch e := c.Enqueued.(type) {
		case []any:
			if len(js) != len(e) {
				t.Errorf("%s: jobs = %d want %d", tag, len(js), len(e))
				break
			}
			for k, raw := range e {
				m := raw.(map[string]any)
				wantKey := strings.Replace(m["fire_key"].(string), "w-1", wID, 1)
				if js[k].FireKey != wantKey || js[k].Executor != "go" { // the vector's executor is Python's
					t.Errorf("%s: job %+v want %v", tag, js[k], m)
				}
				var p map[string]any
				_ = json.Unmarshal([]byte(js[k].Payload), &p)
				if p["workspace_id"] != wsA || p["watch_id"] != wID || p["fire_key"] != wantKey {
					t.Errorf("%s: payload %s", tag, js[k].Payload)
				}
			}
		default:
			if len(js) != 0 {
				t.Errorf("%s: jobs = %d want 0", tag, len(js))
			}
		}
		var mn int
		var men bool
		var mnext *time.Time
		_ = pool.QueryRow(ctx, `SELECT count(*), COALESCE(bool_and(enabled), false), max(next_poll_at) FROM watch_schedules WHERE watch_id=$1`, wID).Scan(&mn, &men, &mnext)
		if mn != len(c.Mirror) {
			t.Errorf("%s: mirror rows %d want %d", tag, mn, len(c.Mirror))
		} else if mn == 1 {
			gn := isoOrNil(mnext)
			wn := c.Mirror[0].Next
			if men != c.Mirror[0].Enabled || (gn == nil) != (wn == nil) || (gn != nil && *gn != *wn) {
				t.Errorf("%s: mirror (%v,%v) want %+v", tag, men, deref(gn), c.Mirror[0])
			}
		}
	}
}

// shiftNoAnchorVector adapts the recorded vector, which was captured with a
// frozen Python clock (anchor == now, so next == now). Real Python reads the
// clock again inside compute_next_run, so a NULL anchor yields now + one
// interval (daily here); a due-now result would respin the schedule forever.
func shiftNoAnchorVector(t *testing.T, c reschedVec) reschedVec {
	t.Helper()
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.ReplaceAll(string(raw), "2026-08-29T10:40:00", "2026-08-30T10:40:00"))
	var out reschedVec
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func cleanAll(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	inTx(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		_, _ = tx.Exec(ctx, `DELETE FROM watch_subscriptions`)
		_, _ = tx.Exec(ctx, `DELETE FROM jobs WHERE fire_key LIKE 'watch:p08t9-%' OR id = 7`)
		_, err := tx.Exec(ctx, `DELETE FROM watch_schedules WHERE watch_id LIKE 'p08t9-%'`)
		return err
	})
}

func TestRescheduleDisabledNeverEnqueues(t *testing.T) {
	pool, ctx := setup(t)
	seed{ID: wID, WS: wsA, Interval: sp("hourly"), Anchor: &vAnchor, Enabled: false, Fails: 3}.insert(t, ctx, pool)
	for _, failed := range []bool{false, true} {
		inTx(t, ctx, pool, wsA, func(tx pgx.Tx) error {
			_, err := poller.RescheduleWatch(ctx, tx, loadWatch(t, ctx, tx, wsA), failed, vNow, cfg, "go")
			return err
		})
		var n int
		_ = pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM jobs WHERE fire_key LIKE 'watch:p08t9-%') + (SELECT count(*) FROM watch_schedules WHERE watch_id=$1)`, wID).Scan(&n)
		if n != 0 {
			t.Fatalf("failed=%v: disabled watch produced %d job/mirror rows", failed, n)
		}
	}
}

type recVec struct {
	Name      string            `json:"name"`
	Payload   map[string]string `json:"payload"`
	Error     *string           `json:"error"`
	WillRetry bool              `json:"will_retry"`
	JobNext   string            `json:"job_next_run_at"`
	JobBefore string            `json:"job_status_before"`
	Raised    *struct {
		Message string `json:"message"`
	} `json:"raised"`
	Watch *struct {
		Enabled   bool    `json:"enabled"`
		LastError *string `json:"last_error"`
		Fails     int     `json:"consecutive_failures"`
		Next      *string `json:"next_poll_at"`
	} `json:"watch"`
	Mirror *struct {
		Enabled bool    `json:"enabled"`
		Next    *string `json:"next_poll_at"`
	} `json:"mirror"`
	Job *struct {
		Status    string  `json:"status"`
		Error     *string `json:"error"`
		Completed bool    `json:"completed"`
	} `json:"job"`
}

func recSeed(name string) (fails int, enabled, noWatch, noJob bool) {
	enabled = true
	switch name {
	case "retry_reaching_max_auto_disables_and_cancels_pending_job", "retry_reaching_max_job_already_processing_not_cancelled", "final_failure_at_max_auto_disables":
		fails = 11
	case "final_failure_reschedules_with_backoff":
		fails = 2
	case "disabled_watch_untouched":
		enabled = false
	case "missing_watch_untouched":
		noWatch = true
	case "retry_without_job_defaults_to_now_plus_one_minute":
		noJob = true
	}
	return
}

func TestReconcileRetryVsFinal(t *testing.T) {
	pool, ctx := setup(t)
	var v struct {
		Reconcile []recVec `json:"reconcile"`
	}
	readSched(t, &v)
	if len(v.Reconcile) != 16 {
		t.Fatalf("vectors = %d", len(v.Reconcile))
	}
	for _, c := range v.Reconcile {
		cleanAll(t, ctx, pool)
		fails, enabled, noWatch, noJob := recSeed(c.Name)
		if !noWatch {
			seed{ID: wID, WS: wsA, Interval: sp("daily"), Anchor: &vAnchor, Enabled: enabled, Fails: fails}.insert(t, ctx, pool)
			inTx(t, ctx, pool, wsA, func(tx pgx.Tx) error { return poller.MirrorSet(ctx, tx, wID, wsA, nil, true) })
		}
		jobNext, _ := time.Parse(time.RFC3339, c.JobNext)
		if !noJob {
			testpg.FreeIDs(t, "jobs", 7) // literal id, below testpg.SeqFloor
			_, err := pool.Exec(ctx, `INSERT INTO jobs (id, executor, type, payload, workspace_id, status, priority, fire_key, next_run_at, max_retries)
				VALUES (7,'python','watch_poll','{}'::json,$1,$2,1,'watch:p08t9-job7',$3,3)`, wsA, c.JobBefore, jobNext.UTC())
			if err != nil {
				t.Fatal(err)
			}
		}
		payload := map[string]string{}
		for k, val := range c.Payload {
			if k == "watch_id" && val == "w-1" {
				val = wID
			}
			if k == "workspace_id" && val == "ws-1" {
				val = wsA
			}
			if k == "workspace_id" && val == "ws-2" {
				val = wsB
			}
			payload[k] = val
		}
		pb, _ := json.Marshal(payload)
		reason := ""
		if c.Error != nil {
			reason = *c.Error
		}
		d := poller.Deps{Pool: pool, Cfg: cfg, Now: func() time.Time { return vNow }, ExecutorFor: func(string) string { return "go" }}
		err := poller.Reconcile(ctx, d, queue.FailureNotice{JobID: 7, JobType: "watch_poll", WorkspaceID: wsA, Payload: pb, Reason: reason, WillRetry: c.WillRetry})
		if c.Raised != nil {
			if err == nil || err.Error() != c.Raised.Message {
				t.Errorf("%s: err = %v want %q", c.Name, err, c.Raised.Message)
			}
		} else if err != nil {
			t.Errorf("%s: err = %v", c.Name, err)
		}
		if c.Watch != nil {
			var w poller.Watch
			inTx(t, ctx, pool, wsA, func(tx pgx.Tx) error { w = loadWatch(t, ctx, tx, wsA); return nil })
			gn := isoOrNil(w.NextPollAt)
			if w.Enabled != c.Watch.Enabled || w.ConsecutiveFailures != c.Watch.Fails || deref(w.LastError) != deref(c.Watch.LastError) || deref(gn) != deref(c.Watch.Next) {
				t.Errorf("%s: watch = enabled %v fails %d err %q next %s; want %+v", c.Name, w.Enabled, w.ConsecutiveFailures, deref(w.LastError), deref(gn), *c.Watch)
			}
		}
		if c.Mirror != nil {
			var men bool
			var mnext *time.Time
			_ = pool.QueryRow(ctx, `SELECT enabled, next_poll_at FROM watch_schedules WHERE watch_id=$1`, wID).Scan(&men, &mnext)
			if men != c.Mirror.Enabled || deref(isoOrNil(mnext)) != deref(c.Mirror.Next) {
				t.Errorf("%s: mirror = %v %s want %+v", c.Name, men, deref(isoOrNil(mnext)), *c.Mirror)
			}
		}
		if c.Job != nil {
			var st string
			var errTxt *string
			var completed bool
			if err := pool.QueryRow(ctx, `SELECT status, error, completed_at IS NOT NULL FROM jobs WHERE id=7`).Scan(&st, &errTxt, &completed); err != nil {
				t.Errorf("%s: job: %v", c.Name, err)
				continue
			}
			if st != c.Job.Status || deref(errTxt) != deref(c.Job.Error) || completed != c.Job.Completed {
				t.Errorf("%s: job = %s %q %v want %+v", c.Name, st, deref(errTxt), completed, *c.Job)
			}
		}
	}
}

func TestAutoDisableCancelsPendingRetry(t *testing.T) {
	pool, ctx := setup(t)
	seed{ID: wID, WS: wsA, Interval: sp("daily"), Anchor: &vAnchor, Enabled: true, Fails: 11}.insert(t, ctx, pool)
	inTx(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		ok, err := poller.EnqueuePollIfAbsent(ctx, tx, "watch:"+wID+":retry", wsA, wID, vNow.Add(time.Minute), false, "go")
		if !ok {
			t.Fatal("enqueue")
		}
		return err
	})
	js := jobs(t, ctx, pool, "watch:"+wID+":retry")
	d := poller.Deps{Pool: pool, Cfg: cfg, Now: func() time.Time { return vNow }, ExecutorFor: func(string) string { return "go" }}
	pb, _ := json.Marshal(map[string]string{"workspace_id": wsA, "watch_id": wID})
	if err := poller.Reconcile(ctx, d, queue.FailureNotice{JobID: js[0].ID, JobType: "watch_poll", WorkspaceID: wsA, Payload: pb, Reason: "boom", WillRetry: true}); err != nil {
		t.Fatal(err)
	}
	var st, msg string
	_ = pool.QueryRow(ctx, `SELECT status, error FROM jobs WHERE id=$1`, js[0].ID).Scan(&st, &msg)
	if st != "cancelled" || msg != "Watch auto-disabled after consecutive failures" {
		t.Fatalf("job = %s %q", st, msg)
	}
}

func TestReconcileTenantScoped(t *testing.T) {
	pool, ctx := setup(t)
	// The same watch id lives in workspace B; a failure for workspace A's payload must not touch it.
	seed{ID: wID, WS: wsB, Interval: sp("daily"), Anchor: &vAnchor, Enabled: true}.insert(t, ctx, pool)
	d := poller.Deps{Pool: pool, Cfg: cfg, Now: func() time.Time { return vNow }, ExecutorFor: func(string) string { return "go" }}
	pb, _ := json.Marshal(map[string]string{"workspace_id": wsA, "watch_id": wID})
	if err := poller.Reconcile(ctx, d, queue.FailureNotice{JobID: 99999, JobType: "watch_poll", WorkspaceID: wsA, Payload: pb, Reason: "boom", WillRetry: false}); err != nil {
		t.Fatal(err)
	}
	var w poller.Watch
	inTx(t, ctx, pool, wsB, func(tx pgx.Tx) error {
		var ok bool
		var err error
		w, ok, err = poller.LoadWatch(ctx, tx, wID, wsB)
		if !ok {
			t.Fatal("missing")
		}
		return err
	})
	if w.ConsecutiveFailures != 0 || w.LastError != nil || !w.Enabled {
		t.Fatalf("foreign watch touched: %+v", w)
	}
}

// R-tz: the poll fire key and the naive-UTC jobs.next_run_at are the same instant in UTC whatever
// the Postgres session TimeZone (a UTC session is a documented canary precondition).
func TestFireKeyIsUTCWhateverSessionTimeZone(t *testing.T) {
	pool, ctx := setup(t)
	want := "watch:" + wID + ":2026-08-29T11:00:00+00:00"
	for _, zone := range []string{"Asia/Kolkata", "<+0330>-03:30", "UTC"} {
		cleanAll(t, ctx, pool)
		seed{ID: wID, WS: wsA, Interval: sp("hourly"), Anchor: &vAnchor, Enabled: true}.insert(t, ctx, pool)
		inTx(t, ctx, pool, wsA, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SET LOCAL TimeZone = '`+zone+`'`); err != nil {
				return err
			}
			_, err := poller.RescheduleWatch(ctx, tx, loadWatch(t, ctx, tx, wsA), false, vNow, cfg, "go")
			return err
		})
		js := jobs(t, ctx, pool, "watch:"+wID+":%")
		if len(js) != 1 || js[0].FireKey != want {
			t.Fatalf("%s: jobs %+v, want key %s", zone, js, want)
		}
		var p map[string]any
		_ = json.Unmarshal([]byte(js[0].Payload), &p)
		if p["fire_key"] != want {
			t.Errorf("%s: payload fire_key = %v", zone, p["fire_key"])
		}
		if !js[0].NextRunAt.Equal(time.Date(2026, 8, 29, 11, 0, 0, 0, time.UTC)) {
			t.Errorf("%s: next_run_at %v", zone, js[0].NextRunAt)
		}
	}
}
