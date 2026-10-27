//go:build integration

package autoschedule_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autoschedule"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

const (
	wsA = "ws-p08t7-a"
	wsB = "ws-p08t7-b"
)

var (
	anchor = time.Date(2026, 8, 29, 8, 0, 0, 0, time.UTC)
	now    = time.Date(2026, 8, 29, 9, 30, 0, 0, time.UTC)
)

func setup(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	pool := testpg.NewAppPool(t, 24)
	ctx := context.Background()
	clean := func() {
		for _, w := range []string{wsA, wsB} {
			_ = tenancy.InWorkspace(ctx, pool, w, func(tx pgx.Tx) error {
				_, _ = tx.Exec(ctx, `DELETE FROM triggers`)
				_, _ = tx.Exec(ctx, `DELETE FROM jobs WHERE workspace_id = $1`, w)
				_, err := tx.Exec(ctx, `DELETE FROM scheduled_triggers WHERE workspace_id = $1`, w)
				return err
			})
		}
	}
	clean()
	t.Cleanup(clean)
	return pool, ctx
}

func mkTrigger(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ws, id, typ string, enabled bool, interval string, anc *time.Time) {
	t.Helper()
	cfg, _ := json.Marshal(map[string]any{"interval": interval})
	err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO triggers (id, workspace_id, name, enabled, trigger_type, trigger_config, actions, schedule_anchor)
			VALUES ($1,$2,'t',$3,$4,$5::json,'[]'::json,$6)`, id, ws, enabled, typ, string(cfg), anc)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func trig(ws, id string) autoschedule.Trigger {
	a := anchor
	return autoschedule.Trigger{ID: id, WorkspaceID: ws, Enabled: true, Type: "on_schedule", Interval: "hourly", Anchor: &a}
}

type jobRow struct {
	Executor, Type, FireKey, Status string
	Payload                         string
	NextRunAt                       time.Time
	Priority, MaxRetries            int
}

func jobsFor(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ws string) []jobRow {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT executor, type, COALESCE(fire_key,''), status, payload::text, next_run_at, priority, max_retries
		FROM jobs WHERE workspace_id = $1 ORDER BY id`, ws)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []jobRow
	for rows.Next() {
		var j jobRow
		if err := rows.Scan(&j.Executor, &j.Type, &j.FireKey, &j.Status, &j.Payload, &j.NextRunAt, &j.Priority, &j.MaxRetries); err != nil {
			t.Fatal(err)
		}
		out = append(out, j)
	}
	return out
}

func enqueue(ctx context.Context, pool *pgxpool.Pool, tr autoschedule.Trigger, next time.Time, exec string) (bool, error) {
	var created bool
	err := tenancy.InWorkspace(ctx, pool, tr.WorkspaceID, func(tx pgx.Tx) error {
		var err error
		created, err = autoschedule.EnqueueScheduleIfAbsent(ctx, tx, tr, next, exec)
		return err
	})
	return created, err
}

type mirrorRow struct {
	WS      string
	Next    *time.Time
	Enabled bool
}

func mirror(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string) (mirrorRow, bool) {
	t.Helper()
	var m mirrorRow
	err := pool.QueryRow(ctx, `SELECT workspace_id, next_run_at, enabled FROM scheduled_triggers WHERE trigger_id=$1`, id).Scan(&m.WS, &m.Next, &m.Enabled)
	if err == pgx.ErrNoRows {
		return m, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return m, true
}

func TestEnqueueScheduleSingleFlightAcrossGoAndPython(t *testing.T) {
	pool, ctx := setup(t)
	mkTrigger(t, ctx, pool, wsA, "trg-sf", "on_schedule", true, "hourly", &anchor)
	next := anchor.Add(2 * time.Hour)
	key := autoschedule.FireKey("trg-sf", next)
	// Python-shaped active row for the same fire key.
	if _, err := pool.Exec(ctx, `INSERT INTO jobs (executor, type, payload, workspace_id, priority, fire_key, status, created_at, next_run_at, max_retries, retry_count)
		VALUES ('python','trigger_eval', $1::json, $2, 1, $3, 'pending', now() AT TIME ZONE 'UTC', $4, 3, 0)`,
		`{"trigger_id": "trg-sf", "workspace_id": "`+wsA+`", "fire_source": "schedule", "fire_key": "`+key+`"}`, wsA, key, next); err != nil {
		t.Fatal(err)
	}
	var created atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := enqueue(ctx, pool, trig(wsA, "trg-sf"), next, "go")
			if err != nil {
				t.Error(err)
			}
			if ok {
				created.Add(1)
			}
		}()
	}
	wg.Wait()
	if created.Load() != 0 {
		t.Fatalf("created %d over the python row", created.Load())
	}
	if js := jobsFor(t, ctx, pool, wsA); len(js) != 1 || js[0].Executor != "python" {
		t.Fatalf("jobs: %+v", js)
	}
	// Without the Python row exactly one goroutine wins.
	next2 := anchor.Add(3 * time.Hour)
	created.Store(0)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := enqueue(ctx, pool, trig(wsA, "trg-sf"), next2, "go")
			if err != nil {
				t.Error(err)
			}
			if ok {
				created.Add(1)
			}
		}()
	}
	wg.Wait()
	if created.Load() != 1 {
		t.Fatalf("created %d, want 1", created.Load())
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE fire_key=$1 AND status IN ('pending','processing')`, autoschedule.FireKey("trg-sf", next2)).Scan(&n)
	if n != 1 {
		t.Fatalf("active rows %d", n)
	}
}

func TestPayloadShapeEqualsPython(t *testing.T) {
	pool, ctx := setup(t)
	mkTrigger(t, ctx, pool, wsA, "trg-p", "on_schedule", true, "hourly", &anchor)
	next := anchor.Add(time.Hour)
	if ok, err := enqueue(ctx, pool, trig(wsA, "trg-p"), next, "go"); err != nil || !ok {
		t.Fatalf("enqueue: %v %v", ok, err)
	}
	js := jobsFor(t, ctx, pool, wsA)
	if len(js) != 1 {
		t.Fatalf("jobs %+v", js)
	}
	j := js[0]
	key := "sched:trg-p:2026-08-29T09:00:00+00:00"
	wantPayload := `{"trigger_id":"trg-p","workspace_id":"` + wsA + `","fire_source":"schedule","fire_key":"` + key + `"}`
	if j.Payload != wantPayload {
		t.Errorf("payload %s", j.Payload)
	}
	if j.Type != "trigger_eval" || j.FireKey != key || j.Status != "pending" || j.Priority != 1 || j.MaxRetries != 3 || j.Executor != "go" {
		t.Errorf("row %+v", j)
	}
	if !j.NextRunAt.Equal(time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("next_run_at %v", j.NextRunAt)
	}
}

func TestFireKeyTextEqualsPythonInJobs(t *testing.T) {
	pool, ctx := setup(t)
	mkTrigger(t, ctx, pool, wsA, "trg-k", "on_schedule", true, "hourly", &anchor)
	next := time.Date(2026, 8, 29, 9, 0, 0, 250000000, time.UTC)
	if ok, err := enqueue(ctx, pool, trig(wsA, "trg-k"), next, "go"); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	if k := jobsFor(t, ctx, pool, wsA)[0].FireKey; k != "sched:trg-k:2026-08-29T09:00:00.250000+00:00" {
		t.Fatalf("key %q", k)
	}
}

func TestMirrorWrittenInSameTx(t *testing.T) {
	pool, ctx := setup(t)
	mkTrigger(t, ctx, pool, wsA, "trg-m", "on_schedule", true, "hourly", &anchor)
	next := anchor.Add(time.Hour)
	// rolled back: neither job, trigger.next_run_at nor mirror survive
	err := tenancy.InWorkspace(ctx, pool, wsA, func(tx pgx.Tx) error {
		if ok, err := autoschedule.EnqueueScheduleIfAbsent(ctx, tx, trig(wsA, "trg-m"), next, "go"); err != nil || !ok {
			t.Fatalf("%v %v", ok, err)
		}
		return context.Canceled
	})
	if err == nil {
		t.Fatal("expected rollback error")
	}
	if _, ok := mirror(t, ctx, pool, "trg-m"); ok || len(jobsFor(t, ctx, pool, wsA)) != 0 {
		t.Fatal("rollback leaked mirror or job")
	}
	if ok, err := enqueue(ctx, pool, trig(wsA, "trg-m"), next, "go"); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	m, ok := mirror(t, ctx, pool, "trg-m")
	if !ok || m.WS != wsA || !m.Enabled || m.Next == nil || !m.Next.Equal(next) {
		t.Fatalf("mirror %+v %v", m, ok)
	}
	var tn *time.Time
	_ = tenancy.InWorkspace(ctx, pool, wsA, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT next_run_at FROM triggers WHERE id='trg-m'`).Scan(&tn)
	})
	if tn == nil || !tn.Equal(next) {
		t.Fatalf("triggers.next_run_at %v", tn)
	}
	// MirrorSet / MirrorDelete directly.
	later := next.Add(time.Hour)
	_ = tenancy.InWorkspace(ctx, pool, wsA, func(tx pgx.Tx) error {
		return autoschedule.MirrorSet(ctx, tx, "trg-m", wsA, &later, false)
	})
	if m, _ := mirror(t, ctx, pool, "trg-m"); m.Enabled || !m.Next.Equal(later) {
		t.Fatalf("MirrorSet %+v", m)
	}
	_ = tenancy.InWorkspace(ctx, pool, wsA, func(tx pgx.Tx) error {
		return autoschedule.MirrorDelete(ctx, tx, "trg-m")
	})
	if _, ok := mirror(t, ctx, pool, "trg-m"); ok {
		t.Fatal("MirrorDelete left the row")
	}
}

func seedMirror(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ws, id string, next *time.Time, enabled bool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO scheduled_triggers (trigger_id, workspace_id, next_run_at, enabled) VALUES ($1,$2,$3,$4)`, id, ws, next, enabled); err != nil {
		t.Fatal(err)
	}
}

func TestBootstrapSkipsDisabledAndDeleted(t *testing.T) {
	pool, ctx := setup(t)
	past := anchor
	mkTrigger(t, ctx, pool, wsA, "trg-ok", "on_schedule", true, "hourly", &anchor)
	mkTrigger(t, ctx, pool, wsA, "trg-off", "on_schedule", false, "hourly", &anchor)
	mkTrigger(t, ctx, pool, wsA, "trg-wrong", "on_new_row", true, "hourly", &anchor)
	for _, id := range []string{"trg-ok", "trg-off", "trg-wrong", "trg-gone"} {
		seedMirror(t, ctx, pool, wsA, id, &past, true)
	}
	seedMirror(t, ctx, pool, wsA, "trg-mirror-off", &past, false)
	mkTrigger(t, ctx, pool, wsA, "trg-mirror-off", "on_schedule", true, "hourly", &anchor)
	n, err := autoschedule.BootstrapSchedules(ctx, pool, now, func(string) string { return "go" })
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("enqueued %d, want 1", n)
	}
	js := jobsFor(t, ctx, pool, wsA)
	if len(js) != 1 || js[0].FireKey != "sched:trg-ok:2026-08-29T10:00:00+00:00" {
		t.Fatalf("jobs %+v", js)
	}
}

func TestBootstrapEntersWorkspaceScope(t *testing.T) {
	pool, ctx := setup(t)
	// A mirror row naming workspace B for a trigger that lives in workspace A
	// must not be scheduled (the follow-up read is RLS scoped to the mirror's
	// workspace); a correct pair in each workspace is.
	mkTrigger(t, ctx, pool, wsA, "trg-a", "on_schedule", true, "hourly", &anchor)
	mkTrigger(t, ctx, pool, wsB, "trg-b", "on_schedule", true, "daily", &anchor)
	mkTrigger(t, ctx, pool, wsA, "trg-x", "on_schedule", true, "hourly", &anchor)
	seedMirror(t, ctx, pool, wsA, "trg-a", nil, true)
	seedMirror(t, ctx, pool, wsB, "trg-b", &anchor, true)
	seedMirror(t, ctx, pool, wsB, "trg-x", &anchor, true) // wrong workspace
	// ...and the app role cannot read triggers without a scope at all.
	var c int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM triggers`).Scan(&c); err != nil || c != 0 {
		t.Fatalf("unscoped triggers visible: %d %v", c, err)
	}
	execs := map[string]string{wsA: "go", wsB: "go"}
	n, err := autoschedule.BootstrapSchedules(ctx, pool, now, func(ws string) string { return execs[ws] })
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("enqueued %d, want 2", n)
	}
	if js := jobsFor(t, ctx, pool, wsA); len(js) != 1 || js[0].Executor != "go" {
		t.Fatalf("A %+v", js)
	}
	if js := jobsFor(t, ctx, pool, wsB); len(js) != 1 || js[0].Executor != "go" || js[0].FireKey != "sched:trg-b:2026-08-30T08:00:00+00:00" {
		t.Fatalf("B %+v", js)
	}
}

func TestBootstrapIsIdempotent(t *testing.T) {
	pool, ctx := setup(t)
	mkTrigger(t, ctx, pool, wsA, "trg-i", "on_schedule", true, "hourly", &anchor)
	seedMirror(t, ctx, pool, wsA, "trg-i", nil, true)
	ex := func(string) string { return "go" }
	n, err := autoschedule.BootstrapSchedules(ctx, pool, now, ex)
	if err != nil || n != 1 {
		t.Fatalf("first %d %v", n, err)
	}
	// The mirror now points at the future: not due, and even when forced due the
	// active job blocks a second row.
	if n, err = autoschedule.BootstrapSchedules(ctx, pool, now, ex); err != nil || n != 0 {
		t.Fatalf("second %d %v", n, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE scheduled_triggers SET next_run_at = NULL WHERE trigger_id='trg-i'`); err != nil {
		t.Fatal(err)
	}
	if n, err = autoschedule.BootstrapSchedules(ctx, pool, now, ex); err != nil || n != 0 {
		t.Fatalf("third %d %v", n, err)
	}
	if js := jobsFor(t, ctx, pool, wsA); len(js) != 1 {
		t.Fatalf("jobs %+v", js)
	}
}

func TestBootstrapFlagOff(t *testing.T) {
	pool, ctx := setup(t)
	mkTrigger(t, ctx, pool, wsA, "trg-f", "on_schedule", true, "hourly", &anchor)
	seedMirror(t, ctx, pool, wsA, "trg-f", nil, true)
	n, err := autoschedule.BootstrapSchedulesIf(ctx, pool, false, now, func(string) string { return "go" })
	if err != nil || n != 0 || len(jobsFor(t, ctx, pool, wsA)) != 0 {
		t.Fatalf("flag off: %d %v", n, err)
	}
}

func TestExecutorColumnFollowsPolicy(t *testing.T) {
	pool, ctx := setup(t)
	mkTrigger(t, ctx, pool, wsA, "trg-e1", "on_schedule", true, "hourly", &anchor)
	mkTrigger(t, ctx, pool, wsA, "trg-e2", "on_schedule", true, "hourly", &anchor)
	for id, ex := range map[string]string{"trg-e1": "go", "trg-e2": "go"} {
		if ok, err := enqueue(ctx, pool, trig(wsA, id), anchor.Add(time.Hour), ex); err != nil || !ok {
			t.Fatalf("%v %v", ok, err)
		}
	}
	got := map[string]string{}
	for _, j := range jobsFor(t, ctx, pool, wsA) {
		got[j.FireKey] = j.Executor
	}
	if got["sched:trg-e1:2026-08-29T09:00:00+00:00"] != "go" || got["sched:trg-e2:2026-08-29T09:00:00+00:00"] != "go" {
		t.Fatalf("executors %v", got)
	}
	if _, err := enqueue(ctx, pool, trig(wsA, "trg-e1"), anchor.Add(5*time.Hour), "rust"); err == nil {
		t.Fatal("invalid executor accepted")
	}
}

func TestBootstrapForTriggerAnchorDefaultsToNow(t *testing.T) {
	pool, ctx := setup(t)
	mkTrigger(t, ctx, pool, wsA, "trg-n", "on_schedule", true, "hourly", nil)
	tr := autoschedule.Trigger{ID: "trg-n", WorkspaceID: wsA, Enabled: true, Type: "on_schedule", Interval: "hourly"}
	var next time.Time
	err := tenancy.InWorkspace(ctx, pool, wsA, func(tx pgx.Tx) error {
		var err error
		next, err = autoschedule.BootstrapForTrigger(ctx, tx, tr, now, "go")
		return err
	})
	if err != nil || !next.Equal(now.Add(time.Hour)) {
		t.Fatalf("next %v err %v", next, err)
	}
	if js := jobsFor(t, ctx, pool, wsA); len(js) != 1 || js[0].FireKey != "sched:trg-n:2026-08-29T10:30:00+00:00" {
		t.Fatalf("jobs %+v", js)
	}
}

// R-tz: fire keys render in UTC whatever the Postgres session TimeZone (a UTC session is a
// documented canary precondition), and a zone name Go cannot load no longer refuses to
// schedule: nothing reads the session zone any more.
func TestFireKeyIsUTCWhateverSessionTimeZone(t *testing.T) {
	pool, ctx := setup(t)
	for i, zone := range []string{"Asia/Kolkata", "<+0330>-03:30"} {
		id := fmt.Sprintf("trg-tz%d", i)
		mkTrigger(t, ctx, pool, wsA, id, "on_schedule", true, "hourly", &anchor)
		next := anchor.Add(time.Hour) // 09:00Z
		err := tenancy.InWorkspace(ctx, pool, wsA, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SET LOCAL TimeZone = '`+zone+`'`); err != nil {
				return err
			}
			ok, err := autoschedule.EnqueueScheduleIfAbsent(ctx, tx, trig(wsA, id), next, "go")
			if err == nil && !ok {
				t.Errorf("%s: not created", zone)
			}
			if err != nil {
				return err
			}
			_, err = autoschedule.BootstrapForTrigger(ctx, tx, trig(wsA, id), anchor, "go")
			return err
		})
		if err != nil {
			t.Fatalf("%s: %v", zone, err)
		}
	}
	keys := map[string]bool{}
	for _, j := range jobsFor(t, ctx, pool, wsA) {
		keys[j.FireKey] = true
		// jobs.next_run_at is naive UTC (the claim compares it with now() AT TIME ZONE 'UTC'):
		// it must be the key's instant, not the session-local wall time.
		at, err := time.Parse(time.RFC3339, j.FireKey[strings.LastIndex(j.FireKey, ":2026")+1:])
		if err != nil || !j.NextRunAt.Equal(at) {
			t.Errorf("%s: next_run_at %v", j.FireKey, j.NextRunAt)
		}
	}
	for i := range 2 {
		if k := fmt.Sprintf("sched:trg-tz%d:2026-08-29T09:00:00+00:00", i); !keys[k] {
			t.Fatalf("missing %s in %v", k, keys)
		}
	}
}
