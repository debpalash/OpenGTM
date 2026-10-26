//go:build integration

package leadjobs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type env struct {
	pool, owner *pgxpool.Pool
	prefix      string
}

func setup(t *testing.T) *env {
	t.Helper()
	e := &env{pool: testpg.NewAppPool(t, 8), prefix: fmt.Sprintf("t20_%d_", os.Getpid())}
	var err error
	if e.owner, err = pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.owner.Close)
	e.clean()
	t.Cleanup(e.clean)
	return e
}

func (e *env) clean() {
	c := context.Background()
	_, _ = e.owner.Exec(c, `DELETE FROM jobs WHERE type IN ('source_health_check','source_health_check_x')`)
	_, _ = e.owner.Exec(c, `DELETE FROM source_health WHERE name LIKE $1 OR name LIKE 'zzz%'`, e.prefix+"%")
}

func (e *env) names(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%ssrc%02d", e.prefix, i)
	}
	return out
}

// fake is a Prober: yield per name (nil entry = null), optional failing batch index, call log.
type fake struct {
	mu       sync.Mutex
	names    []string
	yields   map[string]*int
	failCall int // 1-based call number that errors; 0 = none
	calls    [][]string
	inflight int
	maxIn    int
	block    chan struct{} // when set, SourceProbe waits on it or ctx
}

func ip(n int) *int { return &n }

func (f *fake) SourceList(context.Context) ([]string, error) { return f.names, nil }

func (f *fake) SourceProbe(ctx context.Context, names []string, _ time.Duration) (map[string]*int, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string(nil), names...))
	n := len(f.calls)
	f.inflight++
	if f.inflight > f.maxIn {
		f.maxIn = f.inflight
	}
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.inflight--; f.mu.Unlock() }()
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.failCall == n {
		return nil, errors.New("rpc down")
	}
	out := map[string]*int{}
	for _, nm := range names {
		out[nm] = f.yields[nm]
	}
	return out, nil
}

func (e *env) deps(f *fake, kv map[string]string) Deps {
	m := map[string]string{"SOURCE_HEALTH_ENABLED": "1", "SOURCE_HEALTH_PROBE_CONCURRENCY": "100"}
	for k, v := range kv {
		m[k] = v
	}
	return Deps{Pool: e.pool, Py: f, Getenv: envOf(m), Executor: func() string { return "go" },
		Now: func() time.Time { return time.Date(2026, 3, 4, 10, 30, 0, 0, time.UTC) }}
}

func (e *env) claim(t *testing.T, status string) *queue.Claim {
	t.Helper()
	var id int64
	if err := e.owner.QueryRow(context.Background(), `INSERT INTO jobs (executor,type,payload,priority,status,created_at,max_retries,retry_count)
		VALUES ('go','source_health_check','{}',1,$1,now() AT TIME ZONE 'UTC',3,0) RETURNING id`, status).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return &queue.Claim{ID: id, Type: JobType, Payload: []byte("{}")}
}

type health struct {
	state            string
	zero, nonzero    int
	probes           int
	hasOutage        bool
	found            bool
}

func (e *env) health(t *testing.T, name string) health {
	t.Helper()
	var h health
	rows, err := e.owner.Query(context.Background(), `SELECT state, consecutive_zero, consecutive_nonzero, probes, last_outage_at IS NOT NULL FROM source_health WHERE name=$1`, name)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		h.found = true
		if err := rows.Scan(&h.state, &h.zero, &h.nonzero, &h.probes, &h.hasOutage); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func (e *env) seed(t *testing.T, name string, zero int) {
	t.Helper()
	if _, err := e.owner.Exec(context.Background(), `INSERT INTO source_health (name,state,consecutive_zero,consecutive_nonzero,probes,manual_override) VALUES ($1,'healthy',$2,0,0,false)`, name, zero); err != nil {
		t.Fatal(err)
	}
}

type pendingJob struct {
	id       int64
	executor string
	fireKey  string
	nextRun  time.Time
}

func (e *env) pending(t *testing.T) []pendingJob {
	t.Helper()
	rows, err := e.owner.Query(context.Background(), `SELECT id, executor, coalesce(fire_key,''), next_run_at FROM jobs WHERE type='source_health_check' AND status='pending' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []pendingJob
	for rows.Next() {
		var p pendingJob
		if err := rows.Scan(&p.id, &p.executor, &p.fireKey, &p.nextRun); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func TestOutageGuardSuppressesZeroTransitions(t *testing.T) {
	e := setup(t)
	names := e.names(5)
	f := &fake{names: names, yields: map[string]*int{names[4]: ip(7)}} // 4 of 5 zero: 0.8 >= 0.6
	for _, n := range names[:2] {
		e.seed(t, n, 2)
	}
	if err := HandleSourceHealthCheck(context.Background(), e.deps(f, nil), e.claim(t, "processing")); err != nil {
		t.Fatal(err)
	}
	h := e.health(t, names[0])
	if h.zero != 2 || h.state != "healthy" || !h.hasOutage || h.probes != 1 {
		t.Fatalf("zero source during an outage must not advance: %+v", h)
	}
	if h := e.health(t, names[4]); h.nonzero != 1 || h.hasOutage {
		t.Fatalf("nonzero source: %+v", h)
	}
	// below the ratio the same zero advances the counter
	e.clean()
	e.seed(t, names[0], 2)
	f2 := &fake{names: names, yields: map[string]*int{names[1]: ip(1), names[2]: ip(1), names[3]: ip(1), names[4]: ip(1)}}
	if err := HandleSourceHealthCheck(context.Background(), e.deps(f2, nil), e.claim(t, "processing")); err != nil {
		t.Fatal(err)
	}
	if h := e.health(t, names[0]); h.zero != 3 || h.state != "degraded" {
		t.Fatalf("no outage: %+v", h)
	}
}

func TestProbeRPCFailureIsOutageNotZero(t *testing.T) {
	cases := map[string]func(names []string) *fake{
		"rpc error": func(n []string) *fake {
			y := map[string]*int{}
			for _, s := range n {
				y[s] = ip(3)
			}
			return &fake{names: n, yields: y, failCall: 2}
		},
		"null yield": func(n []string) *fake {
			y := map[string]*int{}
			for _, s := range n {
				y[s] = ip(3)
			}
			y[n[0]] = nil
			return &fake{names: n, yields: y}
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			names := e.names(10) // one zero of ten (0.1) stays under the ratio; the rpc failure alone makes it an outage
			f := mk(names)
			victim := names[0]
			if name == "rpc error" {
				victim = names[5] // second batch of 5
			}
			e.seed(t, victim, 5)
			d := e.deps(f, map[string]string{"SOURCE_HEALTH_PROBE_CONCURRENCY": "5"})
			if err := HandleSourceHealthCheck(context.Background(), d, e.claim(t, "processing")); err != nil {
				t.Fatal(err)
			}
			h := e.health(t, victim)
			if h.zero != 5 || h.state != "healthy" || !h.hasOutage {
				t.Fatalf("failed probe must be outage, never a counted zero: %+v", h)
			}
			if name == "rpc error" {
				if h := e.health(t, names[0]); h.nonzero != 1 {
					t.Fatalf("healthy batch still recorded: %+v", h)
				}
			}
		})
	}
}

func TestBatchesRespectConcurrency(t *testing.T) {
	e := setup(t)
	names := e.names(7)
	y := map[string]*int{}
	for _, n := range names {
		y[n] = ip(1)
	}
	f := &fake{names: names, yields: y}
	d := e.deps(f, map[string]string{"SOURCE_HEALTH_PROBE_CONCURRENCY": "3"})
	if err := HandleSourceHealthCheck(context.Background(), d, e.claim(t, "processing")); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 3 || len(f.calls[0]) != 3 || len(f.calls[1]) != 3 || len(f.calls[2]) != 1 || f.maxIn != 1 {
		t.Fatalf("calls %v maxInflight %d", f.calls, f.maxIn)
	}
	for _, n := range names {
		if e.health(t, n).probes != 1 {
			t.Fatalf("%s not applied", n)
		}
	}
}

func TestTransitionsPersistedAtomically(t *testing.T) {
	e := setup(t)
	bad := "zzz" + strings.Repeat("x", 120) // varchar(100): the second row's upsert fails
	names := append(e.names(1), bad)
	f := &fake{names: names, yields: map[string]*int{names[0]: ip(1), bad: ip(1)}}
	if err := HandleSourceHealthCheck(context.Background(), e.deps(f, nil), e.claim(t, "processing")); err == nil {
		t.Fatal("want the apply error")
	}
	if e.health(t, names[0]).found {
		t.Fatal("first row committed although the run failed")
	}
	if p := e.pending(t); len(p) != 0 {
		t.Fatalf("a failed run must not reschedule: %v", p)
	}
}

func TestRescheduleSingleFlight(t *testing.T) {
	e := setup(t)
	// near-miss rows that a LIKE/prefix match would wrongly treat as the same chain
	if _, err := e.owner.Exec(context.Background(), `INSERT INTO jobs (executor,type,payload,priority,fire_key,status,created_at,next_run_at,max_retries,retry_count)
		VALUES ('go','source_health_check_x','{}',1,'source_health:2026-03-05T00:00:00+00:0_','pending',now() AT TIME ZONE 'UTC',now() AT TIME ZONE 'UTC',3,0)`); err != nil {
		t.Fatal(err)
	}
	names := e.names(2)
	f := &fake{names: names, yields: map[string]*int{names[0]: ip(1), names[1]: ip(1)}}
	d := e.deps(f, nil)
	c := e.claim(t, "processing")
	var wg sync.WaitGroup
	errs := make([]error, 6)
	for i := range errs {
		wg.Add(1)
		go func() { defer wg.Done(); errs[i] = HandleSourceHealthCheck(context.Background(), d, c) }()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	p := e.pending(t)
	if len(p) != 1 {
		t.Fatalf("want exactly one next job, got %v", p)
	}
	if p[0].fireKey != "source_health:2026-03-05T00:00:00+00:00" || p[0].executor != "go" || !p[0].nextRun.Equal(time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("%+v", p[0])
	}
	var payload string
	_ = e.owner.QueryRow(context.Background(), `SELECT payload::text FROM jobs WHERE id=$1`, p[0].id).Scan(&payload)
	if payload != `{"fire_key":"source_health:2026-03-05T00:00:00+00:00"}` {
		t.Fatal(payload)
	}
	// the executor of the next job follows the current flags
	d.Executor = func() string { return "python" }
	e.clean()
	if err := HandleSourceHealthCheck(context.Background(), d, e.claim(t, "processing")); err != nil {
		t.Fatal(err)
	}
	if p := e.pending(t); len(p) != 1 || p[0].executor != "python" {
		t.Fatalf("%v", p)
	}
}

func TestCancelAppliesNothing(t *testing.T) {
	e := setup(t)
	names := e.names(3)
	f := &fake{names: names, yields: map[string]*int{}, block: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- HandleSourceHealthCheck(ctx, e.deps(f, nil), e.claim(t, "processing")) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not abort on cancel")
	}
	for _, n := range names {
		if e.health(t, n).found {
			t.Fatal("cancelled run applied transitions")
		}
	}
	if p := e.pending(t); len(p) != 0 {
		t.Fatalf("cancelled run rescheduled: %v", p)
	}
}

func TestBootstrapIdempotent(t *testing.T) {
	e := setup(t)
	d := e.deps(&fake{}, map[string]string{"SOURCE_HEALTH_ENABLED": "0"})
	tx, err := e.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ok, err := BootstrapSourceHealth(context.Background(), tx, d)
	_ = tx.Commit(context.Background())
	if err != nil || ok || len(e.pending(t)) != 0 {
		t.Fatalf("flag off: %v %v", ok, err)
	}
	d = e.deps(&fake{}, nil)
	for i, want := range []bool{true, false, false} {
		tx, err := e.pool.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		ok, err := BootstrapSourceHealth(context.Background(), tx, d)
		if err != nil {
			t.Fatal(err)
		}
		_ = tx.Commit(context.Background())
		if ok != want {
			t.Fatalf("call %d: %v, want %v", i, ok, want)
		}
	}
	if p := e.pending(t); len(p) != 1 || p[0].executor != "go" {
		t.Fatalf("%v", p)
	}
}

// A pending occurrence blocks a second one even under a different fire key (the Python
// single-flight is by job type), but this job's own processing row does not.
func TestPendingOccurrenceBlocksReschedule(t *testing.T) {
	e := setup(t)
	if _, err := e.owner.Exec(context.Background(), `INSERT INTO jobs (executor,type,payload,priority,fire_key,status,created_at,next_run_at,max_retries,retry_count)
		VALUES ('python','source_health_check','{}',1,'source_health:other','pending',now() AT TIME ZONE 'UTC',now() AT TIME ZONE 'UTC' + interval '1 day',3,0)`); err != nil {
		t.Fatal(err)
	}
	f := &fake{names: nil}
	if err := HandleSourceHealthCheck(context.Background(), e.deps(f, nil), e.claim(t, "processing")); err != nil {
		t.Fatal(err)
	}
	if p := e.pending(t); len(p) != 1 || p[0].fireKey != "source_health:other" {
		t.Fatalf("%v", p)
	}
}
