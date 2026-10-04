package pluginrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/server/internal/db"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/declarative"
	"github.com/debpalash/OpenGTM/apps/server/internal/progress"
	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
)

// JobType is the queue job that executes one plugin run. It is routed to the
// Go executor by migration 7c1e5a9d3b20.
const JobType = "plugin_run"

type jobPayload struct {
	WorkspaceID string `json:"workspace_id"`
	RunID       string `json:"run_id"`
}

// Worker executes plugin_run jobs.
type Worker struct {
	pool    *pgxpool.Pool
	catalog *Catalog
	runner  *Runner
	log     *slog.Logger
}

// NewWorker builds the job handler.
func NewWorker(pool *pgxpool.Pool, catalog *Catalog, runner *Runner, log *slog.Logger) *Worker {
	if log == nil {
		log = slog.Default()
	}
	return &Worker{pool: pool, catalog: catalog, runner: runner, log: log}
}

// Register adds the handlers to a queue registry.
func (w *Worker) Register(r *queue.Registry) {
	r.Register(JobType, w.Handle)
	r.RegisterFailure(JobType, w.Reconcile)
}

// errSkip ends an attempt without running the plugin (run gone or cancelled).
var errSkip = errors.New("skip")

// Handle runs one attempt: claim the run, execute the plugin outside any
// transaction, then commit results under the job lease.
func (w *Worker) Handle(ctx context.Context, job queue.Job) error {
	var pl jobPayload
	if err := json.Unmarshal(job.Payload, &pl); err != nil || pl.RunID == "" || pl.WorkspaceID == "" {
		return fmt.Errorf("plugin_run payload requires workspace_id and run_id: %s", job.Payload)
	}
	if job.WorkspaceID != "" && job.WorkspaceID != pl.WorkspaceID {
		return fmt.Errorf("plugin_run payload workspace %q does not match job workspace %q", pl.WorkspaceID, job.WorkspaceID)
	}
	log := w.log.With("job_id", job.ID, "run_id", pl.RunID)

	var name string
	var inputs map[string]any
	err := db.WithTenant(ctx, w.pool, pl.WorkspaceID, func(tx pgx.Tx) error {
		if err := queue.HoldLease(ctx, tx, job); err != nil {
			return err
		}
		var status string
		var rawInputs []byte
		err := tx.QueryRow(ctx,
			`SELECT plugin_name, inputs, status FROM plugin_runs WHERE id = $1 FOR UPDATE`, pl.RunID,
		).Scan(&name, &rawInputs, &status)
		if errors.Is(err, pgx.ErrNoRows) {
			log.Warn("plugin run not found; nothing to do")
			return errSkip
		}
		if err != nil {
			return err
		}
		if status == "cancelled" || status == "completed" || status == "failed" {
			return errSkip
		}
		if err := json.Unmarshal(rawInputs, &inputs); err != nil {
			return fmt.Errorf("decode run inputs: %w", err)
		}
		// A retry starts clean: results from an earlier attempt that died
		// before committing cannot exist, but a lost-lease attempt may have
		// committed some; the run is re-executed as a whole.
		if _, err := tx.Exec(ctx, `DELETE FROM plugin_results WHERE run_id = $1`, pl.RunID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE plugin_runs
			SET status = 'running', started_at = now(), completed_at = NULL, error = NULL, job_id = $2
			WHERE id = $1`, pl.RunID, job.ID); err != nil {
			return err
		}
		return progress.Publish(ctx, tx, pl.WorkspaceID, "plugin_run_started",
			map[string]any{"run_id": pl.RunID, "plugin": name, "attempt": job.RetryCount + 1})
	})
	if errors.Is(err, errSkip) {
		return nil
	}
	if err != nil {
		return err
	}

	entry, ok := w.catalog.Get(name)
	if !ok {
		return w.failPermanently(ctx, job, pl, fmt.Sprintf("plugin %q is not installed on this worker", name))
	}
	if err := entry.Plugin.ValidateInputs(inputs); err != nil {
		return w.failPermanently(ctx, job, pl, "invalid inputs: "+err.Error())
	}

	started := time.Now()
	outcome, err := w.runner.Run(ctx, entry.Plugin, inputs, w.progressFunc(ctx, pl))
	if errors.Is(err, ErrUnsupported) || errors.Is(err, declarative.ErrNoExtractor) {
		return w.failPermanently(ctx, job, pl, err.Error())
	}
	if err != nil {
		return err // the queue retries with backoff; Reconcile records the outcome
	}

	stats := map[string]any{
		"records":     len(outcome.Records),
		"pages":       outcome.Pages,
		"cost_usd":    outcome.CostUSD,
		"duration_ms": time.Since(started).Milliseconds(),
	}
	if outcome.Stopped != "" {
		stats["stopped"] = outcome.Stopped
	}
	if outcome.ProviderError != "" {
		stats["provider_error"] = outcome.ProviderError
	}
	return db.WithTenant(ctx, w.pool, pl.WorkspaceID, func(tx pgx.Tx) error {
		if err := queue.HoldLease(ctx, tx, job); err != nil {
			return err
		}
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM plugin_runs WHERE id = $1 FOR UPDATE`, pl.RunID).Scan(&status); err != nil {
			return err
		}
		if status != "running" {
			return nil // cancelled while running; keep the cancellation
		}
		if err := insertResults(ctx, tx, pl, outcome.Records); err != nil {
			return err
		}
		statsJSON, _ := json.Marshal(stats)
		if _, err := tx.Exec(ctx, `UPDATE plugin_runs
			SET status = 'completed', completed_at = now(), stats = $2
			WHERE id = $1`, pl.RunID, statsJSON); err != nil {
			return err
		}
		return progress.Publish(ctx, tx, pl.WorkspaceID, "plugin_run_completed",
			map[string]any{"run_id": pl.RunID, "plugin": name, "stats": stats})
	})
}

// insertResults writes all records in one statement.
func insertResults(ctx context.Context, tx pgx.Tx, pl jobPayload, records []Record) error {
	if len(records) == 0 {
		return nil
	}
	idx := make([]int32, len(records))
	data := make([]string, len(records))
	evidence := make([]string, len(records))
	for i, r := range records {
		d, err := json.Marshal(r.Data)
		if err != nil {
			return fmt.Errorf("encode record %d: %w", i, err)
		}
		e, err := json.Marshal(r.Evidence)
		if err != nil {
			return fmt.Errorf("encode evidence %d: %w", i, err)
		}
		idx[i], data[i], evidence[i] = int32(i), string(d), string(e)
	}
	_, err := tx.Exec(ctx, `INSERT INTO plugin_results (run_id, workspace_id, record_index, data, evidence)
		SELECT $1, $2, i, d::jsonb, e::jsonb
		FROM unnest($3::int[], $4::text[], $5::text[]) AS r(i, d, e)`,
		pl.RunID, pl.WorkspaceID, idx, data, evidence)
	return err
}

// failPermanently records a failure that retrying cannot fix and completes
// the job, so the queue does not spend three backoff cycles on it.
func (w *Worker) failPermanently(ctx context.Context, job queue.Job, pl jobPayload, reason string) error {
	return db.WithTenant(ctx, w.pool, pl.WorkspaceID, func(tx pgx.Tx) error {
		if err := queue.HoldLease(ctx, tx, job); err != nil {
			return err
		}
		return markFailed(ctx, tx, pl, reason)
	})
}

func markFailed(ctx context.Context, tx pgx.Tx, pl jobPayload, reason string) error {
	tag, err := tx.Exec(ctx, `UPDATE plugin_runs
		SET status = 'failed', completed_at = now(), error = $2
		WHERE id = $1 AND status IN ('pending', 'running')`, pl.RunID, reason)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	return progress.Publish(ctx, tx, pl.WorkspaceID, "plugin_run_failed",
		map[string]any{"run_id": pl.RunID, "error": reason})
}

// Reconcile runs after the queue committed a failed attempt (including
// timeouts and lost leases), mirroring the Python failure handlers.
func (w *Worker) Reconcile(ctx context.Context, job queue.Job, reason string, willRetry bool) error {
	var pl jobPayload
	if err := json.Unmarshal(job.Payload, &pl); err != nil || pl.RunID == "" || pl.WorkspaceID == "" {
		return nil
	}
	return db.WithTenant(ctx, w.pool, pl.WorkspaceID, func(tx pgx.Tx) error {
		if !willRetry {
			return markFailed(ctx, tx, pl, reason)
		}
		_, err := tx.Exec(ctx, `UPDATE plugin_runs SET status = 'pending', error = $2
			WHERE id = $1 AND status = 'running'`, pl.RunID, "Retrying: "+reason)
		return err
	})
}

// progressFunc publishes scraper page progress at most twice a second.
// Progress is best effort: a failed publish never fails the run.
func (w *Worker) progressFunc(ctx context.Context, pl jobPayload) func(declarative.Progress) {
	var mu sync.Mutex
	var last time.Time
	return func(p declarative.Progress) {
		mu.Lock()
		defer mu.Unlock()
		if time.Since(last) < 500*time.Millisecond {
			return
		}
		last = time.Now()
		err := db.WithoutTenant(ctx, w.pool, func(tx pgx.Tx) error {
			return progress.Publish(ctx, tx, pl.WorkspaceID, "plugin_run_progress",
				map[string]any{"run_id": pl.RunID, "pages": p.Pages, "records": p.Records})
		})
		if err != nil && ctx.Err() == nil {
			w.log.Debug("progress publish failed", "run_id", pl.RunID, "err", err)
		}
	}
}
