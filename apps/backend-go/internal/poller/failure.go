package poller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// Deps are the poller core's dependencies; later poller files (HTTP sources,
// pyworker collectors, the poll handler) extend nothing here and take Deps.
type Deps struct {
	Pool *pgxpool.Pool
	Cfg  Config
	// Now is the clock (nil: time.Now).
	Now func() time.Time
	// ExecutorFor picks the executor for a workspace's next poll (nil: python).
	ExecutorFor func(ws string) string
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d Deps) executor(ws string) string {
	if d.ExecutorFor != nil {
		return d.ExecutorFor(ws)
	}
	return "python"
}

const errPayload = "watch poll failure payload requires workspace_id and watch_id"

// cancelReason is the jobs.error text Python writes when it cancels the pending
// retry of an auto-disabled watch.
const cancelReason = "Watch auto-disabled after consecutive failures"

func truncRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// Reconcile is reconcile_watch_poll_failure, registered for job type
// watch_poll with queue.RegisterFailureHook. It runs after the queue persisted
// a failed attempt, inside the payload workspace's tenancy.InWorkspace (a watch
// of any other workspace is never touched). A missing or disabled watch is left
// alone. The error text is stored as "queue_failure: <err>" cut to 255
// characters. A final failure reschedules with backoff (or auto-disables); a
// retry counts a failure, auto-disables at the limit (cancelling the still
// pending retry job) or moves next_poll_at to the retry's next_run_at (default
// now + 1 minute) and mirrors it.
func Reconcile(ctx context.Context, d Deps, n queue.FailureNotice) error {
	var p struct {
		WorkspaceID string `json:"workspace_id"`
		WatchID     string `json:"watch_id"`
	}
	if err := json.Unmarshal(n.Payload, &p); err != nil {
		return errors.New(errPayload)
	}
	ws, watchID := strings.TrimFunc(p.WorkspaceID, isPySpace), strings.TrimFunc(p.WatchID, isPySpace)
	if ws == "" || watchID == "" {
		return errors.New(errPayload)
	}
	reason := n.Reason
	if reason == "" {
		reason = "watch poll failed"
	}
	lastErr := truncRunes("queue_failure: "+reason, 255)

	return tenancy.InWorkspace(ctx, d.Pool, ws, func(tx pgx.Tx) error {
		w, ok, err := LoadWatch(ctx, tx, watchID, ws)
		if err != nil || !ok || !w.Enabled {
			return err
		}
		w.LastError = &lastErr
		if !n.WillRetry {
			_, err := RescheduleWatch(ctx, tx, w, true, d.now(), d.Cfg, d.executor(ws))
			return err
		}
		w.ConsecutiveFailures++
		q := gen.New(tx)
		job, jobErr := q.PollJobForReconcile(ctx, gen.PollJobForReconcileParams{ID: n.JobID, WorkspaceID: ws})
		if jobErr != nil && !errors.Is(jobErr, pgx.ErrNoRows) {
			return fmt.Errorf("poller: load failed job: %w", jobErr)
		}
		if w.ConsecutiveFailures >= d.Cfg.MaxConsecutiveFailures {
			if _, err := disable(ctx, tx, w); err != nil {
				return err
			}
			if jobErr == nil && job.Status.String == "pending" {
				if err := q.PollCancelPendingJob(ctx, gen.PollCancelPendingJobParams{ID: n.JobID, WorkspaceID: ws, Error: cancelReason}); err != nil {
					return fmt.Errorf("poller: cancel retry job: %w", err)
				}
			}
			return nil
		}
		retryAt := d.now().UTC().Add(time.Minute)
		if jobErr == nil && job.NextRunAt.Valid {
			retryAt = job.NextRunAt.Time.UTC()
		}
		w.NextPollAt = &retryAt
		if err := MirrorSet(ctx, tx, w.ID, ws, &retryAt, true); err != nil {
			return err
		}
		return saveWatch(ctx, tx, w)
	})
}
