package audiencesched

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

const (
	errPayload   = "audience refresh failure payload requires workspace_id and audience_id"
	defaultError = "audience refresh failed"
	maxErrorLen  = 1000
)

func payloadID(payload map[string]any, key string) string {
	v, ok := payload[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

func truncRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// ReconcileFailure is reconcile_audience_refresh_failure, registered for job
// type audience_refresh as a queue failure hook (it runs after the queue
// persisted the failed attempt). It enters the payload workspace's
// tenancy.InWorkspace; a missing or disabled audience (or one of another
// workspace) is left alone. A next_refresh_at after now means the handler
// already recorded this attempt: nothing is counted again. Otherwise the audience
// turns degraded with last_refresh_error "Queue retry: ..." or "Final failure:
// ..." (cut to 1000 characters) and one more consecutive failure. On a retry the
// queue's next_run_at is mirrored into next_refresh_at and the schedule row
// (the queue's job is not cancelled); after a terminal failure ScheduleNext
// creates the next regular occurrence. executorFor picks the executor (always "go" since the plan 15 cutover) for
// that job per workspace (nil: go). now is the aware UTC clock.
func ReconcileFailure(ctx context.Context, pool *pgxpool.Pool, executorFor func(ws string) string, jobID int64, payload map[string]any, errText string, willRetry bool, now time.Time) error {
	ws, id := payloadID(payload, "workspace_id"), payloadID(payload, "audience_id")
	if ws == "" || id == "" {
		return errors.New(errPayload)
	}
	if jobID < math.MinInt32 || jobID > math.MaxInt32 {
		return fmt.Errorf("audiencesched: job id %d out of range", jobID)
	}
	now = now.UTC().Truncate(time.Microsecond)
	executor := "go"
	if executorFor != nil {
		executor = executorFor(ws)
	}
	if err := checkExecutor(executor); err != nil {
		return err
	}
	if errText == "" {
		errText = defaultError
	}
	label := "Final failure"
	if willRetry {
		label = "Queue retry"
	}
	return tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		q := gen.New(tx)
		row, err := q.AudSchedLockAudience(ctx, gen.AudSchedLockAudienceParams{ID: id, WorkspaceID: ws})
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !row.RefreshEnabled) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("audiencesched: load audience: %w", err)
		}
		// next_refresh_at is naive UTC (_as_utc), compared as an instant.
		already := row.NextRefreshAt.Valid && row.NextRefreshAt.Time.After(now)
		if already {
			return nil
		}
		if err := q.AudSchedMarkFailed(ctx, gen.AudSchedMarkFailedParams{
			LastRefreshError: truncRunes(label+": "+errText, maxErrorLen), ID: id, WorkspaceID: ws,
		}); err != nil {
			return fmt.Errorf("audiencesched: mark failed: %w", err)
		}
		if !willRetry {
			_, err := ScheduleNext(ctx, tx, ScheduleInput{
				ID: id, WorkspaceID: ws, RefreshEnabled: true, IntervalMinutes: int(row.RefreshIntervalMinutes),
			}, now, executor)
			return err
		}
		retryAt, err := q.AudSchedJobNextRun(ctx, int32(jobID))
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !retryAt.Valid) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("audiencesched: load job: %w", err)
		}
		if err := q.AudSchedSetNextRefreshNaive(ctx, gen.AudSchedSetNextRefreshNaiveParams{NextRefreshAt: retryAt, ID: id, WorkspaceID: ws}); err != nil {
			return fmt.Errorf("audiencesched: mirror retry time: %w", err)
		}
		return q.AudSchedUpsertMirrorNaive(ctx, gen.AudSchedUpsertMirrorNaiveParams{
			AudienceID: id, WorkspaceID: ws, NextRefreshAt: retryAt, Enabled: true,
		})
	})
}

// DueSchedules lists the enabled mirror rows due at now (next_refresh_at NULL or
// <= now), ordered by audience id: the bootstrap_audience_schedules query for a
// later Go scheduler. It reads the non-RLS mirror on the pool without a tenant;
// callers enter InWorkspace per row. A naive column is compared as UTC.
func DueSchedules(ctx context.Context, pool *pgxpool.Pool, now time.Time) ([]DueRow, error) {
	rows, err := gen.New(pool).AudSchedDue(ctx, pgtype.Timestamptz{Time: now.UTC(), Valid: true})
	if err != nil {
		return nil, fmt.Errorf("audiencesched: due schedules: %w", err)
	}
	out := make([]DueRow, 0, len(rows))
	for _, r := range rows {
		d := DueRow{AudienceID: r.AudienceID, WorkspaceID: r.WorkspaceID}
		if r.NextRefreshAt.Valid {
			t := r.NextRefreshAt.Time.UTC()
			d.NextRefreshAt = &t
		}
		out = append(out, d)
	}
	return out, nil
}
