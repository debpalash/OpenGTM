package sourcewb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// JobType is the queue job type.
const JobType = "source_workbook"

// JobTimeout is JOB_TIMEOUTS["source_workbook"].
const JobTimeout = 1800 * time.Second

type payload struct {
	WorkbookID  string `json:"workbook_id"`
	ColumnID    string `json:"column_id"`
	WorkspaceID string `json:"workspace_id"`
	EnrichAfter any    `json:"enrich_after"`
}

// Handle is handle_source_workbook: the tenant comes only from the claim (jobs.workspace_id;
// a job without one, or whose payload names another workspace, is refused permanently), the source column is materialized, and with enrich_after a run_workbook job
// is chained when rows were added. Run errors are data (logged, the job completes);
// ErrUnsupported means the job must be handed back to Python.
func Handle(ctx context.Context, d Deps, c *queue.Claim) error {
	var p payload
	if err := json.Unmarshal(c.Payload, &p); err != nil {
		return &queue.PermanentError{Err: fmt.Errorf("source_workbook: payload: %w", err)}
	}
	ws := c.WorkspaceID
	if ws == "" {
		return &queue.PermanentError{Err: errors.New("source_workbook: job workspace missing")}
	}
	if p.WorkspaceID != "" && p.WorkspaceID != ws {
		return &queue.PermanentError{Err: errors.New("source_workbook: payload workspace_id does not match the job's workspace")}
	}
	if p.WorkbookID == "" || p.ColumnID == "" {
		return &queue.PermanentError{Err: errors.New("source_workbook: workbook_id and column_id are required")}
	}
	res, err := Materialize(ctx, d, ws, p.WorkbookID, p.ColumnID)
	if err != nil {
		return err
	}
	if raw, merr := res.MarshalJSON(); merr == nil {
		d.log().Info("source_workbook done", "job_id", c.ID, "result", truncRunes(string(raw), 500))
	}
	if !truthy(p.EnrichAfter) || !truthy(get(res, "added")) {
		return nil
	}
	err = tenancy.InWorkspace(ctx, d.Pool, ws, func(tx pgx.Tx) error {
		if d.EnqueueRun != nil {
			return d.EnqueueRun(ctx, tx, ws, p.WorkbookID)
		}
		_, err := queue.Enqueue(ctx, tx, queue.JobTypeRunWorkbook, ws,
			runPayload{WorkbookID: p.WorkbookID, WorkspaceID: ws}, "")
		return err
	})
	if err != nil { // Python logs and moves on: the sourcing itself succeeded
		d.log().Warn("source_workbook: failed to chain run_workbook", "job_id", c.ID, "err", err)
	}
	return nil
}

type runPayload struct {
	WorkbookID  string `json:"workbook_id"`
	WorkspaceID string `json:"workspace_id"`
}

// JobPayload is the source_workbook payload (Python add_job key order).
type JobPayload struct {
	WorkbookID  string `json:"workbook_id"`
	ColumnID    string `json:"column_id"`
	WorkspaceID string `json:"workspace_id"`
}

// Enqueue inserts a source_workbook job for executor "python" (queue_service.add_job,
// unchanged) or "go" (same row with executor 'go'); q may be the caller's transaction.
func Enqueue(ctx context.Context, q gen.DBTX, ws, wbID, colID, executor string) (int64, error) {
	if ws == "" { // the worker derives the tenant from jobs.workspace_id only
		return 0, errors.New("sourcewb: enqueue needs a workspace")
	}
	p := JobPayload{WorkbookID: wbID, ColumnID: colID, WorkspaceID: ws}
	if executor != "go" {
		return queue.Enqueue(ctx, q, JobType, ws, p, "")
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return 0, err
	}
	id, err := gen.New(q).SWEnqueueJob(ctx, gen.SWEnqueueJobParams{Executor: "go", Payload: raw, WorkspaceID: ws})
	if err != nil {
		return 0, fmt.Errorf("sourcewb: enqueue: %w", err)
	}
	return int64(id), nil
}
