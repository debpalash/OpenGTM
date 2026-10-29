//go:build integration

package db_test

// Naive timestamp columns hold UTC wall time. Every query that writes one, or
// compares one with an aware instant, must give the same answer whatever the
// session TimeZone is. Each test runs inside a rolled-back transaction whose
// session zone is Asia/Kolkata (UTC+5:30), so a query that casts or compares
// through the session zone is off by 5.5 hours.

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

const tzWS = "tzq_ws"

// instant is the aware value handed to the queries; its UTC wall time is "2031-03-04 05:00:00".
var instant = time.Date(2031, 3, 4, 5, 0, 0, 0, time.UTC)

func tstz(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func tzTx(t *testing.T) (context.Context, pgx.Tx, *gen.Queries) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(ctx) })
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	if _, err := tx.Exec(ctx, `SET LOCAL TIME ZONE 'Asia/Kolkata'`); err != nil {
		t.Fatal(err)
	}
	return ctx, tx, gen.New(tx)
}

func tzExec(t *testing.T, ctx context.Context, tx pgx.Tx, sql string, args ...any) {
	t.Helper()
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// wantWall asserts a naive column renders as the UTC wall time of instant.
func wantWall(t *testing.T, ctx context.Context, tx pgx.Tx, sql string, want string, args ...any) {
	t.Helper()
	var got string
	if err := tx.QueryRow(ctx, sql, args...).Scan(&got); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	if got != want {
		t.Errorf("%s = %q, want %q (UTC wall time)", sql, got, want)
	}
}

const wallWant = "2031-03-04 05:00:00"

// wantNowUTC asserts a naive column was stamped with the current UTC wall time.
func wantNowUTC(t *testing.T, ctx context.Context, tx pgx.Tx, sql string, args ...any) {
	t.Helper()
	var skew float64
	q := `SELECT abs(extract(epoch FROM ((` + sql + `) - (now() AT TIME ZONE 'UTC'))))::float8`
	if err := tx.QueryRow(ctx, q, args...).Scan(&skew); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	if skew > 120 {
		t.Errorf("%s is %.0fs from the current UTC wall time; want < 120s", sql, skew)
	}
}

func seedAudience(t *testing.T, ctx context.Context, tx pgx.Tx, id string) {
	t.Helper()
	tzExec(t, ctx, tx, `INSERT INTO audiences (id, workspace_id, name, filters, updated_at) VALUES ($1, $2, 'a', '{}'::json, '2000-01-01')`, id, tzWS)
}

func TestNaiveWritesFromInstantAreUTCWhateverSessionZone(t *testing.T) {
	ctx, tx, q := tzTx(t)

	seedAudience(t, ctx, tx, "tza1")

	if err := q.AudSchedSetNextRefresh(ctx, gen.AudSchedSetNextRefreshParams{NextRefreshAt: tstz(instant), ID: "tza1", WorkspaceID: tzWS}); err != nil {
		t.Fatal(err)
	}
	wantWall(t, ctx, tx, `SELECT next_refresh_at::text FROM audiences WHERE id = 'tza1'`, wallWant)
	wantNowUTC(t, ctx, tx, `SELECT updated_at FROM audiences WHERE id = 'tza1'`)

	if err := q.AudSchedUpsertMirror(ctx, gen.AudSchedUpsertMirrorParams{AudienceID: "tza1", WorkspaceID: tzWS, NextRefreshAt: tstz(instant), Enabled: true}); err != nil {
		t.Fatal(err)
	}
	wantWall(t, ctx, tx, `SELECT next_refresh_at::text FROM audience_schedules WHERE audience_id = 'tza1'`, wallWant)
	wantNowUTC(t, ctx, tx, `SELECT updated_at FROM audience_schedules WHERE audience_id = 'tza1'`)

	if _, err := q.AudSchedInsertJob(ctx, gen.AudSchedInsertJobParams{Executor: "go", Payload: []byte(`{}`), FireKey: "tz:aud:1", NextRunAt: tstz(instant), WorkspaceID: tzWS}); err != nil {
		t.Fatal(err)
	}
	wantWall(t, ctx, tx, `SELECT next_run_at::text FROM jobs WHERE fire_key = 'tz:aud:1'`, wallWant)

	if err := q.AudRefreshMemberInsert(ctx, gen.AudRefreshMemberInsertParams{WorkspaceID: tzWS, AudienceID: "tza1", LeadID: 1, Snapshot: []byte(`{}`), RefreshToken: "r", Now: tstz(instant)}); err != nil {
		t.Fatal(err)
	}
	wantWall(t, ctx, tx, `SELECT joined_at::text FROM audience_members WHERE audience_id = 'tza1'`, wallWant)
	wantWall(t, ctx, tx, `SELECT last_seen_at::text FROM audience_members WHERE audience_id = 'tza1'`, wallWant)

	if err := q.AudRefreshFinish(ctx, gen.AudRefreshFinishParams{MemberCount: 1, RefreshedAt: tstz(instant), ID: "tza1", WorkspaceID: tzWS}); err != nil {
		t.Fatal(err)
	}
	wantWall(t, ctx, tx, `SELECT refreshed_at::text FROM audiences WHERE id = 'tza1'`, wallWant)
}

func TestRetentionNaiveWritesAreUTCWhateverSessionZone(t *testing.T) {
	ctx, tx, q := tzTx(t)

	tzExec(t, ctx, tx, `INSERT INTO retention_policies (workspace_id, retention_days) VALUES ($1, '{}'::json)`, tzWS)
	if _, err := q.RetSetPolicyNextRun(ctx, gen.RetSetPolicyNextRunParams{NextRunAt: tstz(instant), WorkspaceID: tzWS}); err != nil {
		t.Fatal(err)
	}
	wantWall(t, ctx, tx, `SELECT next_run_at::text FROM retention_policies WHERE workspace_id = '`+tzWS+`'`, wallWant)
	wantNowUTC(t, ctx, tx, `SELECT updated_at FROM retention_policies WHERE workspace_id = '`+tzWS+`'`)

	if err := q.RetUpsertMirror(ctx, gen.RetUpsertMirrorParams{WorkspaceID: tzWS, Enabled: true, NextRunAt: tstz(instant)}); err != nil {
		t.Fatal(err)
	}
	wantWall(t, ctx, tx, `SELECT next_run_at::text FROM retention_schedules WHERE workspace_id = '`+tzWS+`'`, wallWant)
	wantNowUTC(t, ctx, tx, `SELECT updated_at FROM retention_schedules WHERE workspace_id = '`+tzWS+`'`)

	if _, err := q.RetInsertScheduledJob(ctx, gen.RetInsertScheduledJobParams{Executor: "go", Payload: []byte(`{}`), WorkspaceID: tzWS, FireKey: "tz:ret:1", NextRunAt: tstz(instant)}); err != nil {
		t.Fatal(err)
	}
	wantWall(t, ctx, tx, `SELECT next_run_at::text FROM jobs WHERE fire_key = 'tz:ret:1'`, wallWant)

	tzExec(t, ctx, tx, `INSERT INTO retention_runs (id, workspace_id, status, policy_snapshot, deleted_counts) VALUES ('tzrun1', $1, 'pending', '{}'::json, '{}'::json)`, tzWS)
	if err := q.RetRunStart(ctx, gen.RetRunStartParams{StartedAt: tstz(instant), ID: "tzrun1", WorkspaceID: tzWS}); err != nil {
		t.Fatal(err)
	}
	wantWall(t, ctx, tx, `SELECT started_at::text FROM retention_runs WHERE id = 'tzrun1'`, wallWant)
	if err := q.RetRunFinish(ctx, gen.RetRunFinishParams{Status: "complete", DeletedCounts: []byte(`{}`), FinishedAt: tstz(instant), ID: "tzrun1", WorkspaceID: tzWS}); err != nil {
		t.Fatal(err)
	}
	wantWall(t, ctx, tx, `SELECT finished_at::text FROM retention_runs WHERE id = 'tzrun1'`, wallWant)
	if err := q.RetRunCancel(ctx, gen.RetRunCancelParams{Error: "x", FinishedAt: tstz(instant.Add(time.Hour)), ID: "tzrun1", WorkspaceID: tzWS}); err != nil {
		t.Fatal(err)
	}
	wantWall(t, ctx, tx, `SELECT finished_at::text FROM retention_runs WHERE id = 'tzrun1'`, "2031-03-04 06:00:00")

	var jobID int32
	if err := tx.QueryRow(ctx, `SELECT id FROM jobs WHERE fire_key = 'tz:ret:1'`).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	if err := q.RetJobCancelPending(ctx, gen.RetJobCancelPendingParams{Error: "x", CompletedAt: tstz(instant), ID: jobID}); err != nil {
		t.Fatal(err)
	}
	wantWall(t, ctx, tx, `SELECT completed_at::text FROM jobs WHERE id = $1`, wallWant, jobID)
}

func TestNaiveNowStampsAreUTCWhateverSessionZone(t *testing.T) {
	ctx, tx, q := tzTx(t)

	seedAudience(t, ctx, tx, "tza2")
	if err := q.AudCrudUpdate(ctx, gen.AudCrudUpdateParams{Name: "n", Description: "", Filters: []byte(`{}`), RefreshEnabled: true, RefreshIntervalMinutes: 60, ID: "tza2", WorkspaceID: tzWS}); err != nil {
		t.Fatal(err)
	}
	wantNowUTC(t, ctx, tx, `SELECT updated_at FROM audiences WHERE id = 'tza2'`)

	tzExec(t, ctx, tx, `UPDATE audiences SET updated_at = '2000-01-01' WHERE id = 'tza2'`)
	if err := q.AudRefreshMarkHealthy(ctx, gen.AudRefreshMarkHealthyParams{ID: "tza2", WorkspaceID: tzWS}); err != nil {
		t.Fatal(err)
	}
	wantNowUTC(t, ctx, tx, `SELECT updated_at FROM audiences WHERE id = 'tza2'`)

	tzExec(t, ctx, tx, `INSERT INTO workspace_credits (workspace_id, balance_usd, updated_at) VALUES ($1, 1, '2000-01-01')`, tzWS)
	if err := q.BillingSetBalance(ctx, gen.BillingSetBalanceParams{BalanceUsd: 2, WorkspaceID: tzWS}); err != nil {
		t.Fatal(err)
	}
	wantNowUTC(t, ctx, tx, `SELECT updated_at FROM workspace_credits WHERE workspace_id = '`+tzWS+`'`)

	tzExec(t, ctx, tx, `INSERT INTO workbooks (id, name, workspace_id, updated_at) VALUES ('tzwb1', 'w', $1, '2000-01-01')`, tzWS)
	if err := q.MarkWorkbookRunning(ctx, gen.MarkWorkbookRunningParams{WorkbookID: "tzwb1", WorkspaceID: tzWS}); err != nil {
		t.Fatal(err)
	}
	wantNowUTC(t, ctx, tx, `SELECT updated_at FROM workbooks WHERE id = 'tzwb1'`)

	tzExec(t, ctx, tx, `INSERT INTO audience_destinations (id, workspace_id, audience_id, name, destination_type, config, field_map, updated_at)
		VALUES ('tzd1', $1, 'tza2', 'd', 'webhook', '{}'::json, '{}'::json, '2000-01-01')`, tzWS)
	if err := q.DestMarkDegraded(ctx, gen.DestMarkDegradedParams{LastError: "e", WorkspaceID: tzWS, ID: "tzd1"}); err != nil {
		t.Fatal(err)
	}
	wantNowUTC(t, ctx, tx, `SELECT updated_at FROM audience_destinations WHERE id = 'tzd1'`)

	tzExec(t, ctx, tx, `INSERT INTO destination_runs (id, workspace_id, destination_id) VALUES ('tzr1', $1, 'tzd1')`, tzWS)
	tzExec(t, ctx, tx, `INSERT INTO destination_deliveries (workspace_id, run_id, destination_id, lead_id, operation, idempotency_key, payload_fingerprint, status, attempts, updated_at)
		VALUES ($1, 'tzr1', 'tzd1', 1, 'upsert', 'tz-idem', 'fp', 'pending', 0, '2000-01-01')`, tzWS)
	var id int32
	if err := tx.QueryRow(ctx, `SELECT id FROM destination_deliveries WHERE idempotency_key = 'tz-idem'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := q.DSDeliveriesMarkInFlight(ctx, gen.DSDeliveriesMarkInFlightParams{WorkspaceID: tzWS, Ids: []int32{id}}); err != nil {
		t.Fatal(err)
	}
	wantNowUTC(t, ctx, tx, fmt.Sprintf(`SELECT updated_at FROM destination_deliveries WHERE id = %d`, id))

	tzExec(t, ctx, tx, `INSERT INTO source_health (name, state, updated_at) VALUES ('tzsrc', 'healthy', '2000-01-01')`)
	if _, err := q.SHReset(ctx, "tzsrc"); err != nil {
		t.Fatal(err)
	}
	wantNowUTC(t, ctx, tx, `SELECT updated_at FROM source_health WHERE name = 'tzsrc'`)
}

func TestInboundTokenRevokeIsUTCWhateverSessionZone(t *testing.T) {
	ctx, tx, q := tzTx(t)
	seedAudience(t, ctx, tx, "tza3")
	tzExec(t, ctx, tx, `INSERT INTO audience_destinations (id, workspace_id, audience_id, name, destination_type, config, field_map)
		VALUES ('tzd3', $1, 'tza3', 'd', 'webhook', '{}'::json, '{}'::json)`, tzWS)
	tzExec(t, ctx, tx, `INSERT INTO destination_inbound_tokens (id, workspace_id, destination_id, token_hash, prefix) VALUES ('tztok1', $1, 'tzd3', 'h', 'p')`, tzWS)
	if _, err := q.DestInboundTokensRevokeActive(ctx, gen.DestInboundTokensRevokeActiveParams{Now: tstz(instant), WorkspaceID: tzWS, DestinationID: "tzd3"}); err != nil {
		t.Fatal(err)
	}
	wantWall(t, ctx, tx, `SELECT revoked_at::text FROM destination_inbound_tokens WHERE id = 'tztok1'`, wallWant)
}

// created_at is 05:00 UTC wall time. A cutoff of 04:30Z must not see it as older; a 05:30Z cutoff must.
func TestNaiveComparisonsAreUTCWhateverSessionZone(t *testing.T) {
	ctx, tx, q := tzTx(t)
	before, after := tstz(instant.Add(-30*time.Minute)), tstz(instant.Add(30*time.Minute))

	tzExec(t, ctx, tx, `INSERT INTO governance_audit_events (id, workspace_id, actor_role, method, route, resource_path, response_status, outcome, request_id, metadata_json, created_at)
		VALUES ('tzev1', $1, 'owner', 'GET', '/r', '/r', 200, 'allowed', 'rq', '{}'::json, $2)`, tzWS, "2031-03-04 05:00:00")
	n, err := q.RetCountAudit(ctx, gen.RetCountAuditParams{WorkspaceID: tzWS, Cutoff: before})
	if err != nil || n != 0 {
		t.Errorf("RetCountAudit(before) = %d, %v; want 0", n, err)
	}
	if n, err = q.RetCountAudit(ctx, gen.RetCountAuditParams{WorkspaceID: tzWS, Cutoff: after}); err != nil || n != 1 {
		t.Errorf("RetCountAudit(after) = %d, %v; want 1", n, err)
	}
	rows, err := q.ListGovernanceAuditEvents(ctx, gen.ListGovernanceAuditEventsParams{WorkspaceID: tzWS, BeforeTz: before, CursorID: pgtype.Text{}, RowLimit: 10})
	if err != nil || len(rows) != 0 {
		t.Errorf("ListGovernanceAuditEvents(before_tz) = %d rows, %v; want 0", len(rows), err)
	}
	if rows, err = q.ListGovernanceAuditEvents(ctx, gen.ListGovernanceAuditEventsParams{WorkspaceID: tzWS, SinceTz: after, RowLimit: 10}); err != nil || len(rows) != 0 {
		t.Errorf("ListGovernanceAuditEvents(since_tz after) = %d rows, %v; want 0", len(rows), err)
	}
	if rows, err = q.ListGovernanceAuditEvents(ctx, gen.ListGovernanceAuditEventsParams{WorkspaceID: tzWS, CursorTz: before, CursorID: pgtype.Text{String: "zzz", Valid: true}, RowLimit: 10}); err != nil || len(rows) != 0 {
		t.Errorf("ListGovernanceAuditEvents(cursor_tz before) = %d rows, %v; want 0", len(rows), err)
	}

	seedAudience(t, ctx, tx, "tza4")
	tzExec(t, ctx, tx, `INSERT INTO audience_destinations (id, workspace_id, audience_id, name, destination_type, config, field_map)
		VALUES ('tzd4', $1, 'tza4', 'd', 'webhook', '{}'::json, '{}'::json)`, tzWS)
	tzExec(t, ctx, tx, `INSERT INTO destination_runs (id, workspace_id, destination_id) VALUES ('tzr4', $1, 'tzd4')`, tzWS)
	tzExec(t, ctx, tx, `INSERT INTO destination_deliveries (workspace_id, run_id, destination_id, lead_id, operation, idempotency_key, payload_fingerprint, status, attempts, created_at)
		VALUES ($1, 'tzr4', 'tzd4', 1, 'upsert', 'tz-cmp', 'fp', 'pending', 0, $2)`, tzWS, "2031-03-04 05:00:00")
	if n, err = q.RetCountDeliveries(ctx, gen.RetCountDeliveriesParams{WorkspaceID: tzWS, Cutoff: before}); err != nil || n != 0 {
		t.Errorf("RetCountDeliveries(before) = %d, %v; want 0", n, err)
	}

	tzExec(t, ctx, tx, `INSERT INTO destination_inbound_receipts (id, workspace_id, destination_id, provider, external_event_id, status, conflict_policy, applied_fields, ignored_fields, payload_fingerprint, created_at)
		VALUES ('tzrc1', $1, 'tzd4', 'p', 'e', 'applied', 'x', '[]'::json, '[]'::json, 'fp', $2)`, tzWS, "2031-03-04 05:00:00")
	recs, err := q.DestTokReceiptsPage(ctx, gen.DestTokReceiptsPageParams{WorkspaceID: tzWS, DestinationID: "tzd4", HasCursor: true, CursorAt: before, CursorID: "zzz", PageLimit: 10})
	if err != nil || len(recs) != 0 {
		t.Errorf("DestTokReceiptsPage(cursor before) = %d rows, %v; want 0", len(recs), err)
	}
	if recs, err = q.DestTokReceiptsPage(ctx, gen.DestTokReceiptsPageParams{WorkspaceID: tzWS, DestinationID: "tzd4", HasCursor: true, CursorAt: after, CursorID: "zzz", PageLimit: 10}); err != nil || len(recs) != 1 {
		t.Errorf("DestTokReceiptsPage(cursor after) = %d rows, %v; want 1", len(recs), err)
	}

	tzExec(t, ctx, tx, `INSERT INTO retention_runs (id, workspace_id, status, policy_snapshot, deleted_counts, created_at) VALUES ('tzrun2', $1, 'pending', '{}'::json, '{}'::json, $2)`, tzWS, "2031-03-04 05:00:00")
	runs, err := q.RetRunsList(ctx, gen.RetRunsListParams{WorkspaceID: tzWS, UseCursor: true, CursorCreated: before, CursorID: "zzz", RowLimit: 10})
	if err != nil || len(runs) != 0 {
		t.Errorf("RetRunsList(cursor before) = %d rows, %v; want 0", len(runs), err)
	}
	if runs, err = q.RetRunsList(ctx, gen.RetRunsListParams{WorkspaceID: tzWS, UseCursor: true, CursorCreated: after, CursorID: "zzz", RowLimit: 10}); err != nil || len(runs) != 1 {
		t.Errorf("RetRunsList(cursor after) = %d rows, %v; want 1", len(runs), err)
	}
}
