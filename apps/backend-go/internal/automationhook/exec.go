package automationhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// CohortExecutor is go_executor.trigger_eval_executor: "go" only when GO_EXECUTOR_ENABLED,
// the per-job-kind flag (kindEnabled: GO_EXECUTOR_TRIGGER_EVAL or GO_EXECUTOR_WATCH_POLL) and the workspace is in
// GO_EXECUTOR_WORKSPACES (workspaces); otherwise "python".
// Legacy-outreach / non-Go re-enrich hand-back is decided later by the Go worker.
func CohortExecutor(enabled, kindEnabled bool, workspaces map[string]bool) func(ws string) string {
	return func(ws string) string {
		if enabled && kindEnabled && (workspaces["*"] || workspaces[ws]) {
			return "go"
		}
		return "python"
	}
}

func (h *hook) executor(ws string) string {
	if h.exec != nil && h.exec(ws) == "go" {
		return "go"
	}
	return "python"
}

// guarded runs fn in a savepoint of the caller's transaction. A failure rolls the savepoint back
// (the caller's tx stays usable), is logged without secrets, swallowed, and yields 0.
func (h *hook) guarded(ctx context.Context, tx pgx.Tx, label, ws string, fn func(sp pgx.Tx) (int, error)) int {
	n, err := func() (n int, err error) {
		sp, err := tx.Begin(ctx)
		if err != nil {
			return 0, err
		}
		done := false
		defer func() {
			if r := recover(); r != nil {
				// The value may carry row data: log only its type.
				n, err = 0, fmt.Errorf("panic in rule evaluation (%T)", r)
			}
			if done {
				return
			}
			if rbErr := sp.Rollback(context.WithoutCancel(ctx)); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
				h.log.Warn("automation "+label+" hook savepoint rollback failed", "workspace_id", ws, "error", rbErr.Error())
			}
		}()
		n, err = fn(sp)
		if err != nil {
			return 0, err
		}
		if err := sp.Commit(ctx); err != nil {
			return 0, err
		}
		done = true
		return n, nil
	}()
	if err != nil {
		h.log.Warn("automation "+label+" hook failed", "workspace_id", ws, "error", err.Error())
		return 0
	}
	return n
}

// enqueue is events._enqueue_eval: one pending trigger_eval job, payload key order as Python.
func (h *hook) enqueue(ctx context.Context, q *gen.Queries, ws, triggerID string, targets []target, source, fireKey string) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload{TriggerID: triggerID, WorkspaceID: ws, Targets: targets,
		FireSource: source, FireKey: fireKey}); err != nil {
		return err
	}
	return q.InsertTriggerEvalJobFor(ctx, gen.InsertTriggerEvalJobForParams{
		Executor: h.executor(ws), Payload: bytes.TrimSpace(buf.Bytes()), WorkspaceID: ws,
	})
}
