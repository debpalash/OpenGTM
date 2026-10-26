//go:build integration

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audiencerefresh"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/destdeliver"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/destsync"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/telemetry"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

type audEnv struct {
	pool, owner *pgxpool.Pool
	ws          string
}

var audCtr atomic.Int32

func setupAud(t *testing.T) *audEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	owner, err := pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM jobs`); err != nil {
		t.Fatal(err)
	}
	ws := fmt.Sprintf("wsaud%d-%d", os.Getpid(), audCtr.Add(1))
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id,name,slug,description,icon,created_at,updated_at) VALUES ($1::text,$1::text,$1::text,'','',1,1)`, ws); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = owner.Exec(c, `DELETE FROM jobs WHERE workspace_id=$1`, ws)
		for _, tbl := range []string{"destination_deliveries", "destination_runs", "audience_destinations", "audience_membership_events",
			"audience_members", "audience_schedules", "audiences"} {
			_, _ = owner.Exec(c, `DELETE FROM `+tbl+` WHERE workspace_id=$1`, ws)
		}
		_, _ = owner.Exec(c, `DELETE FROM workspaces WHERE id=$1`, ws)
	})
	return &audEnv{pool: pool, owner: owner, ws: ws}
}

func audConfig(ws string, destTypes ...string) config.Config {
	return config.Config{
		HeartbeatInterval: time.Hour, JobTimeout: time.Minute, AppEnv: "test",
		Executor: config.ExecutorSettings{Enabled: true, Workspaces: map[string]bool{ws: true}},
		Flags: config.Flags{Audiences: config.AudienceSettings{GoExecutorAudienceRefresh: true, GoDestTypes: destTypes,
			RefreshPageSize: 500, DestMemberPageSize: 500, DestAdBatchSize: 5000, DestDeliveryTimeout: 30 * time.Second}},
	}
}

type nopEmitter struct{ calls atomic.Int32 }

func (e *nopEmitter) AudienceEvents(context.Context, pgx.Tx, string, []audiencerefresh.MembershipEvent) (int, error) {
	e.calls.Add(1)
	return 0, nil
}

type fakeWebhook struct {
	mu    sync.Mutex
	sent  int
	panic bool
}

func (f *fakeWebhook) Send(context.Context, destdeliver.WebhookRequest) (destdeliver.Result, error) {
	if f.panic {
		panic("boom")
	}
	f.mu.Lock()
	f.sent++
	f.mu.Unlock()
	return destdeliver.Result{Success: true, Summary: "ok"}, nil
}

func (f *fakeWebhook) count() int { f.mu.Lock(); defer f.mu.Unlock(); return f.sent }

type noSecrets struct{}

func (noSecrets) Get(context.Context, string, string) (string, string, error) { return "", "", nil }

// runAud runs the audience handlers over a slot until no pending go job is due.
func (g *audEnv) runAud(t *testing.T, cfg config.Config, em audiencerefresh.Emitter, o audienceOpts) {
	t.Helper()
	ctx := context.Background()
	log := telemetry.NewLogger(io.Discard)
	rn := dispatch(audienceHandlers(cfg, g.pool, log, noSecrets{}, em, o))
	cctx, stop := context.WithTimeout(ctx, 20*time.Second)
	defer stop()
	done := make(chan struct{})
	go func() { slotLoop(cctx, ctx, g.pool, cfg, log, "w", rn); close(done) }()
	for cctx.Err() == nil {
		var n int
		_ = g.pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE executor='go' AND status IN ('pending','processing') AND (next_run_at IS NULL OR next_run_at <= now() AT TIME ZONE 'UTC')`).Scan(&n)
		if n == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	stop()
	<-done
}

func (g *audEnv) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := g.owner.Exec(context.Background(), q, args...); err != nil {
		t.Fatal(err)
	}
}

func (g *audEnv) audience(t *testing.T, id string, enabled bool) {
	t.Helper()
	g.exec(t, `INSERT INTO audiences (id, workspace_id, name, filters, refresh_enabled, refresh_interval_minutes) VALUES ($1,$2,'a','{}'::json,$3,60)`, id, g.ws, enabled)
}

func (g *audEnv) job(t *testing.T, typ string, payload map[string]any, executor string, maxRetries int) (id int64) {
	t.Helper()
	b, _ := json.Marshal(payload)
	if err := g.owner.QueryRow(context.Background(), `INSERT INTO jobs (type,payload,status,priority,created_at,retry_count,max_retries,workspace_id,executor)
		VALUES ($1,$2,'pending',0,now() AT TIME ZONE 'UTC',0,$3,$4,$5) RETURNING id`, typ, string(b), maxRetries, g.ws, executor).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return
}

func (g *audEnv) jobRow(t *testing.T, id int64) (status, executor string, retries int) {
	t.Helper()
	if err := g.owner.QueryRow(context.Background(), `SELECT status, executor, retry_count FROM jobs WHERE id=$1`, id).Scan(&status, &executor, &retries); err != nil {
		t.Fatal(err)
	}
	return
}

func (g *audEnv) pendingNext(t *testing.T, aud string) (n int, executor string) {
	t.Helper()
	_ = g.owner.QueryRow(context.Background(), `SELECT count(*), coalesce(max(executor),'') FROM jobs WHERE type='audience_refresh' AND status='pending' AND payload::jsonb->>'audience_id'=$1`, aud).Scan(&n, &executor)
	return
}

func (g *audEnv) destRun(t *testing.T, destType string, members int) (runID string, jobID func(maxRetries int, executor string) int64) {
	t.Helper()
	g.audience(t, "aud1", false)
	g.exec(t, `INSERT INTO audience_destinations (id, workspace_id, audience_id, name, destination_type, enabled, config, field_map, health_status)
		VALUES ('d1',$1,'aud1','d1',$2,true,'{"url":"https://hooks.example.test/x"}'::json,'{}'::json,'unverified')`, g.ws, destType)
	runID = "run1"
	g.exec(t, `INSERT INTO destination_runs (id, workspace_id, destination_id, status, created_at) VALUES ($1,$2,'d1','pending','2026-08-02 09:00:00')`, runID, g.ws)
	for i := 1; i <= members; i++ {
		g.exec(t, `INSERT INTO audience_members (workspace_id, audience_id, lead_id, snapshot) VALUES ($1,'aud1',$2,$3::json)`, g.ws, i, fmt.Sprintf(`{"id":%d,"email":"u%d@x.test"}`, i, i))
	}
	return runID, func(maxRetries int, executor string) int64 {
		return g.job(t, "audience_destination_sync", map[string]any{"workspace_id": g.ws, "run_id": runID}, executor, maxRetries)
	}
}

func (g *audEnv) runState(t *testing.T, id string) (status, errText string) {
	t.Helper()
	var e *string
	if err := g.owner.QueryRow(context.Background(), `SELECT status, error FROM destination_runs WHERE id=$1`, id).Scan(&status, &e); err != nil {
		t.Fatal(err)
	}
	if e != nil {
		errText = *e
	}
	return
}

func TestWorkerRunsAudienceRefreshAndReschedules(t *testing.T) {
	g := setupAud(t)
	g.audience(t, "aud1", true)
	job := g.job(t, "audience_refresh", map[string]any{"workspace_id": g.ws, "audience_id": "aud1"}, "go", 3)
	em := &nopEmitter{}
	g.runAud(t, audConfig(g.ws), em, audienceOpts{})
	if st, _, _ := g.jobRow(t, job); st != "completed" {
		t.Fatalf("job %s", st)
	}
	var health string
	var next *time.Time
	if err := g.owner.QueryRow(context.Background(), `SELECT refresh_health, next_refresh_at FROM audiences WHERE id='aud1'`).Scan(&health, &next); err != nil {
		t.Fatal(err)
	}
	if health != "healthy" || next == nil {
		t.Fatalf("health %q next %v", health, next)
	}
	if n, ex := g.pendingNext(t, "aud1"); n != 1 || ex != "go" {
		t.Fatalf("next job: %d pending, executor %q", n, ex)
	}
}

// A refresh that fails still schedules the next occurrence before the error goes back to the queue.
func TestWorkerRefreshErrorStillSchedulesNext(t *testing.T) {
	g := setupAud(t)
	g.audience(t, "aud1", true)
	job := g.job(t, "audience_refresh", map[string]any{"workspace_id": g.ws, "audience_id": "aud1"}, "go", 0)
	g.runAud(t, audConfig(g.ws), &nopEmitter{}, audienceOpts{leads: func(context.Context, pgx.Tx, map[string]any, int, int) ([]json.RawMessage, int, error) {
		return nil, 0, errors.New("source down")
	}})
	if st, _, _ := g.jobRow(t, job); st != "failed" {
		t.Fatalf("job %s", st)
	}
	var health string
	var fails int
	if err := g.owner.QueryRow(context.Background(), `SELECT refresh_health, consecutive_refresh_failures FROM audiences WHERE id='aud1'`).Scan(&health, &fails); err != nil {
		t.Fatal(err)
	}
	if health != "degraded" || fails != 1 {
		t.Fatalf("health %q failures %d (the failure hook must not count the attempt twice)", health, fails)
	}
	if n, _ := g.pendingNext(t, "aud1"); n != 1 {
		t.Fatalf("%d pending next jobs, want 1", n)
	}
}

func TestWorkerRefreshMissingOrDisabledAudienceRemovesSchedule(t *testing.T) {
	g := setupAud(t)
	g.audience(t, "off", false)
	g.exec(t, `INSERT INTO audience_schedules (audience_id, workspace_id, next_refresh_at, enabled) VALUES ('off',$1,now() AT TIME ZONE 'UTC',true),('gone',$1,now() AT TIME ZONE 'UTC',true)`, g.ws)
	a := g.job(t, "audience_refresh", map[string]any{"workspace_id": g.ws, "audience_id": "off"}, "go", 3)
	b := g.job(t, "audience_refresh", map[string]any{"workspace_id": g.ws, "audience_id": "gone"}, "go", 3)
	g.runAud(t, audConfig(g.ws), &nopEmitter{}, audienceOpts{})
	for _, j := range []int64{a, b} {
		if st, _, _ := g.jobRow(t, j); st != "completed" {
			t.Fatalf("job %d %s", j, st)
		}
	}
	var n int
	_ = g.owner.QueryRow(context.Background(), `SELECT count(*) FROM audience_schedules WHERE workspace_id=$1`, g.ws).Scan(&n)
	if n != 0 {
		t.Fatalf("%d schedule rows left", n)
	}
}

func TestWorkerRunsDestinationSync(t *testing.T) {
	g := setupAud(t)
	run, mk := g.destRun(t, "webhook", 2)
	job := mk(3, "go")
	wh := &fakeWebhook{}
	g.runAud(t, audConfig(g.ws, "webhook"), &nopEmitter{}, audienceOpts{ports: destdeliver.Ports{Webhook: wh}})
	if st, _, _ := g.jobRow(t, job); st != "completed" {
		t.Fatalf("job %s", st)
	}
	if s, e := g.runState(t, run); s != "completed" || wh.count() != 2 {
		t.Fatalf("run %s %q, sent %d", s, e, wh.count())
	}
}

func TestWorkerIgnoresPythonRows(t *testing.T) {
	g := setupAud(t)
	g.audience(t, "aud1", true)
	r := g.job(t, "audience_refresh", map[string]any{"workspace_id": g.ws, "audience_id": "aud1"}, "python", 3)
	_, mk := g.destRun(t, "webhook", 1)
	d := mk(3, "python")
	g.runAud(t, audConfig(g.ws, "webhook"), &nopEmitter{}, audienceOpts{ports: destdeliver.Ports{Webhook: &fakeWebhook{}}})
	for _, j := range []int64{r, d} {
		if st, ex, _ := g.jobRow(t, j); st != "pending" || ex != "python" {
			t.Fatalf("job %d %s/%s, want untouched", j, st, ex)
		}
	}
}

// A destination type outside GO_DEST_TYPES, or listed but without a delivery port, is handed
// back to Python untouched (no retry spent), never failed.
func TestWorkerHandsBackIneligibleType(t *testing.T) {
	for _, tc := range []struct {
		name, typ string
		goTypes   []string
		ports     destdeliver.Ports
	}{
		{"not listed", "hubspot", []string{"webhook"}, destdeliver.Ports{Webhook: &fakeWebhook{}}},
		{"listed without port", "webhook", []string{"webhook"}, destdeliver.Ports{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := setupAud(t)
			run, mk := g.destRun(t, tc.typ, 1)
			job := mk(3, "go")
			g.runAud(t, audConfig(g.ws, tc.goTypes...), &nopEmitter{}, audienceOpts{ports: tc.ports})
			if st, ex, rt := g.jobRow(t, job); st != "pending" || ex != "python" || rt != 0 {
				t.Fatalf("job %s/%s retries %d", st, ex, rt)
			}
			if s, _ := g.runState(t, run); s != "pending" {
				t.Fatalf("run %s, want pending (as found)", s)
			}
		})
	}
}

// With no emitter (no AudienceEvents) or the flag off, audience_refresh rows are handed back.
func TestWorkerNoEmitterNoRefreshHandler(t *testing.T) {
	for _, tc := range []struct {
		name string
		em   audiencerefresh.Emitter
		on   bool
	}{{"no emitter", nil, true}, {"flag off", &nopEmitter{}, false}} {
		t.Run(tc.name, func(t *testing.T) {
			g := setupAud(t)
			g.audience(t, "aud1", true)
			cfg := audConfig(g.ws)
			cfg.Flags.Audiences.GoExecutorAudienceRefresh = tc.on
			job := g.job(t, "audience_refresh", map[string]any{"workspace_id": g.ws, "audience_id": "aud1"}, "go", 3)
			rn := dispatch(audienceHandlers(cfg, g.pool, telemetry.NewLogger(io.Discard), noSecrets{}, tc.em, audienceOpts{}))
			c, err := claimNext(context.Background(), g.pool, cfg, "w")
			if err != nil || c == nil || c.Type != "audience_refresh" {
				t.Fatalf("claim %v err %v", c, err)
			}
			if err := rn(context.Background(), c); !errors.Is(err, workbook.ErrHandedBack) {
				t.Fatalf("err %v, want ErrHandedBack", err)
			}
			if st, ex, rt := g.jobRow(t, job); st != "pending" || ex != "python" || rt != 0 {
				t.Fatalf("job %s/%s retries %d", st, ex, rt)
			}
			if h := audienceHealth(t, g); h != "unverified" {
				t.Fatalf("audience touched: %s", h)
			}
		})
	}
}

func audienceHealth(t *testing.T, g *audEnv) string {
	t.Helper()
	var h string
	if err := g.owner.QueryRow(context.Background(), `SELECT refresh_health FROM audiences WHERE id='aud1'`).Scan(&h); err != nil {
		t.Fatal(err)
	}
	return h
}

// A panic in a refresh or a sync (no handler-side bookkeeping) is recovered by the slot and the
// failure hooks reconcile the audience and the run.
func TestWorkerFailureHooksReconcile(t *testing.T) {
	t.Run("audience_refresh", func(t *testing.T) {
		g := setupAud(t)
		g.audience(t, "aud1", true)
		job := g.job(t, "audience_refresh", map[string]any{"workspace_id": g.ws, "audience_id": "aud1"}, "go", 0)
		g.runAud(t, audConfig(g.ws), &nopEmitter{}, audienceOpts{leads: func(context.Context, pgx.Tx, map[string]any, int, int) ([]json.RawMessage, int, error) {
			panic("boom")
		}})
		if st, _, _ := g.jobRow(t, job); st != "failed" {
			t.Fatalf("job %s", st)
		}
		var health, lastErr string
		var fails int
		if err := g.owner.QueryRow(context.Background(), `SELECT refresh_health, coalesce(last_refresh_error,''), consecutive_refresh_failures FROM audiences WHERE id='aud1'`).Scan(&health, &lastErr, &fails); err != nil {
			t.Fatal(err)
		}
		if health != "degraded" || fails != 1 || len(lastErr) < 14 || lastErr[:14] != "Final failure:" {
			t.Fatalf("health %q failures %d err %q", health, fails, lastErr)
		}
		if n, _ := g.pendingNext(t, "aud1"); n != 1 {
			t.Fatalf("%d pending next jobs", n)
		}
	})
	t.Run("audience_destination_sync", func(t *testing.T) {
		g := setupAud(t)
		run, mk := g.destRun(t, "webhook", 1)
		job := mk(0, "go")
		g.runAud(t, audConfig(g.ws, "webhook"), &nopEmitter{}, audienceOpts{ports: destdeliver.Ports{Webhook: &fakeWebhook{panic: true}}})
		if st, _, _ := g.jobRow(t, job); st != "failed" {
			t.Fatalf("job %s", st)
		}
		if s, e := g.runState(t, run); s != "failed" || len(e) < 14 || e[:14] != "Final failure:" {
			t.Fatalf("run %s %q", s, e)
		}
	})
}

// The failure hooks validate the payload workspace against the job row before acting.
func TestFailureHooksRejectForeignPayloadWorkspace(t *testing.T) {
	g := setupAud(t)
	g.audience(t, "aud1", true)
	run, _ := g.destRun(t, "webhook", 0)
	hooks := map[string]queue.FailureHook{
		"audience_refresh":          refreshFailureHook(g.pool, nil),
		"audience_destination_sync": destFailureHook(destsync.New(destsync.Deps{Pool: g.pool}), g.pool),
	}
	for typ, hook := range hooks {
		p, _ := json.Marshal(map[string]any{"workspace_id": g.ws, "audience_id": "aud1", "run_id": run})
		err := hook(context.Background(), queue.FailureNotice{JobID: 1, JobType: typ, WorkspaceID: "someone-else", Payload: p, Reason: "x"})
		if err == nil {
			t.Errorf("%s: hook accepted a payload workspace that is not the job's", typ)
		}
	}
	if h := audienceHealth(t, g); h != "unverified" {
		t.Fatalf("audience touched: %s", h)
	}
	if s, _ := g.runState(t, run); s != "pending" {
		t.Fatalf("run touched: %s", s)
	}
}

// A refresh job whose payload workspace is not the job row's is refused permanently.
func TestAudienceRefreshRefusesForeignPayloadWorkspace(t *testing.T) {
	g := setupAud(t)
	g.audience(t, "aud1", true)
	h := audienceHandlers(audConfig(g.ws), g.pool, telemetry.NewLogger(io.Discard), noSecrets{}, &nopEmitter{}, audienceOpts{})["audience_refresh"]
	p, _ := json.Marshal(map[string]any{"workspace_id": "other", "audience_id": "aud1"})
	err := h(context.Background(), &queue.Claim{ID: 1, Type: "audience_refresh", WorkspaceID: g.ws, Payload: p})
	if err == nil || !queue.IsPermanent(err) {
		t.Fatalf("err %v, want permanent", err)
	}
}
