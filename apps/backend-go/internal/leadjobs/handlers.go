package leadjobs

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
)

// Handler adapts HandleSourceHealthCheck to the worker's runner signature. The worker's heartbeat
// and cancel watcher own the lease (a lost lease or cancel cancels ctx, which aborts the probe RPC
// before anything is applied) and its failure bookkeeping.
func Handler(d Deps) func(context.Context, *queue.Claim) error {
	return func(ctx context.Context, c *queue.Claim) error { return HandleSourceHealthCheck(ctx, d, c) }
}

// BootstrapSourceHealth is bootstrap_source_health: the cold-start seed. When the probe is enabled
// and no health job is pending or processing it enqueues one (executor from d.Executor), inside the
// caller's transaction. It reports whether it enqueued, and is a no-op with
// SOURCE_HEALTH_ENABLED off. Idempotent and replica-safe: the chain lock and the exact fire key
// let exactly one of any number of concurrent callers insert.
func BootstrapSourceHealth(ctx context.Context, tx pgx.Tx, d Deps) (bool, error) {
	if !d.ProbeEnabled() {
		return false, nil
	}
	return d.enqueueIfAbsent(ctx, tx, 0, knobsFrom(d.get), true)
}
