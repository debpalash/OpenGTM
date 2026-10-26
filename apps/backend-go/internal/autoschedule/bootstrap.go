package autoschedule

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// BootstrapForTrigger is schedule_bootstrap_for_trigger: compute the aligned
// next run (anchor defaults to now) and enqueue it if absent. It returns the
// computed instant whether or not a job was created. tx must be inside
// tenancy.InWorkspace.
func BootstrapForTrigger(ctx context.Context, tx pgx.Tx, t Trigger, now time.Time, executor string) (time.Time, error) {
	loc, err := SessionLocation(ctx, tx)
	if err != nil {
		return time.Time{}, err
	}
	next := t.next(now, loc)
	if _, err := EnqueueScheduleIfAbsent(ctx, tx, t, next, executor); err != nil {
		return time.Time{}, err
	}
	return next, nil
}

// BootstrapSchedules is bootstrap_schedules: read the non-RLS mirror for due
// rows with no workspace scope (the only cross-tenant read), then re-enter
// tenancy.InWorkspace per rule, skipping missing, disabled or non-on_schedule
// rules, and enqueue. execFor picks the executor per workspace (nil: python).
// A failing rule does not stop the others; the errors are joined.
func BootstrapSchedules(ctx context.Context, pool *pgxpool.Pool, now time.Time, execFor func(ws string) string) (int, error) {
	due, err := gen.New(pool).DueScheduledTriggers(ctx, tstz(now))
	if err != nil {
		return 0, fmt.Errorf("autoschedule: read mirror: %w", err)
	}
	var errs []error
	enqueued := 0
	for _, d := range due {
		executor := "python"
		if execFor != nil {
			executor = execFor(d.WorkspaceID)
		}
		var created bool
		err := tenancy.InWorkspace(ctx, pool, d.WorkspaceID, func(tx pgx.Tx) error {
			t, ok, err := LoadTrigger(ctx, tx, d.TriggerID, d.WorkspaceID)
			if err != nil || !ok || !t.Enabled || t.Type != "on_schedule" {
				return err
			}
			loc, err := SessionLocation(ctx, tx)
			if err != nil {
				return err
			}
			created, err = EnqueueScheduleIfAbsent(ctx, tx, t, t.next(now, loc), executor)
			return err
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("trigger %s: %w", d.TriggerID, err))
			continue
		}
		if created {
			enqueued++
		}
	}
	return enqueued, errors.Join(errs...)
}

// BootstrapSchedulesIf is BootstrapSchedules behind the AUTOMATIONS_ENABLED
// flag: off returns 0 without touching the database.
func BootstrapSchedulesIf(ctx context.Context, pool *pgxpool.Pool, enabled bool, now time.Time, execFor func(ws string) string) (int, error) {
	if !enabled {
		return 0, nil
	}
	return BootstrapSchedules(ctx, pool, now, execFor)
}
