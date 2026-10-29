package schedboot

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audiencesched"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// Audiences is bootstrap_audience_schedules: for each due mirror row, drop the schedule when the audience
// is gone or no longer refreshes, otherwise enqueue its refresh once (an occurrence still pending or
// processing blocks it). It returns the number of refreshes enqueued.
func (d Deps) Audiences(ctx context.Context) (int, error) {
	due, err := audiencesched.DueSchedules(ctx, d.Pool, d.now())
	if err != nil {
		return 0, err
	}
	exec := d.audienceExecutor()
	var rows perRow
	n := 0
	for _, r := range due {
		created := false
		err := tenancy.InWorkspace(ctx, d.Pool, r.WorkspaceID, func(tx pgx.Tx) error {
			var enabled bool
			err := tx.QueryRow(ctx, `SELECT refresh_enabled FROM audiences WHERE id = $1 AND workspace_id = $2`, r.AudienceID, r.WorkspaceID).Scan(&enabled)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && !enabled) {
				return audiencesched.RemoveSchedule(ctx, tx, r.AudienceID)
			}
			if err != nil {
				return err
			}
			if active, err := activeWithKeyPrefix(ctx, tx, audiencesched.JobType, "audience_refresh:"+r.AudienceID+":", shapeISOOrBootstrap); err != nil || active {
				return err
			}
			key := "audience_refresh:" + r.AudienceID + ":bootstrap"
			if r.NextRefreshAt != nil {
				key = audiencesched.FireKey(r.AudienceID, *r.NextRefreshAt)
			}
			_, created, err = queue.EnqueueOnce(ctx, tx, audiencesched.JobType, r.WorkspaceID, struct {
				WorkspaceID string `json:"workspace_id"`
				AudienceID  string `json:"audience_id"`
			}{r.WorkspaceID, r.AudienceID}, key, exec(r.WorkspaceID))
			return err
		})
		if err != nil {
			rows.add("audience", r.AudienceID, err)
			continue
		}
		if created {
			n++
		}
	}
	if err := rows.err(); err != nil {
		return n, fmt.Errorf("schedboot: audiences: %w", err)
	}
	d.log().Info("bootstrap_audience_schedules", "enqueued", n)
	return n, nil
}
