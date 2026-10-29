package schedboot

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/playbooks"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

const playbookScheduleJob = "research_playbook_schedule"

// Playbooks is bootstrap_playbook_schedules: for each due mirror row, drop the schedule when the playbook
// is gone, disabled or has no schedule audience, otherwise enqueue its schedule occurrence once (an
// occurrence pending or processing blocks it). It returns the occurrences enqueued.
func (d Deps) Playbooks(ctx context.Context) (int, error) {
	now := d.now()
	rs, err := d.Pool.Query(ctx, `SELECT playbook_id, workspace_id, next_run_at AT TIME ZONE 'UTC' FROM playbook_schedules
	    WHERE enabled AND (next_run_at IS NULL OR next_run_at <= ($1::timestamptz AT TIME ZONE 'UTC'))
	    ORDER BY playbook_id`, now)
	if err != nil {
		return 0, fmt.Errorf("schedboot: read playbook schedules: %w", err)
	}
	type due struct {
		id, ws string
		at     *time.Time
	}
	var all []due
	for rs.Next() {
		var x due
		if err := rs.Scan(&x.id, &x.ws, &x.at); err != nil {
			rs.Close()
			return 0, err
		}
		all = append(all, x)
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return 0, err
	}
	var rows perRow
	n := 0
	for _, x := range all {
		created := false
		err := tenancy.InWorkspace(ctx, d.Pool, x.ws, func(tx pgx.Tx) error {
			var enabled bool
			var audience *string
			err := tx.QueryRow(ctx, `SELECT enabled, schedule_audience_id FROM research_playbooks WHERE id = $1 AND workspace_id = $2`, x.id, x.ws).Scan(&enabled, &audience)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && (!enabled || audience == nil || *audience == "")) {
				return playbooks.RemoveSchedule(ctx, tx, x.id)
			}
			if err != nil {
				return err
			}
			if active, err := activeWithKeyPrefix(ctx, tx, playbookScheduleJob, "playbook_schedule:"+x.id+":", shapeISOOrBootstrap); err != nil || active {
				return err
			}
			key := "playbook_schedule:" + x.id + ":bootstrap"
			if x.at != nil {
				key = playbooks.ScheduleFireKey(x.id, *x.at)
			}
			_, created, err = queue.EnqueueOnce(ctx, tx, playbookScheduleJob, x.ws, struct {
				WorkspaceID string `json:"workspace_id"`
				PlaybookID  string `json:"playbook_id"`
			}{x.ws, x.id}, key, playbooks.ExecutorFor(d.Cfg.Executor, d.Cfg.Flags.Governance, x.ws))
			return err
		})
		if err != nil {
			rows.add("playbook", x.id, err)
			continue
		}
		if created {
			n++
		}
	}
	if err := rows.err(); err != nil {
		return n, fmt.Errorf("schedboot: playbooks: %w", err)
	}
	return n, nil
}
