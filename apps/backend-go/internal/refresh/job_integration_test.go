//go:build integration

package refresh_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/refresh"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

var fixedNow = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

type jobRow struct {
	Executor, Type, FireKey string
	Payload                 map[string]any
	Priority, MaxRetries    int
	NextRunAt               time.Time
}

func jobs(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ws string) []jobRow {
	t.Helper()
	var out []jobRow
	err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT executor, type, COALESCE(fire_key,''), payload::text, priority, max_retries, next_run_at
			FROM jobs WHERE workspace_id = $1 ORDER BY id`, ws)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var j jobRow
			var p string
			if err := rows.Scan(&j.Executor, &j.Type, &j.FireKey, &p, &j.Priority, &j.MaxRetries, &j.NextRunAt); err != nil {
				return err
			}
			if err := json.Unmarshal([]byte(p), &j.Payload); err != nil {
				return err
			}
			out = append(out, j)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func ofType(js []jobRow, typ string) []jobRow {
	var out []jobRow
	for _, j := range js {
		if j.Type == typ {
			out = append(out, j)
		}
	}
	return out
}

func activityCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ws string) (n int, msg string) {
	t.Helper()
	_ = tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		_ = tx.QueryRow(ctx, `SELECT count(*), COALESCE(max(message),'') FROM workbook_activity WHERE kind='refresh'`).Scan(&n, &msg)
		return nil
	})
	return
}

func deps(pool *pgxpool.Pool) refresh.Deps {
	return refresh.Deps{Pool: pool, Now: func() time.Time { return fixedNow }}
}

func claim(id int64, ws string, payload map[string]any) *queue.Claim {
	b, _ := json.Marshal(payload)
	return &queue.Claim{ID: id, Type: "refresh_workbook", WorkspaceID: ws, Payload: b}
}

const refreshCols = `[{"id":"src","type":"source"},{"id":"email","type":"enrichment"},{"id":"note","type":"formula","formula":"1","input_columns":["email"]}]`

func seedStaleWorkbook(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ws, wb, cols, policy string) {
	t.Helper()
	err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		mkWorkbook(t, ctx, tx, ws, wb, cols, policy)
		mkRow(t, ctx, tx, ws, wb, 1, nil)
		mkRow(t, ctx, tx, ws, wb, 2, nil)
		complete := "complete"
		fresh := fixedNow.Add(-time.Hour)
		mkOverlay(t, ctx, tx, ws, wb, 1, "email", &complete, &fresh) // fresh, not stale
		return nil                                                   // row 2 has no overlay -> stale
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSetPolicyShapeAndNextEnqueue(t *testing.T) {
	pool, ctx := setupPool(t)
	err := tenancy.InWorkspace(ctx, pool, wsA, func(tx pgx.Tx) error {
		mkWorkbook(t, ctx, tx, wsA, "w1", `[]`, `null`)
		res, err := refresh.SetPolicy(ctx, tx, "w1", map[string]any{"enabled": true, "interval": "hourly"})
		if err != nil {
			return err
		}
		if res["next_in_minutes"] != 60 || res["refresh_policy"] == nil || len(res) != 2 {
			t.Errorf("shape %v", res)
		}
		var stored string
		_ = tx.QueryRow(ctx, `SELECT refresh_policy::text FROM workbooks WHERE id='w1'`).Scan(&stored)
		var sp map[string]any
		_ = json.Unmarshal([]byte(stored), &sp)
		if sp["interval"] != "hourly" || sp["enabled"] != true {
			t.Errorf("stored %s", stored)
		}
		// saving again must not fork a second chain
		if _, err := refresh.SetPolicy(ctx, tx, "w1", map[string]any{"enabled": true, "interval": "weekly"}); err != nil {
			return err
		}
		// disabled: next_in_minutes nil
		res, _ = refresh.SetPolicy(ctx, tx, "w1", map[string]any{"enabled": false, "interval": "daily"})
		if v, ok := res["next_in_minutes"]; !ok || v != nil {
			t.Errorf("disabled: %v", res)
		}
		// unknown workbook
		res, _ = refresh.SetPolicy(ctx, tx, "nope", map[string]any{"enabled": true, "interval": "daily"})
		if res["error"] != "workbook_not_found" || len(res) != 1 {
			t.Errorf("unknown: %v", res)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	js := ofType(jobs(t, ctx, pool, wsA), "refresh_workbook")
	if len(js) != 1 {
		t.Fatalf("want one chain, got %d", len(js))
	}
	j := js[0]
	if j.Payload["workbook_id"] != "w1" || j.Payload["workspace_id"] != wsA || j.Priority != 1 || j.MaxRetries != 3 ||
		!j.NextRunAt.Equal(fixedNow.Add(time.Hour)) {
		t.Errorf("job %+v", j)
	}
}

func TestRefreshEnqueuesRunForStaleOnly(t *testing.T) {
	pool, ctx := setupPool(t)
	seedStaleWorkbook(t, ctx, pool, wsA, "w1", refreshCols, `{"enabled":true,"interval":"daily"}`)
	if err := refresh.Handle(ctx, deps(pool), claim(501, wsA, map[string]any{"workbook_id": "w1", "workspace_id": wsA, "reason": "manual"})); err != nil {
		t.Fatal(err)
	}
	js := jobs(t, ctx, pool, wsA)
	src := ofType(js, "source_workbook")
	if len(src) != 1 || src[0].Executor != "python" || src[0].Payload["column_id"] != "src" || src[0].Payload["workbook_id"] != "w1" {
		t.Errorf("source job: %+v", src)
	}
	run := ofType(js, "run_workbook")
	if len(run) != 1 || run[0].Executor != "python" {
		t.Fatalf("run job: %+v", run)
	}
	p := run[0].Payload
	rc, _ := p["row_columns"].(map[string]any)
	// row 2 only; stale email plus its reactive downstream formula
	if len(rc) != 1 || !jsonEq(rc["2"], []any{"email", "note"}) {
		t.Errorf("row_columns %v", rc)
	}
	if !jsonEq(p["row_ids"], []any{float64(2)}) || !jsonEq(p["column_ids"], []any{"email", "note"}) || p["workspace_id"] != wsA {
		t.Errorf("payload %v", p)
	}
	next := ofType(js, "refresh_workbook")
	if len(next) != 1 || next[0].Executor != "go" || next[0].FireKey == "" || !next[0].NextRunAt.Equal(fixedNow.Add(24*time.Hour)) ||
		next[0].Payload["workbook_id"] != "w1" || next[0].Payload["workspace_id"] != wsA {
		t.Errorf("next: %+v", next)
	}
	if n, msg := activityCount(t, ctx, pool, wsA); n != 1 || msg == "" {
		t.Errorf("activity %d %q", n, msg)
	}

	// All cells fresh and no source column: no run job at all.
	_ = tenancy.InWorkspace(ctx, pool, wsA, func(tx pgx.Tx) error {
		_, _ = tx.Exec(ctx, `DELETE FROM workbooks`)
		_, _ = tx.Exec(ctx, `DELETE FROM jobs WHERE workspace_id=$1`, wsA)
		return nil
	})
	err := tenancy.InWorkspace(ctx, pool, wsA, func(tx pgx.Tx) error {
		mkWorkbook(t, ctx, tx, wsA, "w2", `[{"id":"email","type":"enrichment"}]`, `{}`)
		mkRow(t, ctx, tx, wsA, "w2", 1, nil)
		c, f := "complete", fixedNow.Add(-time.Hour)
		mkOverlay(t, ctx, tx, wsA, "w2", 1, "email", &c, &f)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := refresh.Handle(ctx, deps(pool), claim(502, wsA, map[string]any{"workbook_id": "w2", "workspace_id": wsA})); err != nil {
		t.Fatal(err)
	}
	js = jobs(t, ctx, pool, wsA)
	if len(ofType(js, "run_workbook")) != 0 || len(ofType(js, "source_workbook")) != 0 || len(ofType(js, "refresh_workbook")) != 0 {
		t.Errorf("fresh workbook enqueued work: %+v", js)
	}
}

func jsonEq(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func TestRefreshNextIsOncePerFireKey(t *testing.T) {
	pool, ctx := setupPool(t)
	seedStaleWorkbook(t, ctx, pool, wsA, "w1", refreshCols, `{"enabled":true,"interval":"hourly"}`)
	c := claim(600, wsA, map[string]any{"workbook_id": "w1", "workspace_id": wsA})
	for i := 0; i < 2; i++ { // a retried job handles the same claim again
		if err := refresh.Handle(ctx, deps(pool), c); err != nil {
			t.Fatal(err)
		}
	}
	js := jobs(t, ctx, pool, wsA)
	if n := len(ofType(js, "refresh_workbook")); n != 1 {
		t.Errorf("next chain forked: %d", n)
	}
	if n := len(ofType(js, "run_workbook")); n != 1 {
		t.Errorf("run enqueued %d times", n)
	}
	if n := len(ofType(js, "source_workbook")); n != 1 {
		t.Errorf("source enqueued %d times", n)
	}
}

func TestRefreshDisabledPolicyDoesNotChain(t *testing.T) {
	pool, ctx := setupPool(t)
	seedStaleWorkbook(t, ctx, pool, wsA, "w1", refreshCols, `{"enabled":false,"interval":"daily"}`)
	if err := refresh.Handle(ctx, deps(pool), claim(700, wsA, map[string]any{"workbook_id": "w1", "workspace_id": wsA})); err != nil {
		t.Fatal(err)
	}
	if n := len(ofType(jobs(t, ctx, pool, wsA), "refresh_workbook")); n != 0 {
		t.Errorf("disabled policy chained %d", n)
	}
}

func TestRefreshTenantIsolation(t *testing.T) {
	pool, ctx := setupPool(t)
	seedStaleWorkbook(t, ctx, pool, wsA, "w1", refreshCols, `{"enabled":true,"interval":"daily"}`)
	// ws B asks to refresh ws A's workbook: invisible, completes quietly, writes nothing.
	if err := refresh.Handle(ctx, deps(pool), claim(800, wsB, map[string]any{"workbook_id": "w1", "workspace_id": wsB})); err != nil {
		t.Fatal(err)
	}
	if n := len(jobs(t, ctx, pool, wsB)) + len(jobs(t, ctx, pool, wsA)); n != 0 {
		t.Errorf("cross-tenant refresh enqueued %d jobs", n)
	}
	if n, _ := activityCount(t, ctx, pool, wsA); n != 0 {
		t.Errorf("activity leaked into A: %d", n)
	}
	// SetPolicy and stale selection are RLS-blind to the other tenant too.
	_ = tenancy.InWorkspace(ctx, pool, wsB, func(tx pgx.Tx) error {
		res, _ := refresh.SetPolicy(ctx, tx, "w1", map[string]any{"enabled": true, "interval": "daily"})
		if res["error"] != "workbook_not_found" {
			t.Errorf("SetPolicy leaked: %v", res)
		}
		got, err := refresh.StaleRowColumns(ctx, tx, "w1", []refresh.Col{{ID: "email"}}, nil, fixedNow)
		if err != nil || len(got) != 0 {
			t.Errorf("stale leaked: %v %v", got, err)
		}
		return nil
	})
	// A payload workspace that disagrees with the claim is refused.
	if err := refresh.Handle(ctx, deps(pool), claim(801, wsB, map[string]any{"workbook_id": "w1", "workspace_id": wsA})); err == nil {
		t.Error("mismatched payload workspace must fail")
	}
}

func TestRefreshMissingWorkbookCompletesQuietly(t *testing.T) {
	pool, ctx := setupPool(t)
	if err := refresh.Handle(ctx, deps(pool), claim(900, wsA, map[string]any{"workbook_id": "gone", "workspace_id": wsA})); err != nil {
		t.Fatalf("missing workbook must complete: %v", err)
	}
	if n := len(jobs(t, ctx, pool, wsA)); n != 0 {
		t.Errorf("jobs %d", n)
	}
	if n, _ := activityCount(t, ctx, pool, wsA); n != 0 {
		t.Errorf("activity %d", n)
	}
	// Python fails loud when the payload carries no workspace.
	if err := refresh.Handle(ctx, deps(pool), claim(901, "", map[string]any{"workbook_id": "w1"})); err == nil {
		t.Error("missing workspace_id must error")
	}
}

func TestRefreshSkipsChainWhenAnotherActiveRefreshExists(t *testing.T) {
	pool, ctx := setupPool(t)
	seedStaleWorkbook(t, ctx, pool, wsA, "w1", refreshCols, `{"enabled":true,"interval":"daily"}`)
	// A signal-triggered refresh (no fire key) is already pending for the workbook.
	err := tenancy.InWorkspace(ctx, pool, wsA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO jobs (executor, type, payload, workspace_id, priority, status, next_run_at, max_retries, retry_count)
			VALUES ('python','refresh_workbook', '{"workbook_id":"w1","workspace_id":"ws-p06t16-a","reason":"signal"}'::json, $1, 2, 'pending', now() at time zone 'utc', 3, 0)`, wsA)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := refresh.Handle(ctx, deps(pool), claim(1000, wsA, map[string]any{"workbook_id": "w1", "workspace_id": wsA})); err != nil {
		t.Fatal(err)
	}
	if n := len(ofType(jobs(t, ctx, pool, wsA), "refresh_workbook")); n != 1 {
		t.Errorf("a second chain was started: %d refresh jobs", n)
	}
}
