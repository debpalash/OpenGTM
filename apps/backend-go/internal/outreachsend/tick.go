package outreachsend

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachstore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// TickConfig carries OUTREACH_TICK_INTERVAL, OUTREACH_TICK_MAX_ENQUEUE and the
// executor for enqueued jobs (ExecutorFor(GO_EXECUTOR_OUTREACH_SEND)).
type TickConfig struct {
	Interval   time.Duration
	MaxEnqueue int
	Executor   string
}

// Tick is tick_sequence: a missing or non-active sequence disables its schedule
// and returns 0; otherwise it enqueues due sends and upserts the schedule at
// now+Interval. One transaction, so a failure leaves neither jobs nor schedule.
func Tick(ctx context.Context, pool *pgxpool.Pool, wsID, seqID string, now time.Time, cfg TickConfig) (int, error) {
	n := 0
	err := tenancy.InWorkspace(ctx, pool, wsID, func(tx pgx.Tx) error {
		seq, err := outreachstore.GetSequence(ctx, tx, wsID, seqID)
		if err != nil {
			return err
		}
		if seq == nil || seq.Status == nil || *seq.Status != "active" {
			n = 0
			return outreachstore.DisableSchedule(ctx, tx, seqID)
		}
		n, err = EnqueueDue(ctx, tx, wsID, seqID, cfg.MaxEnqueue, cfg.Executor)
		if err != nil {
			return err
		}
		return outreachstore.UpsertSchedule(ctx, tx, wsID, seqID, now.Add(cfg.Interval), true)
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}
