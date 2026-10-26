//go:build integration

package automationhook_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/automationhook"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

const ws = "ws-ah"

type job struct {
	Type, Executor, Status, WorkspaceID string
	Priority                            int
	FireKey                             *string
	Payload                             string
}

func setup(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	ctx := context.Background()
	clean := func() {
		for _, w := range []string{ws, "ws-ah-other"} {
			_ = tenancy.InWorkspace(ctx, pool, w, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `DELETE FROM triggers`)
				return err
			})
		}
		_, _ = pool.Exec(ctx, `DELETE FROM jobs WHERE type='trigger_eval'`)
	}
	clean()
	t.Cleanup(clean)
	return pool, ctx
}

func addTrigger(t *testing.T, pool *pgxpool.Pool, ctx context.Context, w, id, typ string, enabled bool, scope string) {
	t.Helper()
	if err := tenancy.InWorkspace(ctx, pool, w, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO triggers (id, workspace_id, name, enabled, trigger_type, trigger_config, actions, scope_workbook_ids)
			VALUES ($1,$2,'n',$3,$4,'{}'::json,'[]'::json,$5::json)`, id, w, enabled, typ, scope)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func jobs(t *testing.T, pool *pgxpool.Pool, ctx context.Context) []job {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT type, executor, status, coalesce(workspace_id,''), priority, fire_key, payload::text FROM jobs WHERE type='trigger_eval' ORDER BY payload->>'trigger_id'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.Type, &j.Executor, &j.Status, &j.WorkspaceID, &j.Priority, &j.FireKey, &j.Payload); err != nil {
			t.Fatal(err)
		}
		out = append(out, j)
	}
	return out
}

func fire(t *testing.T, pool *pgxpool.Pool, ctx context.Context, h automationhook.Hook, w, wb string, ids []int64) int {
	t.Helper()
	var n int
	if err := tenancy.InWorkspace(ctx, pool, w, func(tx pgx.Tx) error {
		var err error
		n, err = h.RowsAdded(ctx, tx, w, wb, ids)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDisabledEnqueuesNothing(t *testing.T) {
	pool, ctx := setup(t)
	addTrigger(t, pool, ctx, ws, "t1", "on_row_added", true, "[]")
	if n := fire(t, pool, ctx, automationhook.New(false, slog.Default()), ws, "wb-1", []int64{1}); n != 0 {
		t.Fatalf("n=%d", n)
	}
	if got := jobs(t, pool, ctx); len(got) != 0 {
		t.Fatalf("jobs=%v", got)
	}
}

func TestEnqueuesOneJobPerCoveringRule(t *testing.T) {
	pool, ctx := setup(t)
	addTrigger(t, pool, ctx, ws, "t1", "on_row_added", true, "[]")
	addTrigger(t, pool, ctx, ws, "t2", "on_row_added", true, `["wb-1"]`)
	addTrigger(t, pool, ctx, ws, "t3", "on_row_added", false, "[]")  // disabled
	addTrigger(t, pool, ctx, ws, "t4", "on_row_changed", true, "[]") // other type
	addTrigger(t, pool, ctx, ws, "t5", "on_row_added", true, "null") // null scope = all
	if n := fire(t, pool, ctx, automationhook.New(true, slog.Default()), ws, "wb-1", []int64{1, 2}); n != 3 {
		t.Fatalf("n=%d", n)
	}
	got := jobs(t, pool, ctx)
	if len(got) != 3 {
		t.Fatalf("jobs=%d", len(got))
	}
	for _, j := range got {
		if j.Executor != "python" || j.Status != "pending" || j.Priority != 1 || j.WorkspaceID != ws || j.FireKey != nil {
			t.Fatalf("job row %+v", j)
		}
	}
}

func TestScopedRuleSkipsOtherWorkbook(t *testing.T) {
	pool, ctx := setup(t)
	addTrigger(t, pool, ctx, ws, "t2", "on_row_added", true, `["wb-1"]`)
	if n := fire(t, pool, ctx, automationhook.New(true, slog.Default()), ws, "wb-2", []int64{1}); n != 0 {
		t.Fatalf("n=%d", n)
	}
	if got := jobs(t, pool, ctx); len(got) != 0 {
		t.Fatalf("jobs=%v", got)
	}
}

func TestFireKeyTruncatedAt120(t *testing.T) {
	pool, ctx := setup(t)
	addTrigger(t, pool, ctx, ws, "t1", "on_row_added", true, "[]")
	var ids []int64
	for i := int64(100000); i < 100050; i++ {
		ids = append(ids, i)
	}
	fire(t, pool, ctx, automationhook.New(true, slog.Default()), ws, "wb-1", ids)
	var p struct {
		FireKey string `json:"fire_key"`
	}
	if err := json.Unmarshal([]byte(jobs(t, pool, ctx)[0].Payload), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.FireKey) != len("rowadd:wb-1:")+120 {
		t.Fatalf("len=%d", len(p.FireKey))
	}
}

// Expected jobs recorded from the Python emitter (testdata/python_jobs.json);
// jobs[0] is trg-1 (all workbooks) for rows [10,9,100]: ids sort as strings.
func TestPayloadMatchesPythonJob(t *testing.T) {
	raw, err := os.ReadFile("testdata/python_jobs.json")
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Jobs []struct {
			Payload json.RawMessage `json:"payload"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	pool, ctx := setup(t)
	addTrigger(t, pool, ctx, ws, "trg-1", "on_row_added", true, "[]")
	fire(t, pool, ctx, automationhook.New(true, slog.Default()), ws, "wb-1", []int64{10, 9, 100})
	// Python recorded workspace "ws-1"; compare with workspace swapped, and
	// byte-for-byte key order via re-encoding both through ordered decoding.
	got := jobs(t, pool, ctx)[0].Payload
	want := strings.Replace(compact(t, rec.Jobs[0].Payload), `"ws-1"`, `"`+ws+`"`, 1)
	if compact(t, json.RawMessage(got)) != want {
		t.Fatalf("payload\n got %s\nwant %s", compact(t, json.RawMessage(got)), want)
	}
}

func compact(t *testing.T, b json.RawMessage) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestErrorIsSwallowedAndLogged(t *testing.T) {
	pool, ctx := setup(t)
	var logs bytes.Buffer
	h := automationhook.New(true, slog.New(slog.NewTextHandler(&logs, nil)))
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback(ctx) // closed tx -> every query errors
	n, err := h.RowsAdded(ctx, tx, ws, "wb-1", []int64{1})
	if err != nil || n != 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if !strings.Contains(logs.String(), "automation row-added hook failed") {
		t.Fatalf("log=%q", logs.String())
	}
}

// A server-side failure inside the hook (a test-only trigger makes every
// trigger_eval insert raise) must not abort the caller's transaction: the
// caller's own row still commits and no job is left behind.
func TestFailureKeepsCallerTxUsable(t *testing.T) {
	pool, ctx := setup(t)
	owner, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	for _, stmt := range []string{
		`CREATE OR REPLACE FUNCTION t5_fail_job() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 't5 forced failure'; END $$`,
		`CREATE TRIGGER t5_fail_job BEFORE INSERT ON jobs FOR EACH ROW WHEN (NEW.type = 'trigger_eval') EXECUTE FUNCTION t5_fail_job()`,
	} {
		if _, err := owner.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = owner.Exec(context.Background(), `DROP TRIGGER IF EXISTS t5_fail_job ON jobs`)
		_, _ = owner.Exec(context.Background(), `DROP FUNCTION IF EXISTS t5_fail_job()`)
		_ = tenancy.InWorkspace(context.Background(), pool, ws, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(), `DELETE FROM workbooks WHERE id='wb-t5'`)
			return err
		})
	})
	addTrigger(t, pool, ctx, ws, "t1", "on_row_added", true, "[]")
	addTrigger(t, pool, ctx, ws, "t2", "on_row_added", true, "[]")

	var logs bytes.Buffer
	h := automationhook.New(true, slog.New(slog.NewTextHandler(&logs, nil)))
	err = tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO workbooks (id, name, workspace_id) VALUES ('wb-t5','wb',$1)`, ws); err != nil {
			return err
		}
		n, err := h.RowsAdded(ctx, tx, ws, "wb-t5", []int64{1})
		if err != nil || n != 0 {
			t.Fatalf("n=%d err=%v", n, err)
		}
		var one int
		return tx.QueryRow(ctx, `SELECT 1`).Scan(&one) // fails if tx is aborted
	}) // commit happens inside InWorkspace
	if err != nil {
		t.Fatalf("caller tx poisoned: %v", err)
	}
	if !strings.Contains(logs.String(), "t5 forced failure") {
		t.Fatalf("log=%q", logs.String())
	}
	var cnt int
	if err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM workbooks WHERE id='wb-t5'`).Scan(&cnt)
	}); err != nil || cnt != 1 {
		t.Fatalf("caller row not committed: cnt=%d err=%v", cnt, err)
	}
	if got := jobs(t, pool, ctx); len(got) != 0 {
		t.Fatalf("jobs left: %v", got)
	}
}

func TestRulesTenantScoped(t *testing.T) {
	pool, ctx := setup(t)
	addTrigger(t, pool, ctx, "ws-ah-other", "tx", "on_row_added", true, "[]")
	addTrigger(t, pool, ctx, ws, "t1", "on_row_added", true, "[]")
	if n := fire(t, pool, ctx, automationhook.New(true, slog.Default()), ws, "wb-1", []int64{1}); n != 1 {
		t.Fatalf("n=%d", n)
	}
	got := jobs(t, pool, ctx)
	if len(got) != 1 || !strings.Contains(got[0].Payload, `"t1"`) {
		t.Fatalf("jobs=%v", got)
	}
}
