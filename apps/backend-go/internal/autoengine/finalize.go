package autoengine

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autoschedule"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/rowtmpl"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// deriveStatus is the run status of handle_trigger_eval's finalisation.
func deriveStatus(dry bool, st stats) string {
	switch {
	case dry:
		return "dry_run"
	case st.failed > 0 && st.succeeded > 0:
		return "partial"
	case st.failed > 0:
		if st.attempted > 0 {
			return "failed"
		}
		return "completed"
	case st.skipped > 0 && st.succeeded == 0 && st.attempted > 0:
		return "partial"
	}
	return "completed"
}

// round4 is Python round(x, 4) (half to even on the binary value).
func round4(x float64) float64 {
	v, _ := strconv.ParseFloat(strconv.FormatFloat(x, 'f', 4, 64), 64)
	return v
}

// activityMessage is _log_activity's message.
func activityMessage(name, status string, matched int, st stats, total float64) string {
	return fmt.Sprintf("automation '%s' %s: %d matched, %d ok, %d skipped, %d failed, $%s",
		name, status, matched, st.succeeded, st.skipped, st.failed, rowtmpl.PyStr(round4(total)))
}

// finalize commits the run's counters and status, stamps last_fired_at (not for
// a dry run), writes the best-effort activity rows and, for a schedule fire,
// re-enqueues the next aligned eval (single-flight).
func (e *engine) finalize(ctx context.Context, run runInfo, fireSource string, dry bool, matched int, st stats, cands []candidate) error {
	d := e.d
	ws := run.WorkspaceID
	total := round4(st.charged)
	status := deriveStatus(dry, st)
	if err := tenancy.InWorkspace(ctx, d.Pool, ws, func(tx pgx.Tx) error {
		q := gen.New(tx)
		if _, err := q.AutoRunFinishRun(ctx, gen.AutoRunFinishRunParams{ID: run.ID, WorkspaceID: ws, Status: status,
			MatchedRows: int32(matched), ActionsAttempted: int32(st.attempted), ActionsSucceeded: int32(st.succeeded),
			ActionsSkipped: int32(st.skipped), ActionsFailed: int32(st.failed), TotalChargedUsd: total}); err != nil {
			return err
		}
		if !dry {
			return q.AutoRunSetLastFired(ctx, gen.AutoRunSetLastFiredParams{ID: run.Trigger.ID, WorkspaceID: ws})
		}
		return nil
	}); err != nil {
		return err
	}

	if err := e.logActivity(ctx, run, status, matched, st, total, cands); err != nil {
		d.log().Debug("activity log failed", "err", err)
	}

	if !dry && fireSource == "schedule" {
		if err := e.reschedule(ctx, ws, run.Trigger.ID); err != nil {
			d.log().Warn("reschedule failed", "trigger_id", run.Trigger.ID, "err", err)
		}
	}
	return nil
}

// logActivity writes one workbook activity row per distinct candidate
// workbook (best effort; ordered for determinism).
func (e *engine) logActivity(ctx context.Context, run runInfo, status string, matched int, st stats, total float64, cands []candidate) error {
	seen := map[string]bool{}
	var wbs []string
	for _, c := range cands {
		if !seen[c.workbookID] {
			seen[c.workbookID] = true
			wbs = append(wbs, c.workbookID)
		}
	}
	if len(wbs) == 0 {
		return nil
	}
	sort.Strings(wbs)
	msg := activityMessage(run.Trigger.Name, status, matched, st, total)
	return tenancy.InWorkspace(ctx, e.d.Pool, run.WorkspaceID, func(tx pgx.Tx) error {
		q := gen.New(tx)
		for _, wb := range wbs {
			if err := q.AutoRunInsertActivity(ctx, gen.AutoRunInsertActivityParams{WorkbookID: wb, WorkspaceID: run.WorkspaceID, Message: msg}); err != nil {
				return err
			}
		}
		return nil
	})
}

// reschedule is _reschedule: after commit, in its own transaction, enqueue the
// next aligned eval of an enabled on_schedule rule. The advisory lock and the
// unique active fire key in autoschedule make it single-flight across Go and
// Python.
func (e *engine) reschedule(ctx context.Context, ws, triggerID string) error {
	executor := "python"
	if e.d.ExecFor != nil {
		executor = e.d.ExecFor(ws)
	}
	return tenancy.InWorkspace(ctx, e.d.Pool, ws, func(tx pgx.Tx) error {
		t, ok, err := autoschedule.LoadTrigger(ctx, tx, triggerID, ws)
		if err != nil || !ok || !t.Enabled || t.Type != "on_schedule" {
			return err
		}
		_, err = autoschedule.BootstrapForTrigger(ctx, tx, t, e.d.now(), executor)
		return err
	})
}
