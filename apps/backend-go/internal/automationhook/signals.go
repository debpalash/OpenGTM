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
		if sig.LeadID == nil && sig.CompanyID == "" && sig.PersonID == "" {
			continue
		}
		for _, r := range rules {
			if len(r.wanted) > 0 && !r.wanted[sig.Type] {
				continue
			}
			targets, err := h.subjectTargets(ctx, tx, workspaceID, signalLeadID(sig), sig.CompanyID, sig.PersonID, r.scope)
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

func signalLeadID(s SignalRef) int64 {
	if s.LeadID == nil {
		return 0
	}
	return *s.LeadID
}
func (h *hook) subjectTargets(ctx context.Context, tx pgx.Tx, ws string, leadID int64, cid, pid string, scope []string) ([]target, error) {
	if cid == "" && pid == "" {
		return h.rowTargets(ctx, gen.New(tx), ws, leadID, scope)
	}
	rows, e := tx.Query(ctx, `SELECT workbook_id,id FROM record_workbook_rows WHERE workspace_id=$1 AND (($3<>'' AND person_entity_id=$3) OR ($3='' AND company_entity_id=$2)) AND (cardinality($4::text[])=0 OR workbook_id=ANY($4::text[])) ORDER BY id LIMIT $5`, ws, cid, pid, scope, h.maxTargets)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []target{}
	for rows.Next() {
		var w string
		var id int64
		if e = rows.Scan(&w, &id); e != nil {
			return nil, e
		}
		out = append(out, target{WorkbookID: w, RowID: strconv.FormatInt(id, 10)})
	}
	return out, rows.Err()
}
