//go:build integration

package outreachsend_test

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachsend"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachstore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

const (
	enqWS    = "ws-p09t17-a"
	enqOther = "ws-p09t17-b"
)

type enqEnv struct {
	t    *testing.T
	ctx  context.Context
	pool *pgxpool.Pool
}

func enqSetup(t *testing.T) *enqEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 24)
	e := &enqEnv{t: t, ctx: context.Background(), pool: pool}
	clean := func() {
		for _, w := range []string{enqWS, enqOther} {
			_ = tenancy.InWorkspace(e.ctx, pool, w, func(tx pgx.Tx) error {
				_, _ = tx.Exec(e.ctx, `DELETE FROM outreach_sends`)
				_, _ = tx.Exec(e.ctx, `DELETE FROM outreach_enrollments`)
				_, _ = tx.Exec(e.ctx, `DELETE FROM outreach_sequences`)
				_, err := tx.Exec(e.ctx, `DELETE FROM jobs WHERE type='send' AND workspace_id=$1`, w)
				return err
			})
		}
		_, _ = pool.Exec(e.ctx, `DELETE FROM outreach_schedules WHERE workspace_id IN ($1,$2)`, enqWS, enqOther)
	}
	clean()
	t.Cleanup(clean)
	return e
}

func (e *enqEnv) in(ws string, fn func(tx pgx.Tx)) {
	e.t.Helper()
	if err := tenancy.InWorkspace(e.ctx, e.pool, ws, func(tx pgx.Tx) error { fn(tx); return nil }); err != nil {
		e.t.Fatalf("InWorkspace: %v", err)
	}
}

func (e *enqEnv) seq(status string) string {
	e.t.Helper()
	var id string
	e.in(enqWS, func(tx pgx.Tx) {
		s, err := outreachstore.CreateSequence(e.ctx, tx, enqWS, outreachstore.NewSeqInput("S"))
		if err != nil {
			e.t.Fatal(err)
		}
		id = s.ID
		if _, err := outreachstore.SetSequenceStatus(e.ctx, tx, enqWS, id, status); err != nil {
			e.t.Fatal(err)
		}
	})
	return id
}

func (e *enqEnv) enroll(seq string, lead int, email string, due time.Time) int {
	e.t.Helper()
	var id int
	e.in(enqWS, func(tx pgx.Tx) {
		var err error
		id, _, err = outreachstore.Enroll(e.ctx, tx, enqWS, seq, lead, email, "web form", time.Time{})
		if err != nil {
			e.t.Fatal(err)
		}
		if err := outreachstore.RescheduleEnrollment(e.ctx, tx, id, due); err != nil {
			e.t.Fatal(err)
		}
	})
	return id
}

func (e *enqEnv) jobs() []map[string]any {
	e.t.Helper()
	var out []map[string]any
	e.in(enqWS, func(tx pgx.Tx) {
		rows, err := tx.Query(e.ctx, `SELECT payload::text, executor, status, fire_key FROM jobs WHERE type='send' AND workspace_id=$1 ORDER BY id`, enqWS)
		if err != nil {
			e.t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var p, ex, st string
			var fk *string
			if err := rows.Scan(&p, &ex, &st, &fk); err != nil {
				e.t.Fatal(err)
			}
			m := map[string]any{}
			if err := json.Unmarshal([]byte(p), &m); err != nil {
				e.t.Fatal(err)
			}
			m["_executor"], m["_status"] = ex, st
			out = append(out, m)
		}
	})
	return out
}

func (e *enqEnv) enqueue(seq string, limit int, executor string) int {
	e.t.Helper()
	var n int
	e.in(enqWS, func(tx pgx.Tx) {
		var err error
		n, err = outreachsend.EnqueueDue(e.ctx, tx, enqWS, seq, limit, executor)
		if err != nil {
			e.t.Fatal(err)
		}
	})
	return n
}

func TestEnqueueDueMatchesPythonPayloads(t *testing.T) {
	e := enqSetup(t)
	seq := e.seq("active")
	id := e.enroll(seq, 7, "Ann@Example.com", time.Now().Add(-time.Hour))
	if n := e.enqueue(seq, 10, "go"); n != 1 {
		t.Fatalf("enqueued %d", n)
	}
	js := e.jobs()
	if len(js) != 1 {
		t.Fatalf("jobs %d", len(js))
	}
	j := js[0]
	idem := "seq:" + enqWS + ":" + seq + ":" + itoa(id) + ":0"
	if j["idempotency_key"] != idem || j["workspace_id"] != enqWS || j["sequence_id"] != seq ||
		j["enrollment_id"] != float64(id) || j["lead_id"] != float64(7) || j["step_number"] != float64(0) ||
		j["to_email"] != "ann@example.com" && j["to_email"] != "Ann@Example.com" || j["_executor"] != "go" || j["_status"] != "pending" {
		t.Fatalf("payload %v", j)
	}
	// key order of the stored json text
	var raw string
	e.in(enqWS, func(tx pgx.Tx) {
		_ = tx.QueryRow(e.ctx, `SELECT payload::text FROM jobs WHERE type='send' AND workspace_id=$1`, enqWS).Scan(&raw)
	})
	order := []string{`"workspace_id"`, `"sequence_id"`, `"enrollment_id"`, `"lead_id"`, `"step_number"`, `"to_email"`, `"idempotency_key"`}
	last := -1
	for _, k := range order {
		i := indexOf(raw, k)
		if i <= last {
			t.Fatalf("key order wrong at %s: %s", k, raw)
		}
		last = i
	}
}

func TestEnqueueDedupsPendingJobAndExistingSend(t *testing.T) {
	e := enqSetup(t)
	seq := e.seq("active")
	e1 := e.enroll(seq, 1, "a@example.com", time.Now().Add(-time.Hour))
	e2 := e.enroll(seq, 2, "b@example.com", time.Now().Add(-time.Hour))
	if n := e.enqueue(seq, 10, "go"); n != 2 {
		t.Fatalf("first %d", n)
	}
	if n := e.enqueue(seq, 10, "go"); n != 0 {
		t.Fatalf("pending job must dedup, got %d", n)
	}
	// job finishes, send row exists for e1 only; e2 job completed without a send row
	e.in(enqWS, func(tx pgx.Tx) {
		_, err := tx.Exec(e.ctx, `UPDATE jobs SET status='completed' WHERE type='send' AND workspace_id=$1`, enqWS)
		if err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(e.ctx, `INSERT INTO outreach_sends (workspace_id, sequence_id, enrollment_id, step_number, to_email, status, idempotency_key)
			VALUES ($1,$2,$3,0,'a@example.com','sent',$4)`, enqWS, seq, e1, "seq:"+enqWS+":"+seq+":"+itoa(e1)+":0")
		if err != nil {
			t.Fatal(err)
		}
	})
	// DueEnrollments already hides e1 (send row for current step); e2 is re-enqueued.
	if n := e.enqueue(seq, 10, "go"); n != 1 {
		t.Fatalf("want only e2 re-enqueued, got %d (e2=%d)", n, e2)
	}
}

func TestEnqueueSendDedupsAgainstSendRowDirectly(t *testing.T) {
	e := enqSetup(t)
	seq := e.seq("active")
	idem := "seq:" + enqWS + ":" + seq + ":99:0"
	e.in(enqWS, func(tx pgx.Tx) {
		if _, err := tx.Exec(e.ctx, `INSERT INTO outreach_sends (workspace_id, sequence_id, enrollment_id, step_number, to_email, status, idempotency_key)
			VALUES ($1,$2,99,0,'a@example.com','in_flight',$3)`, enqWS, seq, idem); err != nil {
			t.Fatal(err)
		}
		ok, err := outreachsend.EnqueueSend(e.ctx, tx, enqWS, outreachsend.SendPayload{
			WorkspaceID: enqWS, SequenceID: seq, EnrollmentID: 99, LeadID: 1, StepNumber: 0, ToEmail: "a@example.com", IdempotencyKey: idem,
		}, "go")
		if err != nil || ok {
			t.Fatalf("ok=%v err=%v", ok, err)
		}
	})
}

func TestExecuteAndTickCollapse(t *testing.T) {
	e := enqSetup(t)
	seq := e.seq("active")
	e.enroll(seq, 1, "a@example.com", time.Now().Add(-time.Hour))
	// /execute path then the ticker path for the same step
	if n := e.enqueue(seq, 200, "go"); n != 1 {
		t.Fatalf("execute %d", n)
	}
	n, err := outreachsend.Tick(e.ctx, e.pool, enqWS, seq, time.Now(), outreachsend.TickConfig{Interval: 900 * time.Second, MaxEnqueue: 200, Executors: enqGoRouting, Outreach: config.OutreachSettings{GoExecutorSend: true}})
	if err != nil || n != 0 {
		t.Fatalf("tick n=%d err=%v", n, err)
	}
	if got := len(e.jobs()); got != 1 {
		t.Fatalf("jobs %d", got)
	}
}

func TestEnqueueLimit(t *testing.T) {
	e := enqSetup(t)
	seq := e.seq("active")
	for i := 0; i < 5; i++ {
		e.enroll(seq, i+1, "u"+itoa(i)+"@example.com", time.Now().Add(-time.Hour))
	}
	if n := e.enqueue(seq, 3, "go"); n != 3 {
		t.Fatalf("limit 3 got %d", n)
	}
	if n := e.enqueue(seq, 0, "go"); n != 0 {
		t.Fatalf("limit 0 got %d", n)
	}
	if got := len(e.jobs()); got != 3 {
		t.Fatalf("jobs %d", got)
	}
}

func TestTickDisablesInactiveSequence(t *testing.T) {
	e := enqSetup(t)
	seq := e.seq("paused")
	e.in(enqWS, func(tx pgx.Tx) {
		if err := outreachstore.UpsertSchedule(e.ctx, tx, enqWS, seq, time.Now(), true); err != nil {
			t.Fatal(err)
		}
	})
	cfg := outreachsend.TickConfig{Interval: time.Minute, MaxEnqueue: 10}
	for _, s := range []string{seq, "missing-seq"} {
		n, err := outreachsend.Tick(e.ctx, e.pool, enqWS, s, time.Now(), cfg)
		if err != nil || n != 0 {
			t.Fatalf("%s n=%d err=%v", s, n, err)
		}
	}
	var enabled bool
	if err := e.pool.QueryRow(e.ctx, `SELECT enabled FROM outreach_schedules WHERE sequence_id=$1`, seq).Scan(&enabled); err == nil && enabled {
		t.Fatal("schedule still enabled")
	}
}

func TestTickReschedules(t *testing.T) {
	e := enqSetup(t)
	seq := e.seq("active")
	e.enroll(seq, 1, "a@example.com", time.Now().Add(-time.Hour))
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	n, err := outreachsend.Tick(e.ctx, e.pool, enqWS, seq, now, outreachsend.TickConfig{Interval: 900 * time.Second, MaxEnqueue: 200})
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	var enabled bool
	var next time.Time
	if err := e.pool.QueryRow(e.ctx, `SELECT enabled, next_tick_at AT TIME ZONE 'UTC' FROM outreach_schedules WHERE sequence_id=$1`, seq).Scan(&enabled, &next); err != nil {
		t.Fatal(err)
	}
	if !enabled || !next.Equal(now.Add(900*time.Second)) {
		t.Fatalf("enabled=%v next=%v", enabled, next)
	}
}

func TestEnqueueExecutorFollowsFlag(t *testing.T) {
	e := enqSetup(t)
	seq := e.seq("active")
	e.enroll(seq, 1, "a@example.com", time.Now().Add(-time.Hour))
	e.enqueue(seq, 10, "go")
	if js := e.jobs(); js[0]["_executor"] != "go" {
		t.Fatalf("%v", js[0])
	}
	e.in(enqWS, func(tx pgx.Tx) {
		if _, err := outreachsend.EnqueueDue(e.ctx, tx, enqWS, seq, 10, "bogus"); err == nil {
			t.Fatal("bogus executor must error")
		}
	})
	on := config.ExecutorSettings{Enabled: true, Workspaces: map[string]bool{enqWS: true}}
	flag := config.OutreachSettings{GoExecutorSend: true}
	for _, c := range []struct {
		name string
		ex   config.ExecutorSettings
		o    config.OutreachSettings
		ws   string
		want string
	}{
		{"all gates", on, flag, enqWS, "go"},
		// The gates are retired (plan 15 cutover): every send job is "go".
		{"master off", config.ExecutorSettings{Workspaces: on.Workspaces}, flag, enqWS, "go"},
		{"outside cohort", on, flag, enqOther, "go"},
		{"flag off", on, config.OutreachSettings{}, enqWS, "go"},
	} {
		if got := outreachsend.ExecutorFor(c.ex, c.o, c.ws); got != c.want {
			t.Errorf("%s: %s want %s", c.name, got, c.want)
		}
	}
}

func TestEnqueueRollsBackWithTx(t *testing.T) {
	e := enqSetup(t)
	seq := e.seq("active")
	e.enroll(seq, 1, "a@example.com", time.Now().Add(-time.Hour))
	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(e.ctx, `SELECT set_config('app.workspace_id', $1, true)`, enqWS); err != nil {
		t.Fatal(err)
	}
	if n, err := outreachsend.EnqueueDue(e.ctx, tx, enqWS, seq, 10, "go"); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	_ = tx.Rollback(e.ctx)
	if got := len(e.jobs()); got != 0 {
		t.Fatalf("jobs after rollback %d", got)
	}
}

func TestConcurrentEnqueueSingleFlight(t *testing.T) {
	e := enqSetup(t)
	seq := e.seq("active")
	e.enroll(seq, 1, "a@example.com", time.Now().Add(-time.Hour))
	var total atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := tenancy.InWorkspace(e.ctx, e.pool, enqWS, func(tx pgx.Tx) error {
				n, err := outreachsend.EnqueueDue(e.ctx, tx, enqWS, seq, 10, "go")
				total.Add(int32(n))
				return err
			})
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if total.Load() != 1 || len(e.jobs()) != 1 {
		t.Fatalf("total=%d jobs=%d", total.Load(), len(e.jobs()))
	}
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestEnqueueSendBlockedByPythonCreatedJob(t *testing.T) {
	e := enqSetup(t)
	seq := e.seq("active")
	idem := "seq:" + enqWS + ":" + seq + ":5:0"
	for _, st := range []string{"pending", "processing"} {
		e.in(enqWS, func(tx pgx.Tx) {
			// Python add_job shape: NULL fire_key, no fire_key in the payload.
			if _, err := tx.Exec(e.ctx, `INSERT INTO jobs (type, payload, status, workspace_id, executor, fire_key)
				VALUES ('send', json_build_object('idempotency_key', $1::text, 'workspace_id', $2::text), $3, $2, 'python', NULL)`, idem, enqWS, st); err != nil {
				t.Fatal(err)
			}
			ok, err := outreachsend.EnqueueSend(e.ctx, tx, enqWS, outreachsend.SendPayload{WorkspaceID: enqWS, SequenceID: seq, EnrollmentID: 5, IdempotencyKey: idem}, "go")
			if err != nil || ok {
				t.Fatalf("%s: ok=%v err=%v", st, ok, err)
			}
			if _, err := tx.Exec(e.ctx, `DELETE FROM jobs WHERE type='send' AND workspace_id=$1`, enqWS); err != nil {
				t.Fatal(err)
			}
		})
	}
}

var enqGoRouting = config.ExecutorSettings{Enabled: true, Workspaces: map[string]bool{enqWS: true}}
