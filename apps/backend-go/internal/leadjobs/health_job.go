// Package leadjobs holds the plan 14 (leadgen) durable jobs of the Go worker. source_health_check
// mirrors apps/api/services/leadgen/source_health.py handle_source_health_check: probe every
// registry source, apply the systemic-outage guard and the health transitions, then
// self-reschedule on a single global cadence.
package leadjobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autoschedule"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/sourcehealth"
)

// JobType is the durable job type.
const JobType = "source_health_check"

const (
	listTimeout         = 30 * time.Second
	defaultBatchTimeout = 120 * time.Second
	// chainLockKey serializes every enqueue of the one global health chain.
	chainLockKey = "source_health:chain"
)

// Prober lists and probes the scraped sources (internal/collect over leadweb and the browser service), so the
// probes run behind the egress controls.
type Prober interface {
	SourceList(ctx context.Context, timeout time.Duration) ([]string, error)
	// SourceProbe probes one batch; a nil yield is a probe that did not finish or raised.
	SourceProbe(ctx context.Context, names []string, timeout time.Duration) (map[string]*int, error)
}

// Deps are the handler's dependencies. Getenv reads the SOURCE_HEALTH_* knobs at call time (as
// Python's _cfg does); Executor is config.ExecutorSettings.SourceHealthExecutor for the next job.
type Deps struct {
	Pool         *pgxpool.Pool
	Py           Prober
	Getenv       func(string) string
	Executor     func() string
	Now          func() time.Time
	BatchTimeout time.Duration
	Log          *slog.Logger
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now().UTC()
	}
	return time.Now().UTC()
}

func (d Deps) get(k string) string {
	if d.Getenv == nil {
		return ""
	}
	return d.Getenv(k)
}

func (d Deps) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}

func (d Deps) executor() string {
	if d.Executor != nil {
		return d.Executor()
	}
	return "go"
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "t", "yes", "y", "on":
		return true
	}
	return false
}

// ProbeEnabled is probe_enabled: the SOURCE_HEALTH_ENABLED master switch.
func (d Deps) ProbeEnabled() bool { return truthy(d.get("SOURCE_HEALTH_ENABLED")) }

type knobs struct {
	concurrency int
	outageRatio float64
	interval    string
}

func knobsFrom(get func(string) string) knobs {
	k := knobs{concurrency: 8, outageRatio: sourcehealth.DefaultOutageRatio, interval: "daily"}
	if n, err := strconv.Atoi(strings.TrimSpace(get("SOURCE_HEALTH_PROBE_CONCURRENCY"))); err == nil {
		k.concurrency = n
	}
	if k.concurrency < 1 {
		k.concurrency = 1
	}
	if f, err := strconv.ParseFloat(strings.TrimSpace(get("SOURCE_HEALTH_OUTAGE_RATIO")), 64); err == nil && f == f {
		k.outageRatio = f
	}
	if v := strings.TrimSpace(get("SOURCE_HEALTH_INTERVAL")); v != "" {
		k.interval = v
	}
	return k
}

// nextRunAt is _next_run_at: compute_next_run from the fixed 2026-01-01 UTC anchor, so the fire
// schedule is stable and replica-independent.
func nextRunAt(interval string, now time.Time) time.Time {
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return autoschedule.NextRun(anchor, interval, now)
}

// FireKey is the occurrence key source_health:<next run, UTC isoformat to the second>.
func FireKey(next time.Time) string {
	return "source_health:" + next.UTC().Truncate(time.Second).Format("2006-01-02T15:04:05") + "+00:00"
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, n := range in {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// HandleSourceHealthCheck is handle_source_health_check. With SOURCE_HEALTH_ENABLED off it is a
// no-op and does not reschedule. Probes go to the Prober one batch of
// SOURCE_HEALTH_PROBE_CONCURRENCY at a time. A null yield or a failed RPC counts as yield 0 and
// marks the whole run an outage (stricter than Python, so an unreachable worker can never
// auto-disable a source). A cancelled context aborts the RPC and applies nothing.
func HandleSourceHealthCheck(ctx context.Context, d Deps, job *queue.Claim) error {
	if !d.ProbeEnabled() {
		d.log().Info("source_health_check: SOURCE_HEALTH_ENABLED off, skip and no reschedule", "job_id", jobID(job))
		return nil
	}
	if d.Py == nil {
		return errors.New("source_health_check: no source prober configured")
	}
	k := knobsFrom(d.get)
	names, err := d.Py.SourceList(ctx, listTimeout)
	if err != nil {
		return fmt.Errorf("source_health_check: list sources: %w", err)
	}
	names = dedupe(names)
	timeout := d.BatchTimeout
	if timeout <= 0 {
		timeout = defaultBatchTimeout
	}
	yields := make(map[string]int, len(names))
	rpcOutage := false
	for i := 0; i < len(names); i += k.concurrency {
		batch := names[i:min(i+k.concurrency, len(names))]
		res, err := d.Py.SourceProbe(ctx, batch, timeout)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			d.log().Warn("source_health_check: probe batch failed; run counts as an outage", "job_id", jobID(job), "sources", len(batch))
			rpcOutage = true
		}
		for _, n := range batch {
			y := res[n] // nil map read is safe
			if err != nil || y == nil {
				rpcOutage = true
				yields[n] = 0
				continue
			}
			yields[n] = *y
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	all := make([]int, 0, len(yields))
	for _, y := range yields {
		all = append(all, y)
	}
	outage := rpcOutage || sourcehealth.OutageGuard(all, k.outageRatio)
	if outage {
		d.log().Warn("source_health_check: outage guard tripped; no zero transitions applied", "job_id", jobID(job), "rpc_outage", rpcOutage)
	}
	if err := d.apply(ctx, yields, outage); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Python swallows a failed reschedule (warning only); the next bootstrap re-seeds the chain.
	if _, err := d.reschedule(ctx, jobID(job), k); err != nil {
		d.log().Warn("source_health reschedule skipped", "job_id", jobID(job), "err", err)
	}
	return nil
}

func jobID(j *queue.Claim) int64 {
	if j == nil {
		return 0
	}
	return j.ID
}

// apply persists every transition in one transaction: all rows commit together or none do.
func (d Deps) apply(ctx context.Context, yields map[string]int, outage bool) error {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := sourcehealth.ApplyRunWith(ctx, tx, yields, outage, d.now(), sourcehealth.ThresholdsFromEnv(d.get)); err != nil {
		return fmt.Errorf("source_health_check: apply: %w", err)
	}
	return tx.Commit(ctx)
}

// reschedule is _reschedule_next: one next job, single-flight under concurrency.
func (d Deps) reschedule(ctx context.Context, selfID int64, k knobs) (bool, error) {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	ok, err := d.enqueueIfAbsent(ctx, tx, selfID, k, false)
	if err != nil {
		return false, err
	}
	return ok, tx.Commit(ctx)
}

// enqueueIfAbsent is _enqueue_health_if_absent. Under the chain lock it enqueues one job unless a
// pending occurrence exists (handler: any pending job of the type; bootstrap also counts a
// processing one, which will reschedule itself) or the exact fire key is already active. Keys and
// the type are compared exactly, never with LIKE.
func (d Deps) enqueueIfAbsent(ctx context.Context, tx pgx.Tx, selfID int64, k knobs, countProcessing bool) (bool, error) {
	q := gen.New(tx)
	if err := q.LockJobFireKey(ctx, chainLockKey); err != nil {
		return false, err
	}
	var busy bool
	statuses := []string{"pending"}
	if countProcessing {
		statuses = append(statuses, "processing")
	}
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM jobs WHERE type = $1 AND status = ANY($2::text[]) AND id <> $3)`,
		JobType, statuses, selfID).Scan(&busy); err != nil {
		return false, err
	}
	if busy {
		return false, nil
	}
	executor := d.executor()
	if executor != "go" {
		return false, fmt.Errorf("source_health: executor %q must be go (the only executor since the plan 15 cutover)", executor)
	}
	next := nextRunAt(k.interval, d.now())
	key := FireKey(next)
	payload, err := json.Marshal(map[string]string{"fire_key": key})
	if err != nil {
		return false, err
	}
	var id int64
	err = tx.QueryRow(ctx, `INSERT INTO jobs (executor, type, payload, priority, fire_key, status, created_at, next_run_at, max_retries, retry_count)
		VALUES ($1, $2, $5::json, 1, $3, 'pending', (now() AT TIME ZONE 'UTC'),
		        ($4::timestamptz AT TIME ZONE 'UTC'), 3, 0)
		ON CONFLICT (fire_key) WHERE status IN ('pending', 'processing') DO NOTHING
		RETURNING id`, executor, JobType, key, next.Truncate(time.Microsecond), string(payload)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
