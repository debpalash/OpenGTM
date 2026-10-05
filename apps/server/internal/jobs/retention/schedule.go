package retention

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
)

// schedulePolicy ports schedule_policy for the policy of workspaceID, inside
// the caller's tenant transaction:
//
//   - pending retention_enforce jobs whose fire_key matches
//     'retention:<workspace>:%' are cancelled (status only, as Python does;
//     the LIKE pattern is deliberately not escaped, so '_' and '%' in a
//     workspace id behave exactly as they do in Python);
//   - the non-RLS retention_schedules mirror and policy.next_run_at are set
//     to now+1 day, or cleared when the policy is disabled or on legal hold;
//   - the next occurrence is enqueued once under fire_key
//     'retention:<workspace>:<YYYY-MM-DD of the next run, UTC>'.
//
// It returns the next run time, nil when none is scheduled. A workspace
// without a policy row has nothing to schedule.
func (w *Worker) schedulePolicy(ctx context.Context, tx pgx.Tx, workspaceID string, now time.Time) (*time.Time, error) {
	policy, err := loadPolicy(ctx, tx, workspaceID)
	if err != nil {
		return nil, err
	}
	if policy == nil {
		return nil, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE jobs SET status = 'cancelled'
		WHERE type = $1 AND status = 'pending' AND fire_key LIKE $2`,
		JobType, "retention:"+workspaceID+":%"); err != nil {
		return nil, fmt.Errorf("cancel pending retention jobs: %w", err)
	}
	var next *time.Time
	if policy.enabled && !policy.legalHold {
		n := now.UTC().Add(24 * time.Hour)
		next = &n
	}
	// Written only when something changed, like the ORM's dirty tracking, so
	// updated_at (onupdate=now()) is not bumped by a no-op reschedule.
	if _, err := tx.Exec(ctx, `
INSERT INTO retention_schedules (workspace_id, enabled, next_run_at)
VALUES ($1, $2, $3::timestamptz::timestamp)
ON CONFLICT (workspace_id) DO UPDATE
   SET enabled = EXCLUDED.enabled, next_run_at = EXCLUDED.next_run_at, updated_at = now()
 WHERE retention_schedules.enabled IS DISTINCT FROM EXCLUDED.enabled
    OR retention_schedules.next_run_at IS DISTINCT FROM EXCLUDED.next_run_at`,
		workspaceID, next != nil, next); err != nil {
		return nil, fmt.Errorf("update retention schedule: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE retention_policies
		SET next_run_at = $2::timestamptz::timestamp, updated_at = now()
		WHERE workspace_id = $1 AND next_run_at IS DISTINCT FROM $2::timestamptz::timestamp`,
		workspaceID, next); err != nil {
		return nil, fmt.Errorf("update retention policy: %w", err)
	}
	if next != nil {
		fireKey := fmt.Sprintf("retention:%s:%s", workspaceID, next.UTC().Format("2006-01-02"))
		_, _, err := queue.EnqueueOnce(ctx, tx, JobType, map[string]string{"workspace_id": workspaceID}, fireKey, *next)
		if err != nil {
			return nil, err
		}
	}
	return next, nil
}
