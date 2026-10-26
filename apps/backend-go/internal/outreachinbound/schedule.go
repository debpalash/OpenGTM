package outreachinbound

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachstore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// JobType is the durable-queue job type.
const JobType = "outreach_inbound_poll"

const maxBackoffSeconds = 86400

// ScheduleConfig carries what Reschedule needs: the interval and failure cap
// (Outreach) and the executor routing (cohort + GO_EXECUTOR_OUTREACH_INBOUND).
type ScheduleConfig struct {
	Outreach config.OutreachSettings
	Executor config.ExecutorSettings
}

// Backoff is min(interval * 2^(failures-1), 86400) seconds.
func Backoff(intervalSec, failures int) int {
	if failures < 1 {
		failures = 1
	}
	if intervalSec >= maxBackoffSeconds || failures > 40 {
		return maxBackoffSeconds
	}
	d := intervalSec << (failures - 1)
	if d > maxBackoffSeconds || d < 0 {
		return maxBackoffSeconds
	}
	return d
}

// FireKey is inbound_fire_key: inbound:<ws>:<datetime.isoformat()> of an aware
// UTC time (microseconds only when non-zero, +00:00 offset).
func FireKey(wsID string, next time.Time) string {
	next = next.UTC()
	layout := "2006-01-02T15:04:05"
	if next.Nanosecond()/1000 != 0 {
		layout += ".000000"
	}
	return "inbound:" + wsID + ":" + next.Truncate(time.Microsecond).Format(layout) + "+00:00"
}

// Reschedule is _reschedule: update the non-RLS mirror row and, while the
// schedule stays enabled, enqueue the next poll once (fire_key single-flight).
// A failure bumps the counter, backs off min(interval*2^(n-1), 86400) and, at
// the cap, disables the schedule with no further poll. Success resets the
// counter. uidValidity (nil: keep) is stored as given.
func Reschedule(ctx context.Context, pool *pgxpool.Pool, wsID string, failed bool, uidValidity *string, now time.Time, cfg ScheduleConfig) error {
	interval := cfg.Outreach.InboundPollInterval
	return tenancy.InWorkspace(ctx, pool, wsID, func(tx pgx.Tx) error {
		// serialise concurrent reschedules of one workspace: the failure counter
		// is read-modify-write
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "inbound-resched:"+wsID); err != nil {
			return fmt.Errorf("outreachinbound: lock: %w", err)
		}
		cur, err := outreachstore.GetInboundSchedule(ctx, tx, wsID)
		if err != nil {
			return err
		}
		failures := 0
		if cur != nil {
			failures = cur.ConsecutiveFailures
		}
		up := outreachstore.InboundScheduleUpsert{UIDValidity: uidValidity}
		var next time.Time
		if failed {
			failures++
			up.ConsecutiveFailures = &failures
			if failures >= cfg.Outreach.InboundMaxConsecutiveFailures {
				up.Enabled, up.NextPollAt = false, nil
				return outreachstore.UpsertInboundSchedule(ctx, tx, wsID, up)
			}
			next = now.Add(time.Duration(Backoff(interval, failures)) * time.Second)
		} else {
			zero := 0
			up.ConsecutiveFailures = &zero
			next = now.Add(time.Duration(interval) * time.Second)
		}
		up.Enabled, up.NextPollAt = true, &next
		if err := outreachstore.UpsertInboundSchedule(ctx, tx, wsID, up); err != nil {
			return err
		}
		return enqueueNext(ctx, tx, wsID, next, cfg)
	})
}

func enqueueNext(ctx context.Context, tx pgx.Tx, wsID string, next time.Time, cfg ScheduleConfig) error {
	executor, err := cfg.Executor.OutreachExecutor(cfg.Outreach, JobType, wsID)
	if err != nil {
		return err
	}
	key := FireKey(wsID, next)
	id, created, err := queue.EnqueueOnce(ctx, tx, JobType, wsID, struct {
		WorkspaceID string `json:"workspace_id"`
	}{wsID}, key, executor)
	if err != nil || !created {
		return err
	}
	// EnqueueOnce makes the job due now; Python's next_run_at is the backoff time.
	if _, err := tx.Exec(ctx, `UPDATE jobs SET next_run_at = ($1::timestamptz AT TIME ZONE 'UTC') WHERE id = $2`, next.UTC(), id); err != nil {
		return fmt.Errorf("outreachinbound: set next_run_at: %w", err)
	}
	return nil
}
