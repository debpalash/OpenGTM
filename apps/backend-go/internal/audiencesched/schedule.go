// Package audiencesched ports audiences/scheduler.py: schedule_next, the
// audience_schedules mirror, remove_schedule and the queue-failure reconcile.
package audiencesched

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
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
	NextRefreshAt *time.Time
}

var errNotImplemented = errors.New("audiencesched: not implemented")

func ScheduleNext(ctx context.Context, tx pgx.Tx, a ScheduleInput, now time.Time, executor string) (*time.Time, error) {
	return nil, errNotImplemented
}

func RemoveSchedule(ctx context.Context, tx pgx.Tx, audienceID string) error {
	return errNotImplemented
}
