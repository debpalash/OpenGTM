package audiencerefresh

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/server/internal/jobs/jobkit"
	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
)

// mirror is the non-RLS audience_schedules table.
var mirror = jobkit.Mirror{
	Table: "audience_schedules", KeyCol: "audience_id", WorkspaceCol: "workspace_id",
	EnabledCol: "enabled", NextCol: "next_refresh_at",
}

// removeSchedule ports remove_schedule: delete the audience's mirror row and
// cancel its pending occurrences. Unlike Python it only touches rows that
// belong to the job's own workspace: the mirror and jobs tables have no
// row-level security, and the audience id comes from the payload.
func removeSchedule(ctx context.Context, tx pgx.Tx, workspaceID, audienceID string) error {
	if err := mirror.DeleteFor(ctx, tx, audienceID, workspaceID); err != nil {
		return err
	}
	_, err := cancelPending(ctx, tx, workspaceID, audienceID)
	return err
}

func cancelPending(ctx context.Context, tx pgx.Tx, workspaceID, audienceID string) (int64, error) {
	return jobkit.CancelPendingFor(ctx, tx, JobType, "audience_refresh:"+audienceID+":%", workspaceID)
}

// scheduleNext ports schedule_next: cancel pending occurrences, then either
// clear the schedule (refresh disabled) or store, mirror and enqueue the next
// occurrence once. It returns the next run time, nil when none is scheduled.
func (w *Worker) scheduleNext(ctx context.Context, tx pgx.Tx, workspaceID string, a *audience, now time.Time) (*time.Time, error) {
	if _, err := cancelPending(ctx, tx, workspaceID, a.id); err != nil {
		return nil, err
	}
	if !a.enabled {
		if a.nextSet {
			if _, err := tx.Exec(ctx, `UPDATE audiences SET next_refresh_at = NULL, updated_at = now()
				WHERE id = $1 AND workspace_id = $2`, a.id, workspaceID); err != nil {
				return nil, fmt.Errorf("clear audience next_refresh_at: %w", err)
			}
		}
		return nil, mirror.Upsert(ctx, tx, a.id, workspaceID, false, nil)
	}
	minutes := jobkit.ClampMinutes(a.intervalMins, defaultMinutes, minMinutes, maxMinutes)
	next := jobkit.Micros(now.UTC()).Add(time.Duration(minutes) * time.Minute)
	fireKey := fmt.Sprintf("audience_refresh:%s:%s", a.id, jobkit.IsoFormat(next))
	payload := struct {
		WorkspaceID string `json:"workspace_id"`
		AudienceID  string `json:"audience_id"`
	}{workspaceID, a.id}
	if _, _, err := queue.EnqueueOnce(ctx, tx, JobType, payload, fireKey, next); err != nil {
		return nil, err
	}
	// The ORM sees a naive stored value and a new aware one as different, so
	// the column is always rewritten (bumping updated_at).
	if _, err := tx.Exec(ctx, `UPDATE audiences SET next_refresh_at = $3::timestamptz::timestamp, updated_at = now()
		WHERE id = $1 AND workspace_id = $2`, a.id, workspaceID, next); err != nil {
		return nil, fmt.Errorf("update audience next_refresh_at: %w", err)
	}
	a.nextSet = true
	if err := mirror.Upsert(ctx, tx, a.id, workspaceID, true, &next); err != nil {
		return nil, err
	}
	return &next, nil
}
