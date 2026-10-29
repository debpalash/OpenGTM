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
// ErrUnsupported means this worker lacks the seam the source kind needs; the worker fails the job permanently.
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
	res, err := Materialize(WithJobKey(ctx, fmt.Sprintf("source_workbook:%d", c.ID)), d, ws, p.WorkbookID, p.ColumnID)
	if err != nil {
		return err
	}
	if raw, merr := res.MarshalJSON(); merr == nil {
		d.log().Info("source_workbook done", "job_id", c.ID, "result", truncRunes(string(raw), 500))
	}
	if !truthy(p.EnrichAfter) || !shouldChainRun(res) {
		return nil
	}
	if err := d.chainRun(ctx, ws, p.WorkbookID, c.ID); err != nil { // Python logs and moves on: the sourcing itself succeeded
		d.log().Warn("source_workbook: failed to chain run_workbook", "job_id", c.ID, "err", err)
	}
	return nil
}

// shouldChainRun: rows were added, or (a redelivery after an attempt that committed them and died before chaining)
// they already exist and were skipped as duplicates.
func shouldChainRun(res interface{ Get(string) (any, bool) }) bool {
	return truthy(get(res, "added")) || truthy(get(res, "skipped"))
}

// chainRun enqueues the run_workbook follow-up of enrich_after exactly once per source job: the row carries the fire
// key source_wb_run:<jobID>, and an existing row with that key (queued, running or finished) means a previous attempt
// already chained, so a redelivered attempt never chains a second run.
func (d Deps) chainRun(ctx context.Context, ws, wbID string, jobID int64) error {
	return tenancy.InWorkspace(ctx, d.Pool, ws, func(tx pgx.Tx) error {
		if d.EnqueueRun != nil {
			return d.EnqueueRun(ctx, tx, ws, wbID)
		}
		key := fmt.Sprintf("source_wb_run:%d", jobID)
		used, err := gen.New(tx).SWFireKeyUsed(ctx, gen.SWFireKeyUsedParams{FireKey: key, WorkspaceID: ws})
		if err != nil || used {
			return err
		}
		// The key goes on the jobs.fire_key column only, so the payload stays byte-equal to Python's.
		_, err = queue.Enqueue(ctx, tx, queue.JobTypeRunWorkbook, ws, runPayload{WorkbookID: wbID, WorkspaceID: ws}, key)
		return err
	})
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

// Enqueue inserts a source_workbook job (queue_service.add_job's row) for executor "go", the only executor since
// the plan 15 cutover (any other executor is refused); q may be the caller's transaction.
func Enqueue(ctx context.Context, q gen.DBTX, ws, wbID, colID, executor string) (int64, error) {
	if ws == "" { // the worker derives the tenant from jobs.workspace_id only
		return 0, errors.New("sourcewb: enqueue needs a workspace")
	}
	if executor != "go" {
		return 0, fmt.Errorf("sourcewb: executor %q must be go (the only executor since the plan 15 cutover)", executor)
	}
	p := JobPayload{WorkbookID: wbID, ColumnID: colID, WorkspaceID: ws}
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
