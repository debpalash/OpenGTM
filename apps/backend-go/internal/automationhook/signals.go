package automationhook

import (
	"context"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// SignalsInserted is events.emit_signal_matches (callers pass only signals they inserted).
// A rule whose signal_types config is malformed fires nothing (fail closed).
func (h *hook) SignalsInserted(ctx context.Context, tx pgx.Tx, workspaceID string, sigs []SignalRef) (int, error) {
	if !h.enabled || len(sigs) == 0 {
		return 0, nil
	}
	return h.guarded(ctx, tx, "signal", workspaceID, func(sp pgx.Tx) (int, error) {
		return h.signals(ctx, sp, workspaceID, sigs)
	}), nil
}

func (h *hook) signals(ctx context.Context, tx pgx.Tx, workspaceID string, sigs []SignalRef) (int, error) {
	q := gen.New(tx)
	all, err := q.ListEnabledSignalTriggers(ctx, workspaceID)
	if err != nil {
		return 0, err
	}
	type rule struct {
		id     string
		wanted map[string]bool
		scope  []string
	}
	var rules []rule
	for _, r := range all {
		wanted, ok := configSet(r.TriggerConfig, "signal_types")
		if !ok {
			h.log.Warn("automation rule skipped: malformed signal_types", "workspace_id", workspaceID, "trigger_id", r.ID)
			continue
		}
		rules = append(rules, rule{r.ID, wanted, scopeList(r.ScopeWorkbookIds)})
	}
	n := 0
	for _, sig := range sigs {
		if sig.LeadID == nil {
			continue
		}
		for _, r := range rules {
			if len(r.wanted) > 0 && !r.wanted[sig.Type] {
				continue
			}
			targets, err := h.rowTargets(ctx, q, workspaceID, *sig.LeadID, r.scope)
			if err != nil {
				return 0, err
			}
			if len(targets) == 0 {
				continue // signal lead in no scoped workbook: no firing
			}
			if err := h.enqueue(ctx, q, workspaceID, r.id, targets, "signal", "signal:"+sig.PK); err != nil {
				return 0, err
			}
			n++
		}
	}
	return n, nil
}

// rowTargets maps a lead to its workbook rows (inside scope), bounded by the fan-out cap.
func (h *hook) rowTargets(ctx context.Context, q *gen.Queries, ws string, leadID int64, scope []string) ([]target, error) {
	if leadID < -1<<31 || leadID > 1<<31-1 {
		return nil, nil
	}
	rows, err := q.AudienceRowTargets(ctx, gen.AudienceRowTargetsParams{
		WorkspaceID: ws, LeadID: int32(leadID), Scope: scope, MaxRows: int32(h.maxTargets)})
	if err != nil {
		return nil, err
	}
	out := make([]target, len(rows))
	for i, r := range rows {
		out[i] = target{WorkbookID: r.WorkbookID, RowID: strconv.FormatInt(int64(r.RowID), 10)}
	}
	return out, nil
}
