// Package audiencesched ports audiences/scheduler.py: schedule_next, the
// audience_schedules mirror, remove_schedule and the queue-failure reconcile.
// The Python scheduler stays the only ticker; the rows written here are
// byte-identical to Python's so either side may continue a chain.
package audiencesched

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// JobType is the queue job type of a scheduled refresh.
const JobType = "audience_refresh"

const (
	minIntervalMinutes     = 15
	maxIntervalMinutes     = 10080
	defaultIntervalMinutes = 60
)

// ScheduleInput is the slice of an audience row schedule_next reads.
type ScheduleInput struct {
	ID             string
	WorkspaceID    string
	RefreshEnabled bool
	// IntervalMinutes is refresh_interval_minutes; 0 (Python's falsy) means 60.
	IntervalMinutes int
}

// DueRow is one due audience_schedules mirror row.
type DueRow struct {
	AudienceID    string
	WorkspaceID   string
	NextRefreshAt *time.Time // naive UTC; nil means due at the next bootstrap
}

// Interval is max(15, min(int(minutes or 60), 10080)).
func Interval(minutes int) time.Duration {
	if minutes == 0 {
		minutes = defaultIntervalMinutes
	}
	return time.Duration(max(minIntervalMinutes, min(minutes, maxIntervalMinutes))) * time.Minute
}

// FireKey is audience_refresh:<id>:<next.isoformat()> for an aware UTC instant
// (microseconds only when non-zero, then +00:00).
func FireKey(audienceID string, next time.Time) string {
	next = next.UTC()
	s := next.Format("2006-01-02T15:04:05")
	if us := next.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	return JobType + ":" + audienceID + ":" + s + "+00:00"
}

func tstz(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t.UTC(), Valid: true} }

func checkExecutor(executor string) error {
	if executor != "go" && executor != "python" {
		return fmt.Errorf("audiencesched: executor %q must be go or python", executor)
	}
	return nil
}

// requireTenant fails unless the transaction's app.workspace_id is ws.
func requireTenant(ctx context.Context, tx pgx.Tx, ws string) error {
	if ws == "" {
		return errors.New("audiencesched: a workspace is required")
	}
	var txWS string
	if err := tx.QueryRow(ctx, `SELECT coalesce(current_setting('app.workspace_id', true), '')`).Scan(&txWS); err != nil {
		return fmt.Errorf("audiencesched: read tenant: %w", err)
	}
	if txWS != ws {
		return fmt.Errorf("audiencesched: workspace %q does not match transaction tenant %q", ws, txWS)
	}
	return nil
}

func cancelPending(ctx context.Context, q *gen.Queries, audienceID string) error {
	if _, err := q.AudSchedCancelPending(ctx, JobType+":"+audienceID+":"); err != nil {
		return fmt.Errorf("audiencesched: cancel pending: %w", err)
	}
	return nil
}

// ScheduleNext is schedule_next. In the caller's transaction (inside
// tenancy.InWorkspace for a.WorkspaceID; the join rolls back with it): lock the
// audience row (so concurrent callers serialise and each cancels its
// predecessor's pending job), cancel pending audience_refresh jobs of this
// audience, then, when disabled, clear next_refresh_at and mirror a disabled
// row (nil returned); otherwise next = now + clamped interval, enqueue one job
// due then (single-flight per fire key, payload {workspace_id, audience_id,
// fire_key}, workspace_id NULL as Python), set audiences.next_refresh_at and
// upsert the mirror. now is taken as an aware UTC instant. executor is "go" or
// "python".
func ScheduleNext(ctx context.Context, tx pgx.Tx, a ScheduleInput, now time.Time, executor string) (*time.Time, error) {
	if a.ID == "" {
		return nil, errors.New("audiencesched: an audience id is required")
	}
	if err := checkExecutor(executor); err != nil {
		return nil, err
	}
	if err := requireTenant(ctx, tx, a.WorkspaceID); err != nil {
		return nil, err
	}
	q := gen.New(tx)
	if _, err := q.AudSchedLockAudience(ctx, gen.AudSchedLockAudienceParams{ID: a.ID, WorkspaceID: a.WorkspaceID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("audiencesched: audience %q not found in workspace", a.ID)
		}
		return nil, fmt.Errorf("audiencesched: lock audience: %w", err)
	}
	if err := cancelPending(ctx, q, a.ID); err != nil {
		return nil, err
	}
	if !a.RefreshEnabled {
		if err := setNext(ctx, q, a, nil); err != nil {
			return nil, err
		}
		return nil, nil
	}
	next := now.UTC().Truncate(time.Microsecond).Add(Interval(a.IntervalMinutes))
	key := FireKey(a.ID, next)
	if err := enqueue(ctx, q, a, key, next, executor); err != nil {
		return nil, err
	}
	if err := setNext(ctx, q, a, &next); err != nil {
		return nil, err
	}
	return &next, nil
}

type refreshPayload struct {
	WorkspaceID string `json:"workspace_id"`
	AudienceID  string `json:"audience_id"`
	FireKey     string `json:"fire_key"`
}

// enqueue is enqueue_job_once with next_run_at = the occurrence (queue.EnqueueOnce
// would make the job due now, a refresh early): advisory lock on the key, no second
// active row for it, uq_jobs_fire_key_active as backstop.
func enqueue(ctx context.Context, q *gen.Queries, a ScheduleInput, key string, next time.Time, executor string) error {
	if err := q.LockJobFireKey(ctx, key); err != nil {
		return fmt.Errorf("audiencesched: lock: %w", err)
	}
	if _, err := q.ActiveJobByFireKey(ctx, key); err == nil {
		return nil // an active occurrence already holds the key
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("audiencesched: look up job: %w", err)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(refreshPayload{a.WorkspaceID, a.ID, key}); err != nil {
		return fmt.Errorf("audiencesched: encode payload: %w", err)
	}
	if _, err := q.AudSchedInsertJob(ctx, gen.AudSchedInsertJobParams{
		Executor: executor, Payload: bytes.TrimSpace(buf.Bytes()), FireKey: key, NextRunAt: tstz(next),
	}); err != nil && !errors.Is(err, pgx.ErrNoRows) { // ErrNoRows: lost the unique-index race
		return fmt.Errorf("audiencesched: enqueue: %w", err)
	}
	return nil
}

// setNext is audience.next_refresh_at = next plus _mirror(db, audience, next).
func setNext(ctx context.Context, q *gen.Queries, a ScheduleInput, next *time.Time) error {
	var ts pgtype.Timestamptz
	if next != nil {
		ts = tstz(*next)
	}
	if err := q.AudSchedSetNextRefresh(ctx, gen.AudSchedSetNextRefreshParams{NextRefreshAt: ts, ID: a.ID, WorkspaceID: a.WorkspaceID}); err != nil {
		return fmt.Errorf("audiencesched: set next_refresh_at: %w", err)
	}
	if err := q.AudSchedUpsertMirror(ctx, gen.AudSchedUpsertMirrorParams{
		AudienceID: a.ID, WorkspaceID: a.WorkspaceID, NextRefreshAt: ts, Enabled: a.RefreshEnabled,
	}); err != nil {
		return fmt.Errorf("audiencesched: mirror: %w", err)
	}
	return nil
}

// RemoveSchedule is remove_schedule: delete the audience's mirror row and cancel
// its pending refresh jobs, both limited to the transaction's tenant. tx must be
// inside tenancy.InWorkspace.
func RemoveSchedule(ctx context.Context, tx pgx.Tx, audienceID string) error {
	if audienceID == "" {
		return errors.New("audiencesched: an audience id is required")
	}
	q := gen.New(tx)
	if err := q.AudSchedDeleteMirror(ctx, audienceID); err != nil {
		return fmt.Errorf("audiencesched: delete mirror: %w", err)
	}
	return cancelPending(ctx, q, audienceID)
}
