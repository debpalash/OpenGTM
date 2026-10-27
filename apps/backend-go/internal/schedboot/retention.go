package schedboot

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/retention"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// Retention is bootstrap_retention_schedules: for each enabled schedule that is due, enqueue the
// workspace's retention_enforce for that occurrence's UTC date, unless the policy is missing, disabled or
// on legal hold or an enforcement is already pending or processing. It returns the jobs enqueued.
func (d Deps) Retention(ctx context.Context) (int, error) {
	now := d.now()
	rs, err := d.Pool.Query(ctx, `SELECT workspace_id, next_run_at AT TIME ZONE 'UTC' FROM retention_schedules
	    WHERE enabled AND (next_run_at IS NULL OR next_run_at <= ($1::timestamptz AT TIME ZONE 'UTC'))
	    ORDER BY workspace_id`, now)
	if err != nil {
		return 0, fmt.Errorf("schedboot: read retention schedules: %w", err)
	}
	type due struct {
		ws   string
		next *time.Time
	}
	var all []due
	for rs.Next() {
		var x due
		if err := rs.Scan(&x.ws, &x.next); err != nil {
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
			var enabled, hold bool
			err := tx.QueryRow(ctx, `SELECT enabled, legal_hold FROM retention_policies WHERE workspace_id = $1`, x.ws).Scan(&enabled, &hold)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && (!enabled || hold)) {
				return nil
			}
			if err != nil {
				return err
			}
			if active, err := activeWithKeyPrefix(ctx, tx, retention.JobType, "retention:"+x.ws+":", shapeDate); err != nil || active {
				return err
			}
			occurrence := now
			if x.next != nil {
				occurrence = *x.next
			}
			_, created, err = queue.EnqueueOnce(ctx, tx, retention.JobType, x.ws, struct {
				WorkspaceID string `json:"workspace_id"`
			}{x.ws}, retention.FireKey(x.ws, occurrence), retention.ExecutorFor(d.Cfg.Executor, d.Cfg.Flags.Governance, x.ws))
			return err
		})
		if err != nil {
			rows.add("retention workspace", x.ws, err)
			continue
		}
		if created {
			n++
		}
	}
	if err := rows.err(); err != nil {
		return n, fmt.Errorf("schedboot: retention: %w", err)
	}
	return n, nil
}
