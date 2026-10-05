package retention

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/server/internal/jobs/jobkit"
	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
)

// scheduleMirror is the non-RLS retention_schedules mirror, keyed by workspace.
var scheduleMirror = jobkit.Mirror{
	Table: "retention_schedules", KeyCol: "workspace_id", EnabledCol: "enabled", NextCol: "next_run_at",
}

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
	if _, err := jobkit.CancelPending(ctx, tx, JobType, "retention:"+workspaceID+":%"); err != nil {
		return nil, err
	}
	var next *time.Time
	if policy.enabled && !policy.legalHold {
		n := now.UTC().Add(24 * time.Hour)
		next = &n
	}
	if err := scheduleMirror.Upsert(ctx, tx, workspaceID, workspaceID, next != nil, next); err != nil {
		return nil, err
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
