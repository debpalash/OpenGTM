package poller

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

// BootstrapWatchSchedules is bootstrap_watch_schedules: read the non-RLS mirror for due rows
// with no workspace scope (the only cross-tenant read), then re-enter tenancy.InWorkspace per
// watch, skip missing or disabled watches (a watch of another workspace named by a mirror row
// is "missing": the load is workspace scoped) and enqueue single-flight under the exact key
// Python locks. On a created job the watch's next_poll_at and the mirror advance in the same
// transaction. execFor picks the executor per workspace (nil: python). A failing watch does
// not stop the others; the errors are joined. The returned count is the jobs created.
func BootstrapWatchSchedules(ctx context.Context, pool *pgxpool.Pool, now time.Time, cfg Config, execFor func(ws string) string) (int, error) {
	due, err := gen.New(pool).PollDueMirror(ctx, tsz(&now))
	if err != nil {
		return 0, fmt.Errorf("poller: read mirror: %w", err)
	}
	var errs []error
	enqueued := 0
	for _, d := range due {
		executor := "python"
		if execFor != nil {
			executor = execFor(d.WorkspaceID)
		}
		created := false
		err := tenancy.InWorkspace(ctx, pool, d.WorkspaceID, func(tx pgx.Tx) error {
			w, ok, err := LoadWatch(ctx, tx, d.WatchID, d.WorkspaceID)
			if err != nil || !ok || !w.Enabled {
				return err
			}
			loc, err := SessionLocation(ctx, tx)
			if err != nil {
				return err
			}
			next := NextPollForIn(w, now, cfg, loc)
			created, err = EnqueuePollIfAbsent(ctx, tx, ScheduledFireKeyIn(w.ID, next, loc), w.WorkspaceID, w.ID, next, false, executor)
			if err != nil || !created {
				return err
			}
			w.NextPollAt = &next
			if err := saveWatch(ctx, tx, w); err != nil {
				return err
			}
			return MirrorSet(ctx, tx, w.ID, w.WorkspaceID, &next, true)
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("watch %s: %w", d.WatchID, err))
			continue
		}
		if created {
			enqueued++
		}
	}
	return enqueued, errors.Join(errs...)
}

// BootstrapWatchSchedulesIf is BootstrapWatchSchedules behind INTENT_POLLER_ENABLED and
// PG_LEAD_STORE (the poller is PG-only): either off returns 0 without touching the database.
func BootstrapWatchSchedulesIf(ctx context.Context, pool *pgxpool.Pool, enabled, pgStore bool, now time.Time, cfg Config, execFor func(ws string) string) (int, error) {
	if !enabled || !pgStore {
		return 0, nil
	}
	return BootstrapWatchSchedules(ctx, pool, now, cfg, execFor)
}
