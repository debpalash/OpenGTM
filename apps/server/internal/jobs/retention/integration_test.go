package retention

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/server/internal/db/dbtest"
	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
)

// frozen is the instant every test treats as "now".
var frozen = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

const frozenSQL = "TIMESTAMP '2026-06-15 12:00:00'"

type env struct {
	t      *testing.T
	owner  *pgxpool.Pool // schema owner: seeds and inspects, bypasses RLS
	app    *pgxpool.Pool // NOSUPERUSER NOBYPASSRLS runtime role: what the worker uses
	worker *Worker
	lib    string
	spaces []string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ownerURL := dbtest.OwnerURL(t)
	dbtest.Migrate(t, ownerURL)
	lib, err := os.ReadFile(filepath.Join(dbtest.RepoRoot(t), "tests", "retention_parity", "dataset_lib.sql"))
	if err != nil {
		t.Fatal(err)
	}
	// The "signals" cutoff reads the naive wall clock in the process's local
	// zone (as Python's datetime.timestamp() does). The seed is UTC based, so
	// pin the zone; TestEpochCutoffUsesLocalZone covers the other zones.
	prev := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = prev })
	e := &env{
		t:     t,
		owner: dbtest.Pool(t, ownerURL, 4),
		app:   dbtest.Pool(t, dbtest.AppURL(t, ownerURL), 8),
		lib:   string(lib),
	}
	e.worker = NewWorker(e.app, nil)
	e.worker.Now = func() time.Time { return frozen }
	e.worker.BatchSize = 7 // force many batches per table
	t.Cleanup(e.cleanup)
	return e
}

// ws returns a unique workspace id and schedules its cleanup.
func (e *env) ws(label string) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	ws := "rt-" + label + "-" + hex.EncodeToString(b[:])
	e.spaces = append(e.spaces, ws)
	return ws
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.owner.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

func (e *env) scalar(sql string, args ...any) string {
	e.t.Helper()
	var v *string
	if err := e.owner.QueryRow(context.Background(), "SELECT ("+sql+")::text", args...).Scan(&v); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
	if v == nil {
		return "<nil>"
	}
	return *v
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// seed writes the shared dataset (see tests/retention_parity/dataset_lib.sql).
func (e *env) seed(ws string) {
	e.t.Helper()
	conn, err := e.owner.Acquire(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	defer conn.Release()
	// No arguments: simple protocol, so the multi-statement script is allowed.
	if _, err := conn.Exec(context.Background(), e.lib+"\nSELECT pg_temp.seed_rows("+quote(ws)+", "+frozenSQL+");"); err != nil {
		e.t.Fatalf("seed %s: %v", ws, err)
	}
}

func (e *env) policy(ws string, enabled, hold bool, days string) {
	e.t.Helper()
	e.exec(`INSERT INTO retention_policies (workspace_id, enabled, legal_hold, retention_days)
		VALUES ($1, $2, $3, $4::json)`, ws, enabled, hold, days)
}

func (e *env) run(ws, id, requestedBy, snapshot string) {
	e.t.Helper()
	e.exec(`INSERT INTO retention_runs (id, workspace_id, status, requested_by, policy_snapshot, deleted_counts)
		VALUES ($1, $2, 'pending', $3, $4::json, '{}'::json)`, id, ws, requestedBy, snapshot)
}

// job inserts a processing retention_enforce job holding a lease and returns
// it as the queue would hand it to the handler.
func (e *env) job(ws string, payload map[string]any) queue.Job {
	e.t.Helper()
	raw, _ := json.Marshal(payload)
	j := queue.Job{Type: JobType, Payload: raw, WorkspaceID: ws, Lease: queue.Lease{WorkerID: "test-worker"}}
	err := e.owner.QueryRow(context.Background(), `INSERT INTO jobs
		(type, payload, workspace_id, priority, status, created_at, next_run_at, max_retries, retry_count, worker_id, locked_at)
		VALUES ('retention_enforce', $1::json, $2, 1, 'processing', LOCALTIMESTAMP, LOCALTIMESTAMP, 3, 0, 'test-worker', LOCALTIMESTAMP)
		RETURNING id, locked_at`, string(raw), ws).Scan(&j.ID, &j.Lease.LockedAt)
	if err != nil {
		e.t.Fatal(err)
	}
	return j
}

func (e *env) cleanup() {
	ctx := context.Background()
	for _, ws := range e.spaces {
		for _, t := range []string{
			"governance_audit_events", "llm_usage_daily", "signals", "destination_deliveries",
			"destination_inbound_receipts", "audience_membership_events", "playbook_results", "outreach_sends",
			"playbook_runs", "research_playbooks", "destination_runs", "audience_destinations", "audiences",
			"retention_runs", "retention_policies", "retention_schedules",
		} {
			_, _ = e.owner.Exec(ctx, "DELETE FROM "+t+" WHERE workspace_id = $1", ws)
		}
		_, _ = e.owner.Exec(ctx, `DELETE FROM jobs WHERE type = $1 AND (workspace_id = $2 OR payload->>'workspace_id' = $2)`, JobType, ws)
	}
}

func (e *env) count(table, ws string) int {
	e.t.Helper()
	var n int
	if err := e.owner.QueryRow(context.Background(), "SELECT count(*) FROM "+table+" WHERE workspace_id = $1", ws).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// The seeded dataset: these ages (days) x offsets (seconds older).
var (
	seedAges = []int{1, 30, 45, 90, 100, 179, 180, 181, 364, 365, 366, 400, 3650}
	seedOffs = []int{1, 0, -1}
)

// seeded is the row count per table for one seed call (signals and
// outreach_sends carry one extra NULL-timestamp row).
func seeded(table string) int {
	n := len(seedAges) * len(seedOffs)
	if table == "signals" || table == "outreach_sends" {
		n++
	}
	return n
}

// expired counts seeded rows strictly older than `days` before frozen. Dates
// compare by calendar day (the seed is at noon, so only whole ages matter).
func expired(days int, dateOnly bool) int {
	n := 0
	for _, a := range seedAges {
		for _, o := range seedOffs {
			if dateOnly {
				if a > days {
					n++
				}
			} else if a*86400+o > days*86400 {
				n++
			}
		}
	}
	return n
}

func (e *env) runRow(id string) (status, errText, counts, snapshot string) {
	e.t.Helper()
	status = e.scalar(`SELECT status FROM retention_runs WHERE id = $1`, id)
	errText = e.scalar(`SELECT error FROM retention_runs WHERE id = $1`, id)
	counts = e.scalar(`SELECT deleted_counts::text FROM retention_runs WHERE id = $1`, id)
	snapshot = e.scalar(`SELECT policy_snapshot::text FROM retention_runs WHERE id = $1`, id)
	return
}

const defaultPolicyJSON = `{"audit": 365, "llm_usage": 365, "signals": 365, "activation": 180, "audience_history": 365, "agent_results": 180, "outreach_history": 365}`

func TestEnforceDeletesExpiredRowsAcrossAllTables(t *testing.T) {
	e := newEnv(t)
	ws := e.ws("all")
	e.seed(ws)
	e.policy(ws, true, false, defaultPolicyJSON)
	runID := "run-" + ws
	e.run(ws, runID, "1", defaultPolicyJSON)
	job := e.job(ws, map[string]any{"workspace_id": ws, "run_id": runID})

	if err := e.worker.Handle(context.Background(), job); err != nil {
		t.Fatal(err)
	}

	want := map[string]int{
		"audit":            expired(365, false),
		"llm_usage":        expired(365, true),
		"signals":          expired(365, false),
		"activation":       2 * expired(180, false),
		"audience_history": expired(365, false),
		"agent_results":    expired(180, false),
		"outreach_history": expired(365, false),
	}
	status, errText, counts, _ := e.runRow(runID)
	if status != "completed" || errText != "<nil>" {
		t.Fatalf("run = %s / %s", status, errText)
	}
	var got map[string]int
	if err := json.Unmarshal([]byte(counts), &got); err != nil {
		t.Fatal(err)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("deleted_counts[%s] = %d, want %d (all: %s)", k, got[k], v, counts)
		}
	}
	// Byte-for-byte what Python's json.dumps would store.
	if !strings.HasPrefix(counts, `{"audit": `) || !strings.Contains(counts, `, "llm_usage": `) {
		t.Errorf("deleted_counts text = %s", counts)
	}
	remaining := map[string]int{
		"governance_audit_events":      want["audit"],
		"llm_usage_daily":              want["llm_usage"],
		"signals":                      want["signals"],
		"destination_deliveries":       expired(180, false),
		"destination_inbound_receipts": expired(180, false),
		"audience_membership_events":   want["audience_history"],
		"playbook_results":             want["agent_results"],
		"outreach_sends":               want["outreach_history"],
	}
	for table, del := range remaining {
		if n := e.count(table, ws); n != seeded(table)-del {
			t.Errorf("%s has %d rows, want %d", table, n, seeded(table)-del)
		}
	}
	if n := e.scalar(`SELECT count(*) FROM signals WHERE workspace_id = $1 AND created_at IS NULL`, ws); n != "1" {
		t.Errorf("NULL-timestamp signal must survive, count = %s", n)
	}

	// Next run: policy.next_run_at, the mirror and one pending job, keyed by date.
	next := "'2026-06-16 12:00:00+00'::timestamptz::timestamp"
	if e.scalar(`SELECT next_run_at = `+next+` FROM retention_policies WHERE workspace_id = $1`, ws) != "true" {
		t.Error("policy.next_run_at not set to now+1d")
	}
	if e.scalar(`SELECT enabled AND next_run_at = `+next+` FROM retention_schedules WHERE workspace_id = $1`, ws) != "true" {
		t.Error("schedule mirror not updated")
	}
	key := "retention:" + ws + ":2026-06-16"
	if e.scalar(`SELECT count(*) FROM jobs WHERE fire_key = $1 AND status = 'pending' AND type = 'retention_enforce'
		AND workspace_id IS NULL AND next_run_at = `+next+` AND payload->>'workspace_id' = $2`, key, ws) != "1" {
		t.Errorf("expected exactly one pending job %s", key)
	}
	// Idempotent: a completed run is never re-run.
	job2 := e.job(ws, map[string]any{"workspace_id": ws, "run_id": runID})
	if err := e.worker.Handle(context.Background(), job2); err != nil {
		t.Fatal(err)
	}
	if _, _, again, _ := e.runRow(runID); again != counts {
		t.Errorf("completed run changed: %s -> %s", counts, again)
	}
}

func TestTenantIsolation(t *testing.T) {
	e := newEnv(t)
	a, b := e.ws("a"), e.ws("b")
	for _, ws := range []string{a, b} {
		e.seed(ws)
		e.policy(ws, true, false, defaultPolicyJSON)
	}
	e.run(a, "run-"+a, "1", defaultPolicyJSON)
	e.run(b, "run-"+b, "1", defaultPolicyJSON)

	if err := e.worker.Handle(context.Background(), e.job(a, map[string]any{"workspace_id": a, "run_id": "run-" + a})); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"governance_audit_events", "signals", "outreach_sends", "destination_deliveries"} {
		if n := e.count(table, b); n != seeded(table) {
			t.Errorf("tenant B lost rows in %s: %d of %d", table, n, seeded(table))
		}
	}
	if s, _, _, _ := e.runRow("run-" + b); s != "pending" {
		t.Errorf("tenant B run status = %s", s)
	}

	// A run id from another tenant is invisible under RLS: the handler
	// creates its own scheduler run for the job's workspace and leaves the
	// foreign run alone.
	other := e.ws("c")
	e.seed(other)
	e.policy(other, true, false, defaultPolicyJSON)
	if err := e.worker.Handle(context.Background(), e.job(other, map[string]any{"workspace_id": other, "run_id": "run-" + b})); err != nil {
		t.Fatal(err)
	}
	if s, _, _, _ := e.runRow("run-" + b); s != "pending" {
		t.Errorf("foreign run was driven by another tenant: %s", s)
	}
	if e.scalar(`SELECT count(*) FROM retention_runs WHERE workspace_id = $1 AND requested_by = 'scheduler' AND status = 'completed'`, other) != "1" {
		t.Error("expected a completed scheduler run for the job's own workspace")
	}
	if n := e.count("governance_audit_events", b); n != seeded("governance_audit_events") {
		t.Errorf("tenant B audit rows changed: %d", n)
	}

	// Outside a tenant transaction the runtime role sees nothing.
	var visible int
	if err := e.app.QueryRow(context.Background(), `SELECT count(*) FROM retention_runs`).Scan(&visible); err != nil {
		t.Fatal(err)
	}
	if visible != 0 {
		t.Errorf("RLS must fail closed without app.workspace_id; saw %d runs", visible)
	}
}

func TestCustomRetentionDays(t *testing.T) {
	e := newEnv(t)
	ws := e.ws("custom")
	e.seed(ws)
	custom := `{"audit": 90, "llm_usage": 30, "signals": 30, "activation": 100, "audience_history": 3650, "agent_results": 45, "outreach_history": 365}`
	e.policy(ws, true, false, custom)
	e.run(ws, "run-"+ws, "9", custom)
	if err := e.worker.Handle(context.Background(), e.job(ws, map[string]any{"workspace_id": ws, "run_id": "run-" + ws})); err != nil {
		t.Fatal(err)
	}
	cases := map[string]int{
		"governance_audit_events":      seeded("governance_audit_events") - expired(90, false),
		"llm_usage_daily":              seeded("llm_usage_daily") - expired(30, true),
		"signals":                      seeded("signals") - expired(30, false),
		"destination_deliveries":       seeded("destination_deliveries") - expired(100, false),
		"audience_membership_events":   seeded("audience_membership_events") - expired(3650, false),
		"playbook_results":             seeded("playbook_results") - expired(45, false),
		"outreach_sends":               seeded("outreach_sends") - expired(365, false),
		"destination_inbound_receipts": seeded("destination_inbound_receipts") - expired(100, false),
	}
	for table, want := range cases {
		if got := e.count(table, ws); got != want {
			t.Errorf("%s has %d rows, want %d", table, got, want)
		}
	}
}

func TestLegalHoldAndDisabledSchedule(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	hold := e.ws("hold")
	e.seed(hold)
	e.policy(hold, true, true, defaultPolicyJSON)
	e.run(hold, "run-"+hold, "1", defaultPolicyJSON) // even a manual run is blocked
	e.exec(`INSERT INTO retention_schedules (workspace_id, enabled, next_run_at) VALUES ($1, true, LOCALTIMESTAMP)`, hold)
	e.exec(`INSERT INTO jobs (type, payload, status, fire_key, next_run_at, priority, max_retries, retry_count)
		VALUES ('retention_enforce', '{}', 'pending', $1, LOCALTIMESTAMP, 1, 3, 0)`, "retention:"+hold+":2026-06-16")
	if err := e.worker.Handle(ctx, e.job(hold, map[string]any{"workspace_id": hold, "run_id": "run-" + hold})); err != nil {
		t.Fatal(err)
	}
	status, errText, _, _ := e.runRow("run-" + hold)
	if status != "cancelled" || errText != "legal hold or scheduled retention disabled" {
		t.Fatalf("hold run = %s / %s", status, errText)
	}
	if n := e.count("governance_audit_events", hold); n != seeded("governance_audit_events") {
		t.Errorf("legal hold deleted rows: %d left", n)
	}
	if e.scalar(`SELECT count(*) FROM jobs WHERE fire_key = $1 AND status = 'cancelled'`, "retention:"+hold+":2026-06-16") != "1" {
		t.Error("pending retention job must be cancelled by the reschedule")
	}
	if e.scalar(`SELECT enabled::text || coalesce(next_run_at::text, 'null') FROM retention_schedules WHERE workspace_id = $1`, hold) != "falsenull" {
		t.Error("mirror must be disabled")
	}
	if e.scalar(`SELECT count(*) FROM jobs WHERE fire_key LIKE $1 AND status = 'pending'`, "retention:"+hold+":%") != "0" {
		t.Error("legal hold must not schedule a next run")
	}

	// Disabled schedule: a scheduler run is cancelled; a manual run still executes.
	off := e.ws("off")
	e.seed(off)
	e.policy(off, false, false, defaultPolicyJSON)
	if err := e.worker.Handle(ctx, e.job(off, map[string]any{"workspace_id": off})); err != nil {
		t.Fatal(err)
	}
	if e.scalar(`SELECT status || '/' || error FROM retention_runs WHERE workspace_id = $1`, off) != "cancelled/legal hold or scheduled retention disabled" {
		t.Error("disabled scheduler run must be cancelled")
	}
	if n := e.count("signals", off); n != seeded("signals") {
		t.Errorf("disabled policy deleted rows: %d", n)
	}
	e.run(off, "run-"+off, "7", defaultPolicyJSON)
	if err := e.worker.Handle(ctx, e.job(off, map[string]any{"workspace_id": off, "run_id": "run-" + off})); err != nil {
		t.Fatal(err)
	}
	if s, _, _, _ := e.runRow("run-" + off); s != "completed" {
		t.Errorf("manual run on a disabled policy = %s, want completed", s)
	}
	if e.scalar(`SELECT count(*) FROM jobs WHERE fire_key LIKE $1`, "retention:"+off+":%") != "0" {
		t.Error("disabled policy must not schedule a next run")
	}
}

func TestNoPolicyAndTerminalRunsAreLeftAlone(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ws := e.ws("none")
	e.seed(ws)
	e.run(ws, "run-"+ws, "1", defaultPolicyJSON)
	if err := e.worker.Handle(ctx, e.job(ws, map[string]any{"workspace_id": ws, "run_id": "run-" + ws})); err != nil {
		t.Fatal(err)
	}
	if s, _, _, _ := e.runRow("run-" + ws); s != "pending" {
		t.Errorf("run without a policy must be untouched, got %s", s)
	}
	if n := e.count("signals", ws); n != seeded("signals") {
		t.Errorf("rows deleted without a policy: %d", n)
	}
	if err := e.worker.Handle(ctx, e.job(ws, map[string]any{})); err == nil || err.Error() != "retention enforcement requires workspace_id" {
		t.Errorf("missing workspace error = %v", err)
	}

	e.policy(ws, true, false, defaultPolicyJSON)
	e.exec(`UPDATE retention_runs SET status = 'cancelled' WHERE id = $1`, "run-"+ws)
	if err := e.worker.Handle(ctx, e.job(ws, map[string]any{"workspace_id": ws, "run_id": "run-" + ws})); err != nil {
		t.Fatal(err)
	}
	if n := e.count("signals", ws); n != seeded("signals") {
		t.Errorf("cancelled run deleted rows: %d", n)
	}
}

func TestInvalidStoredPolicyFailsTheJob(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, tc := range []struct{ days, want string }{
		{`{"leads": 30, "audit": 5, "zzz": 1}`, "unsupported retention categories: leads, zzz"},
		{`{"audit": 30}`, "audit retention must be 90..3650 days"},
		{`{"signals": "abc"}`, "invalid literal for int() with base 10: 'abc'"},
		{`{"signals": null}`, "int() argument must be a string, a bytes-like object or a real number, not 'NoneType'"},
		{`{"llm_usage": 29.9}`, "llm_usage retention must be 30..3650 days"},
		{`{"llm_usage": " 4_0 "}`, ""},
	} {
		ws := e.ws("bad")
		e.policy(ws, true, false, tc.days)
		err := e.worker.Handle(ctx, e.job(ws, map[string]any{"workspace_id": ws}))
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: unexpected error %v", tc.days, err)
			}
			continue
		}
		if err == nil || err.Error() != tc.want {
			t.Errorf("%s: error = %v, want %q", tc.days, err, tc.want)
		}
		if e.scalar(`SELECT count(*) FROM retention_runs WHERE workspace_id = $1`, ws) != "0" {
			t.Errorf("%s: an invalid policy must not create a run", tc.days)
		}
	}
}

// A failure in the middle of the purge rolls back every earlier delete, marks
// the run failed with the error text, and returns the error so the queue
// retries.
func TestMidRunFailureRollsBackAndMarksFailed(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ws := e.ws("boom")
	e.seed(ws)
	e.policy(ws, true, false, defaultPolicyJSON)
	e.run(ws, "run-"+ws, "1", defaultPolicyJSON)
	fn := "rt_boom_" + strings.ReplaceAll(ws, "-", "_")
	e.exec(fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF OLD.workspace_id = %s THEN RAISE EXCEPTION 'boom'; END IF; RETURN OLD; END $$`, fn, quote(ws)))
	e.exec(fmt.Sprintf(`CREATE TRIGGER %s BEFORE DELETE ON signals FOR EACH ROW EXECUTE FUNCTION %s()`, fn, fn))
	t.Cleanup(func() {
		_, _ = e.owner.Exec(ctx, fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON signals", fn))
		_, _ = e.owner.Exec(ctx, fmt.Sprintf("DROP FUNCTION IF EXISTS %s()", fn))
	})

	err := e.worker.Handle(ctx, e.job(ws, map[string]any{"workspace_id": ws, "run_id": "run-" + ws}))
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error = %v", err)
	}
	status, errText, counts, _ := e.runRow("run-" + ws)
	if status != "failed" || !strings.Contains(errText, "boom") || counts != "{}" {
		t.Fatalf("run = %s / %s / %s", status, errText, counts)
	}
	if e.scalar(`SELECT finished_at IS NOT NULL FROM retention_runs WHERE id = $1`, "run-"+ws) != "true" {
		t.Error("failed run needs finished_at")
	}
	for _, table := range []string{"governance_audit_events", "llm_usage_daily", "signals"} {
		if n := e.count(table, ws); n != seeded(table) {
			t.Errorf("%s: partial deletion survived the failure (%d of %d rows)", table, n, seeded(table))
		}
	}
	if e.scalar(`SELECT count(*) FROM jobs WHERE fire_key LIKE $1`, "retention:"+ws+":%") != "0" {
		t.Error("a failed attempt must not schedule the next run (the reconciler does)")
	}
}

func TestSnapshotErrorsMatchPythonExceptionText(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, tc := range []struct{ snapshot, want string }{
		{`{"llm_usage": 30}`, "'audit'"}, // KeyError
		{`{"audit": "abc"}`, "unsupported type for timedelta days component: str"},
		{`{"audit": null}`, "unsupported type for timedelta days component: NoneType"},
		{`{"audit": [1]}`, "unsupported type for timedelta days component: list"},
		{`{"audit": 1000000000}`, "days=1000000000; must have magnitude <= 999999999"},
		{`{"audit": 800000000}`, "date value out of range"},
		{`[1]`, "list indices must be integers or slices, not str"},
		{`null`, "'NoneType' object is not subscriptable"},
	} {
		ws := e.ws("snap")
		e.policy(ws, true, false, defaultPolicyJSON)
		e.run(ws, "run-"+ws, "1", tc.snapshot)
		err := e.worker.Handle(ctx, e.job(ws, map[string]any{"workspace_id": ws, "run_id": "run-" + ws}))
		if err == nil || err.Error() != tc.want {
			t.Errorf("%s: error = %v, want %q", tc.snapshot, err, tc.want)
		}
		if _, got, _, _ := e.runRow("run-" + ws); got != tc.want {
			t.Errorf("%s: run.error = %q, want %q", tc.snapshot, got, tc.want)
		}
	}
}

func TestLeaseLostWritesNothing(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// Cancelled before the attempt starts.
	ws := e.ws("lease")
	e.seed(ws)
	e.policy(ws, true, false, defaultPolicyJSON)
	e.run(ws, "run-"+ws, "1", defaultPolicyJSON)
	job := e.job(ws, map[string]any{"workspace_id": ws, "run_id": "run-" + ws})
	e.exec(`UPDATE jobs SET status = 'cancelled' WHERE id = $1`, job.ID)
	if err := e.worker.Handle(ctx, job); !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("error = %v, want ErrLeaseLost", err)
	}
	if s, _, _, _ := e.runRow("run-" + ws); s != "pending" {
		t.Errorf("run changed after lease loss: %s", s)
	}
	if n := e.count("signals", ws); n != seeded("signals") {
		t.Errorf("rows deleted by a cancelled attempt: %d", n)
	}

	// Cancelled while purging: the whole purge rolls back.
	ws2 := e.ws("lease2")
	e.seed(ws2)
	e.policy(ws2, true, false, defaultPolicyJSON)
	e.run(ws2, "run-"+ws2, "1", defaultPolicyJSON)
	job2 := e.job(ws2, map[string]any{"workspace_id": ws2, "run_id": "run-" + ws2})
	e.worker.beforeFinish = func() { e.exec(`UPDATE jobs SET status = 'cancelled' WHERE id = $1`, job2.ID) }
	err := e.worker.Handle(ctx, job2)
	e.worker.beforeFinish = nil
	if !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("mid-purge error = %v, want ErrLeaseLost", err)
	}
	status, errText, counts, _ := e.runRow("run-" + ws2)
	if status != "running" || errText != "<nil>" || counts != "{}" {
		t.Errorf("run = %s / %s / %s; only the pre-purge 'running' marker may exist", status, errText, counts)
	}
	for _, table := range []string{"governance_audit_events", "signals", "outreach_sends", "playbook_results"} {
		if n := e.count(table, ws2); n != seeded(table) {
			t.Errorf("%s: %d of %d rows left after a lost lease", table, n, seeded(table))
		}
	}
	if e.scalar(`SELECT count(*) FROM jobs WHERE fire_key LIKE $1 AND status = 'pending'`, "retention:"+ws2+":%") != "0" {
		t.Error("a lost lease must not schedule a next run")
	}

	// Reclaimed by another worker (different lock): same outcome.
	ws3 := e.ws("lease3")
	e.seed(ws3)
	e.policy(ws3, true, false, defaultPolicyJSON)
	e.run(ws3, "run-"+ws3, "1", defaultPolicyJSON)
	job3 := e.job(ws3, map[string]any{"workspace_id": ws3, "run_id": "run-" + ws3})
	e.exec(`UPDATE jobs SET worker_id = 'someone-else', locked_at = locked_at + interval '1 second' WHERE id = $1`, job3.ID)
	if err := e.worker.Handle(ctx, job3); !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("reclaimed error = %v", err)
	}
	if n := e.count("signals", ws3); n != seeded("signals") {
		t.Errorf("rows deleted by a reclaimed attempt: %d", n)
	}
}

func TestReconcileWillRetryThenFinalFailureReschedules(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ws := e.ws("rc")
	e.policy(ws, true, false, `{"audit": 120}`)
	job := e.job(ws, map[string]any{"workspace_id": ws, "fire_key": "retention:" + ws + ":x"})

	if err := e.worker.Reconcile(ctx, job, "worker timeout", true); err != nil {
		t.Fatal(err)
	}
	var runID string
	if err := e.owner.QueryRow(ctx, `SELECT id FROM retention_runs WHERE workspace_id = $1`, ws).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	status, errText, _, snapshot := e.runRow(runID)
	if status != "pending" || errText != "Queue retry scheduled: worker timeout" {
		t.Errorf("run = %s / %s", status, errText)
	}
	if !strings.Contains(snapshot, `"audit": 120`) || !strings.Contains(snapshot, `"signals": 365`) {
		t.Errorf("snapshot not normalized from the policy: %s", snapshot)
	}
	if e.scalar(`SELECT payload->>'run_id' FROM jobs WHERE id = $1`, job.ID) != runID {
		t.Error("job payload must carry the new run id")
	}
	if e.scalar(`SELECT payload->>'fire_key' FROM jobs WHERE id = $1`, job.ID) != "retention:"+ws+":x" {
		t.Error("existing payload fields must be preserved")
	}

	job.Payload, _ = json.Marshal(map[string]any{"workspace_id": ws, "run_id": runID})
	if err := e.worker.Reconcile(ctx, job, "worker timeout", false); err != nil {
		t.Fatal(err)
	}
	status, errText, _, _ = e.runRow(runID)
	if status != "failed" || errText != "Final failure: worker timeout" {
		t.Errorf("run = %s / %s", status, errText)
	}
	if e.scalar(`SELECT count(*) FROM jobs WHERE fire_key = $1 AND status = 'pending'`, "retention:"+ws+":2026-06-16") != "1" {
		t.Error("final failure of an enabled policy must schedule the next run")
	}

	// Terminal runs are never rewritten.
	e.exec(`UPDATE retention_runs SET status = 'completed', error = NULL WHERE id = $1`, runID)
	if err := e.worker.Reconcile(ctx, job, "late", false); err != nil {
		t.Fatal(err)
	}
	if s, er, _, _ := e.runRow(runID); s != "completed" || er != "<nil>" {
		t.Errorf("completed run rewritten: %s / %s", s, er)
	}

	if err := e.worker.Reconcile(ctx, e.job(ws, map[string]any{}), "x", true); err == nil ||
		err.Error() != "retention failure payload requires workspace_id" {
		t.Errorf("empty payload error = %v", err)
	}
}

func TestReconcileLegalHoldCancelsPendingJob(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ws := e.ws("rchold")
	e.policy(ws, true, true, defaultPolicyJSON)
	job := e.job(ws, map[string]any{"workspace_id": ws})
	e.exec(`UPDATE jobs SET status = 'pending', worker_id = NULL, locked_at = NULL WHERE id = $1`, job.ID) // the retry state
	if err := e.worker.Reconcile(ctx, job, "boom", true); err != nil {
		t.Fatal(err)
	}
	const msg = "Legal hold enabled during retention enforcement"
	if e.scalar(`SELECT status || '/' || error FROM retention_runs WHERE workspace_id = $1`, ws) != "cancelled/"+msg {
		t.Error("run must be cancelled for legal hold")
	}
	if e.scalar(`SELECT status || '/' || error || '/' || (completed_at IS NOT NULL) FROM jobs WHERE id = $1`, job.ID) != "cancelled/"+msg+"/true" {
		t.Error("pending job must be cancelled for legal hold")
	}

	// A job that already failed for good is left as it is.
	ws2 := e.ws("rchold2")
	e.policy(ws2, true, true, defaultPolicyJSON)
	job2 := e.job(ws2, map[string]any{"workspace_id": ws2})
	e.exec(`UPDATE jobs SET status = 'failed' WHERE id = $1`, job2.ID)
	if err := e.worker.Reconcile(ctx, job2, "boom", false); err != nil {
		t.Fatal(err)
	}
	if e.scalar(`SELECT status FROM jobs WHERE id = $1`, job2.ID) != "failed" {
		t.Error("failed job must stay failed")
	}
}

func TestReconcileTruncatesLongErrorsByCharacter(t *testing.T) {
	e := newEnv(t)
	ws := e.ws("long")
	e.policy(ws, false, false, `{}`)
	job := e.job(ws, map[string]any{"workspace_id": ws})
	if err := e.worker.Reconcile(context.Background(), job, strings.Repeat("é", 1500), false); err != nil {
		t.Fatal(err)
	}
	want := "Final failure: " + strings.Repeat("é", 1000)
	if got := e.scalar(`SELECT error FROM retention_runs WHERE workspace_id = $1`, ws); got != want {
		t.Errorf("error has %d chars, want %d", len([]rune(got)), len([]rune(want)))
	}
	if e.scalar(`SELECT count(*) FROM jobs WHERE fire_key LIKE $1`, "retention:"+ws+":%") != "0" {
		t.Error("disabled policy must not reschedule")
	}
}

// '_' in a workspace id is a LIKE wildcard in Python's cancellation query;
// the port keeps that exactly.
func TestScheduleCancelsPendingJobsWithPythonLikeSemantics(t *testing.T) {
	e := newEnv(t)
	ws := e.ws("w_ld") // '_' also matches the 'X' used below
	twin := strings.Replace(ws, "_", "X", 1)
	e.spaces = append(e.spaces, twin)
	e.policy(ws, true, false, defaultPolicyJSON)
	e.run(ws, "run-"+ws, "1", defaultPolicyJSON)
	e.exec(`INSERT INTO jobs (type, payload, status, fire_key, next_run_at, priority, max_retries, retry_count)
		VALUES ('retention_enforce', '{}', 'pending', $1, LOCALTIMESTAMP, 1, 3, 0)`, "retention:"+twin+":2026-06-20")
	if err := e.worker.Handle(context.Background(), e.job(ws, map[string]any{"workspace_id": ws, "run_id": "run-" + ws})); err != nil {
		t.Fatal(err)
	}
	if e.scalar(`SELECT status FROM jobs WHERE fire_key = $1`, "retention:"+twin+":2026-06-20") != "cancelled" {
		t.Error("expected the wildcard match to cancel the look-alike workspace's job, as Python does")
	}
}

func TestScheduleDoesNotDuplicateAnActiveFireKey(t *testing.T) {
	e := newEnv(t)
	ws := e.ws("dup")
	e.policy(ws, true, false, defaultPolicyJSON)
	e.run(ws, "run-"+ws, "1", defaultPolicyJSON)
	key := "retention:" + ws + ":2026-06-16"
	e.exec(`INSERT INTO jobs (type, payload, status, fire_key, next_run_at, priority, max_retries, retry_count)
		VALUES ('retention_enforce', '{}', 'processing', $1, LOCALTIMESTAMP, 1, 3, 0)`, key)
	if err := e.worker.Handle(context.Background(), e.job(ws, map[string]any{"workspace_id": ws, "run_id": "run-" + ws})); err != nil {
		t.Fatal(err)
	}
	if e.scalar(`SELECT count(*) FROM jobs WHERE fire_key = $1`, key) != "1" {
		t.Error("an active job already holds the fire_key; none may be added")
	}
}

func TestJobTimeoutMatchesPython(t *testing.T) {
	if got := queue.JobTimeouts[JobType]; got != 1800*time.Second {
		t.Fatalf("timeout = %s, want 1800s", got)
	}
}
