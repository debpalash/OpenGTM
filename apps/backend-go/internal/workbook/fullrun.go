package workbook

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// GO_EXECUTOR_FULL_RUN gates of enrich_cell that run before the "running" broadcast: the
// column condition and the output run-once guard. The in-request single-cell run answers
// both itself (cellrun.go) before it reaches enrichCell.

// preRunGates returns done=true with the cell's outcome when a gate answered it.
func (rs *runState) preRunGates(ctx context.Context, lead map[string]any, col column, rowID, leadID int64) (cellOutcome, bool, error) {
	if rs.cell != nil || !rs.deps.FullRun {
		return cellOutcome{}, false, nil
	}
	if cond := col.cfg["condition"]; manifest.PyTruthy(cond) {
		if !colgraph.EvalConditionCols(cond, rowForColumn(lead), rs.allCols) {
			out, err := rs.skipCell(ctx, col, rowID, leadID)
			return out, true, err
		}
	}
	if col.Type == "output" && !rs.p.Force {
		runOnce := true
		if v, ok := col.cfg["run_once"]; ok {
			runOnce = manifest.PyTruthy(v)
		}
		if runOnce {
			return rs.outputRunOnce(ctx, lead, col, rowID, leadID)
		}
	}
	return cellOutcome{}, false, nil
}

// skipCell writes the `skipped` cell of an unmet condition (fenced) and broadcasts it.
func (rs *runState) skipCell(ctx context.Context, col column, rowID, leadID int64) (cellOutcome, error) {
	err := tenancy.InWorkspace(ctx, rs.deps.Pool, rs.p.WorkspaceID, func(tx pgx.Tx) error {
		if err := fence(ctx, tx, rs.claim, rs.p.WorkbookID); err != nil {
			return err
		}
		return setEnrichment(ctx, tx, cellWrite{WorkspaceID: rs.p.WorkspaceID, WorkbookID: rs.p.WorkbookID,
			LeadID: int32(leadID), RowID: int32(rowID), ColumnID: col.ID, Status: "skipped"})
	})
	if err != nil {
		if a := abortErr(ctx, err); a != nil {
			return cellOutcome{}, a
		}
		return cellOutcome{}, err
	}
	rs.publish(ctx, map[string]any{"type": "cell_update", "leadId": leadID, "rowId": rowID,
		"colId": col.ID, "status": "skipped", "value": nil})
	return cellOutcome{Skipped: true, Err: "condition_not_met"}, nil
}

// outputRunOnce is enrich_cell's run-once guard for a queued run: a complete own cell is
// reused (never re-pushed), an old lead-keyed receipt without an own cell needs review, and a
// row deleted mid-run is not pushed. Nothing is written in any of these cases.
func (rs *runState) outputRunOnce(ctx context.Context, lead map[string]any, col column, rowID, leadID int64) (cellOutcome, bool, error) {
	var (
		priorComplete bool
		own           any
		rowMissing    bool
	)
	_, hasRow := lead["__row_id"]
	err := tenancy.InWorkspace(ctx, rs.deps.Pool, rs.p.WorkspaceID, func(tx pgx.Tx) error {
		q := gen.New(tx)
		prior, err := q.GetEnrichmentForOutputGuard(ctx, gen.GetEnrichmentForOutputGuardParams{
			WorkbookID: rs.p.WorkbookID, WorkspaceID: rs.p.WorkspaceID, LeadID: int32(leadID), ColumnID: col.ID})
		switch {
		case err == nil:
			priorComplete = prior.Status.Valid && prior.Status.String == "complete"
			if !hasRow && priorComplete {
				own = map[string]any{"status": "complete", "value": textValue(prior.Value), "provider": textValue(prior.Provider)}
			}
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		if !hasRow {
			return nil
		}
		cell, err := q.GetRowOverlayCell(ctx, gen.GetRowOverlayCellParams{
			ColumnID: col.ID, RowID: int32(rowID), WorkbookID: rs.p.WorkbookID, WorkspaceID: rs.p.WorkspaceID})
		if errors.Is(err, pgx.ErrNoRows) {
			rowMissing = true
			return nil
		}
		if err != nil {
			return err
		}
		own, err = decodeNumber([]byte(cell))
		if err != nil {
			own = nil
		}
		return nil
	})
	if err != nil {
		return cellOutcome{}, true, err
	}
	if rowMissing {
		return cellOutcome{Err: "output_row_missing"}, true, nil
	}
	if m, ok := own.(map[string]any); ok && m["status"] == "complete" {
		prov, _ := m["provider"].(string)
		return cellOutcome{Success: true, Raw: m["value"], HasRaw: true, Value: manifest.PyStr(m["value"]), Provider: prov}, true, nil
	}
	if own == nil && priorComplete && hasRow {
		return cellOutcome{Err: "output_identity_requires_review"}, true, nil
	}
	return cellOutcome{}, false, nil
}

// verifyEmailCell is the auto-verify block of enrich_cell: best effort, never fails the cell.
func (rs *runState) verifyEmailCell(ctx context.Context, col column, value string) map[string]any {
	if value == "" || !col.needsVerify() || !rs.deps.FullRun || rs.deps.Verify == nil {
		return nil
	}
	vr, err := rs.deps.Verify.Verify(ctx, rs.p.WorkspaceID, value)
	if err != nil {
		rs.log.Debug("email verify failed", "col", col.ID, "err", err)
		return nil
	}
	return map[string]any{"status": vr.Status, "confidence": vr.Confidence, "source": vr.Source}
}
