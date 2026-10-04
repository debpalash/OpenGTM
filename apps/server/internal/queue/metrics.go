package queue

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
)

// Metrics has the same shape (and JSON keys) as QueueService.metrics, so
// dashboards can read either executor. It never exposes payloads.
type Metrics struct {
	WorkerID                string           `json:"worker_id"`
	ConfiguredConcurrency   int              `json:"configured_concurrency"`
	MaxActivePerWorkspace   int              `json:"max_active_per_workspace"`
	LocalActiveSlots        int              `json:"local_active_slots"`
	ActiveWorkers           int              `json:"active_workers"`
	Counts                  map[string]int64 `json:"counts"`
	ActiveByType            map[string]int64 `json:"active_by_type"`
	OldestPendingAgeSeconds float64          `json:"oldest_pending_age_seconds"`
	ObservedAt              time.Time        `json:"observed_at"`
}

// Metrics aggregates queue health across all executors.
func (q *Queue) Metrics(ctx context.Context) (Metrics, error) {
	m := Metrics{
		WorkerID:              q.opts.WorkerID,
		ConfiguredConcurrency: q.opts.Concurrency,
		MaxActivePerWorkspace: q.opts.MaxActivePerWorkspace,
		LocalActiveSlots:      int(q.active.Load()),
		Counts:                map[string]int64{},
		ActiveByType:          map[string]int64{},
	}
	groups := func(sql string, into map[string]int64) error {
		rows, err := q.pool.Query(ctx, sql)
		if err != nil {
			return err
		}
		var key string
		var n int64
		_, err = pgx.ForEachRow(rows, []any{&key, &n}, func() error {
			into[key] = n
			return nil
		})
		return err
	}
	if err := groups(`SELECT coalesce(status, 'null'), count(id) FROM jobs GROUP BY status`, m.Counts); err != nil {
		return m, fmt.Errorf("queue: metrics counts: %w", err)
	}
	if err := groups(`SELECT coalesce(type, 'null'), count(id) FROM jobs
		WHERE status IN ('pending', 'processing') GROUP BY type`, m.ActiveByType); err != nil {
		return m, fmt.Errorf("queue: metrics types: %w", err)
	}
	var age *float64
	err := q.pool.QueryRow(ctx, `
SELECT (SELECT count(DISTINCT worker_id) FROM jobs WHERE status = 'processing' AND worker_id IS NOT NULL),
       (SELECT extract(epoch FROM LOCALTIMESTAMP - min(created_at))::float8 FROM jobs WHERE status = 'pending'),
       now()`).Scan(&m.ActiveWorkers, &age, &m.ObservedAt)
	if err != nil {
		return m, fmt.Errorf("queue: metrics summary: %w", err)
	}
	if age != nil {
		m.OldestPendingAgeSeconds = math.Round(max(0, *age)*1000) / 1000
	}
	m.ObservedAt = m.ObservedAt.UTC()
	return m, nil
}
