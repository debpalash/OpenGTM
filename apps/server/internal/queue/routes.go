package queue

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/server/internal/db"
)

// Executors accepted by job_executor_routes.
const (
	ExecutorPython = "python"
	ExecutorGo     = "go"
)

// Route is one job_executor_routes row plus live queue counts for the type,
// so an operator sees what a switch would affect.
type Route struct {
	JobType    string    `json:"job_type"`
	Executor   string    `json:"executor"`
	UpdatedAt  time.Time `json:"updated_at"`
	Pending    int64     `json:"pending"`
	Processing int64     `json:"processing"`
}

// InFlightError refuses a route change while attempts of the type run under
// the current executor.
type InFlightError struct {
	JobType    string
	Processing int64
}

func (e *InFlightError) Error() string {
	return fmt.Sprintf("%d %s job(s) are processing under the current executor", e.Processing, e.JobType)
}

const routeCountsSQL = `
coalesce((SELECT count(*) FROM jobs WHERE type = r.job_type AND status = 'pending'), 0),
coalesce((SELECT count(*) FROM jobs WHERE type = r.job_type AND status = 'processing'), 0)`

// ListRoutes returns every explicit route. Types without a row are Python's.
func ListRoutes(ctx context.Context, b db.Beginner) ([]Route, error) {
	var out []Route
	err := db.WithoutTenant(ctx, b, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT r.job_type, r.executor, r.updated_at, `+routeCountsSQL+`
			FROM job_executor_routes r ORDER BY r.job_type`)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Route])
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("queue: list routes: %w", err)
	}
	return out, nil
}

// SetRoute assigns jobType to executor. Unless force is set it refuses while
// jobs of the type are processing: the new executor must not start claiming
// while the old one may still finalize, retry or reap them. The check is
// advisory (claims do not take this lock), so drain the old executor first.
func SetRoute(ctx context.Context, b db.Beginner, jobType, executor string, force bool) (Route, error) {
	if jobType == "" {
		return Route{}, errors.New("queue: job type is required")
	}
	if executor != ExecutorPython && executor != ExecutorGo {
		return Route{}, fmt.Errorf("queue: executor %q must be %q or %q", executor, ExecutorPython, ExecutorGo)
	}
	var route Route
	err := db.WithoutTenant(ctx, b, func(tx pgx.Tx) error {
		// Serialise concurrent operators changing the same type.
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('job_executor_routes:' || $1))", jobType); err != nil {
			return err
		}
		var processing int64
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM jobs WHERE type = $1 AND status = 'processing'",
			jobType).Scan(&processing); err != nil {
			return err
		}
		if processing > 0 && !force {
			return &InFlightError{JobType: jobType, Processing: processing}
		}
		rows, err := tx.Query(ctx, `
WITH up AS (
  INSERT INTO job_executor_routes (job_type, executor, updated_at) VALUES ($1, $2, now())
  ON CONFLICT (job_type) DO UPDATE SET executor = EXCLUDED.executor, updated_at = now()
  RETURNING job_type, executor, updated_at)
SELECT r.job_type, r.executor, r.updated_at, `+routeCountsSQL+` FROM up r`, jobType, executor)
		if err != nil {
			return err
		}
		route, err = pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[Route])
		return err
	})
	if err != nil {
		var inflight *InFlightError
		if errors.As(err, &inflight) {
			return Route{}, err
		}
		return Route{}, fmt.Errorf("queue: set route: %w", err)
	}
	return route, nil
}
