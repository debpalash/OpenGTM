package playbooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

const errReconcilePayload = "playbook failure payload requires workspace_id and run_id"

// ReconcileFailure is reconcile_playbook_job_failure, the queue failure hook of
// research_playbook_run: a retry puts the run and its running results back to
// pending; a final failure fails the run and its pending/running results and
// recomputes the counts. A run that is gone, of another workspace, completed,
// completed_with_errors or cancelled is left alone, and so is a final failure
// of a run that is already failed (F18: no re-processing of the first error,
// finished_at and counts). A retry of a failed run still puts it back to
// pending so the retried job can run it.
func ReconcileFailure(ctx context.Context, d Deps, jobID int64, payload []byte, errMsg string, willRetry bool) error {
	_ = jobID
	var p map[string]any
	dec := json.NewDecoder(strings.NewReader(string(payload)))
	dec.UseNumber()
	if err := dec.Decode(&p); err != nil {
		p = nil
	}
	ws, runID := pyStrip(valueText(p["workspace_id"])), pyStrip(valueText(p["run_id"]))
	if ws == "" || runID == "" {
		return errors.New(errReconcilePayload)
	}
	msg := errMsg
	if msg == "" {
		msg = "playbook attempt failed"
	}
	msg = dbText(capRunes(msg, maxErrorChars))
	return tenancy.InWorkspace(ctx, d.Pool, ws, func(tx pgx.Tx) error {
		q := gen.New(tx)
		row, err := q.PbEngLockRun(ctx, gen.PbEngLockRunParams{WorkspaceID: ws, ID: runID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("playbooks: reconcile load: %w", err)
		}
		switch row.Status {
		case "completed", "completed_with_errors", "cancelled":
			return nil
		case "failed":
			if !willRetry {
				return nil
			}
		}
		if willRetry {
			text := "Queue retry scheduled: " + msg
			if err := q.PbEngReconcileRetryRun(ctx, gen.PbEngReconcileRetryRunParams{WorkspaceID: ws, ID: runID, Error: text}); err != nil {
				return fmt.Errorf("playbooks: reconcile retry run: %w", err)
			}
			if err := q.PbEngReconcileRetryResults(ctx, gen.PbEngReconcileRetryResultsParams{WorkspaceID: ws, RunID: runID, Error: text}); err != nil {
				return fmt.Errorf("playbooks: reconcile retry results: %w", err)
			}
			return nil
		}
		text := "Final failure: " + msg
		if err := q.PbEngReconcileFailResults(ctx, gen.PbEngReconcileFailResultsParams{WorkspaceID: ws, RunID: runID, Error: text}); err != nil {
			return fmt.Errorf("playbooks: reconcile fail results: %w", err)
		}
		c, err := q.PbResultCounts(ctx, gen.PbResultCountsParams{WorkspaceID: ws, RunID: runID})
		if err != nil {
			return fmt.Errorf("playbooks: reconcile counts: %w", err)
		}
		if err := q.PbEngReconcileFailRun(ctx, gen.PbEngReconcileFailRunParams{
			WorkspaceID: ws, ID: runID, Error: text, Attempted: c.Attempted, Succeeded: c.Succeeded, Failed: c.Failed,
		}); err != nil {
			return fmt.Errorf("playbooks: reconcile fail run: %w", err)
		}
		return nil
	})
}
