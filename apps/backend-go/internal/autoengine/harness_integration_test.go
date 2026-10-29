//go:build integration

package autoengine

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autoactions"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

const (
	ws1 = "ws-1"
	ws2 = "ws-2"
)

const engCols = `[
 {"id":"company","name":"Company","type":"lead_field","lead_field":"company"},
 {"id":"email","name":"Email","type":"lead_field","lead_field":"email"},
 {"id":"score","name":"Score","type":"lead_field","lead_field":"score"},
 {"id":"plat","name":"Platform","type":"waterfall","target_field":"description","waterfall":["research","hunter_io"]},
 {"id":"free","name":"Free","type":"enrichment","provider":"hunter_io","target_field":"email","verify":false}]`

const (
	actWH   = `{"type":"webhook","config":{"url":"https://hooks.example.com/x"}}`
	actPaid = `{"type":"re_enrich","config":{"column_ids":["plat"]}}`
	actFree = `{"type":"re_enrich","config":{"column_ids":["free"]}}`
	actCRM  = `{"type":"push_crm","config":{"type":"hubspot"}}`
)

type env struct {
	t    *testing.T
	pool *pgxpool.Pool
	ctx  context.Context
	d    Deps
	// rowIDs by ordinal, in seeding order.
	rowIDs []int32
}

func init() { queue.AllowJobType(JobType) }

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := testpg.NewAppPool(t, 24)
	e := &env{t: t, pool: pool, ctx: context.Background()}
	e.d = Deps{Pool: pool, Enabled: true, BillingEnabled: true, MaxRowsPerEval: 500, CheckEvery: -1,
		Now:     func() time.Time { return time.Date(2026, 8, 29, 10, 30, 0, 0, time.UTC) },
		ExecFor: func(string) string { return "go" }}
	e.clean()
	t.Cleanup(e.clean)
	for _, ws := range []string{ws1, ws2} {
		if _, err := pool.Exec(e.ctx, `INSERT INTO workspaces (id, name, slug) VALUES ($1::varchar,$1::text,$1::text) ON CONFLICT DO NOTHING`, ws); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func (e *env) clean() {
	_, _ = e.pool.Exec(e.ctx, `DELETE FROM jobs`)
	for _, ws := range []string{ws1, ws2} {
		_ = tenancy.InWorkspace(e.ctx, e.pool, ws, func(tx pgx.Tx) error {
			for _, q := range []string{`DELETE FROM trigger_action_results`, `DELETE FROM trigger_runs`,
				`DELETE FROM trigger_cap_reservations`, `DELETE FROM credit_ledger_entries`, `DELETE FROM workspace_credits`,
				`DELETE FROM workbook_activity`, `DELETE FROM workbook_rows`, `DELETE FROM workbooks`,
				`DELETE FROM scheduled_triggers`, `DELETE FROM triggers`} {
				_, _ = tx.Exec(e.ctx, q)
			}
			return nil
		})
	}
}

func (e *env) inWS(ws string, fn func(tx pgx.Tx) error) {
	e.t.Helper()
	if err := tenancy.InWorkspace(e.ctx, e.pool, ws, fn); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) workbook(ws, id, cols string) {
	e.t.Helper()
	e.inWS(ws, func(tx pgx.Tx) error {
		_, err := tx.Exec(e.ctx, `INSERT INTO workbooks (id, name, workspace_id, columns_config) VALUES ($1,$1,$2,$3::json)`, id, ws, cols)
		return err
	})
}

// row seeds one row and returns its ordinal (the order of seeding).
func (e *env) row(ws, wb, data, enr string, lead *int) int {
	e.t.Helper()
	if enr == "" {
		enr = "{}"
	}
	var id int32
	e.inWS(ws, func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, `INSERT INTO workbook_rows (workbook_id, workspace_id, position, data, enrichments, lead_id)
			VALUES ($1,$2,$3,$4::json,$5::json,$6) RETURNING id`, wb, ws, len(e.rowIDs), data, enr, lead).Scan(&id)
	})
	e.rowIDs = append(e.rowIDs, id)
	return len(e.rowIDs) - 1
}

type trig struct {
	Name, Type, Condition string
	Enabled               *bool
	Actions               string
	Scope                 string
	StopOnError           bool
	MaxSpend              *float64
	MaxActions            *int
	Anchor                *time.Time
	TriggerConfig         string
}

func (e *env) trigger(ws, id string, tr trig) {
	e.t.Helper()
	if tr.Name == "" {
		tr.Name = "rule"
	}
	if tr.Type == "" {
		tr.Type = "on_row_added"
	}
	if tr.Actions == "" {
		tr.Actions = "[" + actWH + "]"
	}
	if tr.Scope == "" {
		tr.Scope = "[]"
	}
	if tr.TriggerConfig == "" {
		tr.TriggerConfig = "{}"
	}
	enabled := true
	if tr.Enabled != nil {
		enabled = *tr.Enabled
	}
	e.inWS(ws, func(tx pgx.Tx) error {
		_, err := tx.Exec(e.ctx, `INSERT INTO triggers (id, workspace_id, name, enabled, trigger_type, trigger_config, "condition",
			actions, scope_workbook_ids, stop_on_error, max_spend_usd_per_day, max_actions_per_day, schedule_anchor)
			VALUES ($1,$2,$3,$4,$5,$6::json,$7,$8::json,$9::json,$10,$11,$12,$13)`,
			id, ws, tr.Name, enabled, tr.Type, tr.TriggerConfig, tr.Condition, tr.Actions, tr.Scope, tr.StopOnError,
			tr.MaxSpend, tr.MaxActions, tr.Anchor)
		return err
	})
}

func (e *env) credits(ws string, usd float64) {
	e.t.Helper()
	e.inWS(ws, func(tx pgx.Tx) error {
		_, err := tx.Exec(e.ctx, `INSERT INTO workspace_credits (workspace_id, balance_usd) VALUES ($1,$2)
			ON CONFLICT (workspace_id) DO UPDATE SET balance_usd = EXCLUDED.balance_usd`, ws, usd)
		return err
	})
}

func (e *env) balance(ws string) float64 {
	var b float64
	e.inWS(ws, func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, `SELECT COALESCE((SELECT balance_usd FROM workspace_credits WHERE workspace_id=$1),0)`, ws).Scan(&b)
	})
	return b
}

// defaultSeed is the vectors' ENG data: workbooks wbA/wbB (ws-1) and wbX
// (ws-2), three rows in wbA and rule trg-1 with the given overrides.
func (e *env) defaultSeed(tr trig) {
	e.workbook(ws1, "wbA", engCols)
	e.workbook(ws1, "wbB", engCols)
	e.workbook(ws2, "wbX", engCols)
	seven := 7
	e.row(ws1, "wbA", `{"company":"Acme","email":"a@acme.com","score":"90"}`, "", nil)
	e.row(ws1, "wbA", `{"company":"Beta","email":"","score":"20"}`, "", nil)
	e.row(ws1, "wbA", `{"company":"Gamma","email":"g@gamma.com","score":"70"}`, `{"free":{"value":"g2@gamma.com","status":"complete"}}`, &seven)
	e.trigger(ws1, "trg-1", tr)
	e.credits(ws1, 10)
}

// claim enqueues a trigger_eval job for ws1 with the payload and claims it.
func (e *env) claim(payload map[string]any) *queue.Claim {
	e.t.Helper()
	if _, ok := payload["workspace_id"]; !ok {
		payload["workspace_id"] = ws1
	}
	if _, ok := payload["trigger_id"]; !ok {
		payload["trigger_id"] = "trg-1"
	}
	return e.claimRaw(payload)
}

// claimRaw enqueues the payload exactly as given.
func (e *env) claimRaw(payload map[string]any) *queue.Claim {
	e.t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		e.t.Fatal(err)
	}
	var id int64
	if err := e.pool.QueryRow(e.ctx, `INSERT INTO jobs (type, payload, status, priority, created_at, retry_count, max_retries, workspace_id, executor)
		VALUES ('trigger_eval', $1::json, 'pending', 0, now() AT TIME ZONE 'UTC', 0, 3, $2, 'go') RETURNING id`, string(raw), ws1).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	c, err := queue.ClaimNext(e.ctx, e.pool, "w-test", 0)
	if err != nil || c == nil || c.ID != id {
		e.t.Fatalf("claim: %v %v", c, err)
	}
	return c
}

func (e *env) run(payload map[string]any) error {
	return Handle(e.ctx, e.d, e.claim(payload))
}

func target(wb string, row int) map[string]any {
	return map[string]any{"workbook_id": wb, "row_id": strconv.Itoa(row)}
}

func (e *env) targetOf(wb string, ord int) map[string]any {
	return map[string]any{"workbook_id": wb, "row_id": strconv.Itoa(int(e.rowIDs[ord]))}
}

type resultRow struct {
	Row                   string
	WB, Type, Status, Key string
	Idx                   int
	Skip, Summary, Error  *string
	Charged               float64
	Lead                  *int
	RunID                 string
}

func (e *env) results(ws string) []resultRow {
	var out []resultRow
	e.inWS(ws, func(tx pgx.Tx) error {
		rows, err := tx.Query(e.ctx, `SELECT row_id, workbook_id, action_type, status, idempotency_key, action_index, skip_reason,
			result_summary, error, COALESCE(charged_usd,0), lead_id, run_id FROM trigger_action_results ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r resultRow
			if err := rows.Scan(&r.Row, &r.WB, &r.Type, &r.Status, &r.Key, &r.Idx, &r.Skip, &r.Summary, &r.Error, &r.Charged, &r.Lead, &r.RunID); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out
}

type runRow struct {
	ID, Status, FireSource, FireKey         string
	Name                                    *string
	Matched, Attempted, OK, Skipped, Failed int
	Charged                                 float64
	Finished                                bool
}

func (e *env) runs(ws string) []runRow {
	var out []runRow
	e.inWS(ws, func(tx pgx.Tx) error {
		rows, err := tx.Query(e.ctx, `SELECT id, status, fire_source, fire_key, trigger_name_snapshot, matched_rows, actions_attempted,
			actions_succeeded, actions_skipped, actions_failed, total_charged_usd, finished_at IS NOT NULL FROM trigger_runs ORDER BY started_at, id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r runRow
			if err := rows.Scan(&r.ID, &r.Status, &r.FireSource, &r.FireKey, &r.Name, &r.Matched, &r.Attempted, &r.OK, &r.Skipped, &r.Failed, &r.Charged, &r.Finished); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out
}

func (e *env) count(ws, table string) int {
	var n int
	e.inWS(ws, func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, "SELECT count(*) FROM "+table).Scan(&n)
	})
	return n
}

func okResult() autoactions.Result { return autoactions.Result{Status: "success", Summary: "ok"} }

// recorder is an Execute override that records its calls.
type recorder struct {
	calls []string // idem keys
	fn    func(idem string, action map[string]any) autoactions.Result
}

func (r *recorder) exec(ctx context.Context, ws string, action map[string]any, wb, rowID string, leadID *int64,
	lead map[string]any, cols []autoactions.Column, idem string) autoactions.Result {
	r.calls = append(r.calls, idem)
	if r.fn != nil {
		return r.fn(idem, action)
	}
	return okResult()
}

func fmtKey(ws, trg, wb string, row int32, idx int, fk string) string {
	return fmt.Sprintf("trig:%s:%s:%s:%d:%d:%s", ws, trg, wb, row, idx, fk)
}
