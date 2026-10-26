// Package automationhook ports the enqueue half of the Python automation
// events (services/automations/events.py emit_row_added): when rows are added to
// a workbook it enqueues one `trigger_eval` job per enabled `on_row_added`
// trigger whose scope covers the workbook. The jobs are executed by the Python
// worker (executor "python") exactly as FastAPI would have queued them.
package automationhook

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// Hook is invoked by workbook-row insertion sites.
type Hook interface {
	// RowsAdded enqueues trigger_eval jobs inside tx (which must be a
	// workspace-scoped transaction). Errors are logged and swallowed; the
	// returned count is the number of jobs enqueued.
	RowsAdded(ctx context.Context, tx pgx.Tx, workspaceID, workbookID string, rowIDs []int64) (enqueued int, err error)
	// SignalsInserted is events.emit_signal_matches: for each freshly inserted signal it
	// enqueues one trigger_eval per enabled on_signal rule (fire_key "signal:<pk>"). Same
	// contract as RowsAdded: inside the caller's workspace-scoped tx, in a savepoint, errors
	// logged and swallowed. Callers pass only rows they actually inserted.
	SignalsInserted(ctx context.Context, tx pgx.Tx, workspaceID string, sigs []SignalRef) (enqueued int, err error)
}

// SignalRef is one inserted signal: {signal_pk, signal_type, lead_id} of emit_signal_matches.
// A nil LeadID is skipped (the signal maps to no workbook row).
type SignalRef struct {
	PK, Type string
	LeadID   *int64
}

// New returns a Hook. When enabled is false (AUTOMATIONS_ENABLED off) every
// call returns 0 without touching the database.
func New(enabled bool, log *slog.Logger) Hook {
	if log == nil {
		log = slog.Default()
	}
	return &hook{enabled: enabled, log: log}
}

type hook struct {
	enabled bool
	log     *slog.Logger
}

type target struct {
	WorkbookID string `json:"workbook_id"`
	RowID      string `json:"row_id"`
}

// payload key order mirrors events._enqueue_eval.
type payload struct {
	TriggerID   string   `json:"trigger_id"`
	WorkspaceID string   `json:"workspace_id"`
	Targets     []target `json:"targets"`
	FireSource  string   `json:"fire_source"`
	FireKey     string   `json:"fire_key"`
	DryRun      bool     `json:"dry_run"`
}

func (h *hook) RowsAdded(ctx context.Context, tx pgx.Tx, workspaceID, workbookID string, rowIDs []int64) (int, error) {
	if !h.enabled || len(rowIDs) == 0 {
		return 0, nil
	}
	n, err := h.runInSavepoint(ctx, tx, workspaceID, workbookID, rowIDs)
	if err != nil {
		h.log.Warn("automation row-added hook failed", "workspace_id", workspaceID, "workbook_id", workbookID, "error", err.Error())
		return 0, nil
	}
	return n, nil
}

// runInSavepoint runs the whole hook (rule SELECT + job inserts) in one
// savepoint so any server-side error rolls back to it and leaves the caller's
// transaction usable. On failure nothing is enqueued.
func (h *hook) runInSavepoint(ctx context.Context, tx pgx.Tx, workspaceID, workbookID string, rowIDs []int64) (int, error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return 0, err
	}
	n, err := h.run(ctx, sp, workspaceID, workbookID, rowIDs)
	if err != nil {
		_ = sp.Rollback(context.WithoutCancel(ctx))
		return 0, err
	}
	if err := sp.Commit(ctx); err != nil {
		return 0, err
	}
	return n, nil
}

func (h *hook) run(ctx context.Context, tx pgx.Tx, workspaceID, workbookID string, rowIDs []int64) (int, error) {
	ids := make([]string, len(rowIDs))
	targets := make([]target, len(rowIDs))
	for i, r := range rowIDs {
		ids[i] = strconv.FormatInt(r, 10)
		targets[i] = target{WorkbookID: workbookID, RowID: ids[i]}
	}
	// Python sorts the stringified ids lexicographically, then cuts the joined
	// string at 120 characters (ids are ASCII digits).
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	joined := strings.Join(sorted, ",")
	if len(joined) > 120 {
		joined = joined[:120]
	}
	fireKey := "rowadd:" + workbookID + ":" + joined

	q := gen.New(tx)
	rules, err := q.ListEnabledTriggersByType(ctx, gen.ListEnabledTriggersByTypeParams{
		WorkspaceID: workspaceID, TriggerType: "on_row_added",
	})
	if err != nil {
		return 0, err
	}
	enqueued := 0
	for _, rule := range rules {
		if !covers(rule.ScopeWorkbookIds, workbookID) {
			continue
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(payload{TriggerID: rule.ID, WorkspaceID: workspaceID, Targets: targets,
			FireSource: "row_added", FireKey: fireKey}); err != nil {
			return 0, err
		}
		if err := q.InsertTriggerEvalJob(ctx, gen.InsertTriggerEvalJobParams{
			Payload: bytes.TrimSpace(buf.Bytes()), WorkspaceID: workspaceID,
		}); err != nil {
			return 0, err
		}
		enqueued++
	}
	return enqueued, nil
}

// covers mirrors _rule_covers_workbook: an empty/null scope covers everything.
func covers(scopeJSON []byte, workbookID string) bool {
	var scope []string
	if len(scopeJSON) == 0 || json.Unmarshal(scopeJSON, &scope) != nil || len(scope) == 0 {
		return true
	}
	for _, s := range scope {
		if s == workbookID {
			return true
		}
	}
	return false
}

func (h *hook) SignalsInserted(ctx context.Context, tx pgx.Tx, workspaceID string, sigs []SignalRef) (int, error) {
	if !h.enabled || len(sigs) == 0 {
		return 0, nil
	}
	n, err := h.signalsInSavepoint(ctx, tx, workspaceID, sigs)
	if err != nil {
		h.log.Warn("automation signal hook failed", "workspace_id", workspaceID, "error", err.Error())
		return 0, nil
	}
	return n, nil
}

func (h *hook) signalsInSavepoint(ctx context.Context, tx pgx.Tx, workspaceID string, sigs []SignalRef) (int, error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return 0, err
	}
	n, err := h.signals(ctx, sp, workspaceID, sigs)
	if err != nil {
		_ = sp.Rollback(context.WithoutCancel(ctx))
		return 0, err
	}
	if err := sp.Commit(ctx); err != nil {
		return 0, err
	}
	return n, nil
}

func (h *hook) signals(ctx context.Context, tx pgx.Tx, workspaceID string, sigs []SignalRef) (int, error) {
	q := gen.New(tx)
	rules, err := q.ListEnabledSignalTriggers(ctx, workspaceID)
	if err != nil {
		return 0, err
	}
	if len(rules) == 0 {
		return 0, nil
	}
	enqueued := 0
	for _, sig := range sigs {
		if sig.LeadID == nil {
			continue
		}
		for _, rule := range rules {
			if wanted := signalTypes(rule.TriggerConfig); len(wanted) > 0 && !wanted[sig.Type] {
				continue
			}
			scope := scopeList(rule.ScopeWorkbookIds)
			rows, err := q.SignalRowTargets(ctx, gen.SignalRowTargetsParams{
				WorkspaceID: workspaceID, LeadID: int32(*sig.LeadID), Scope: scope,
			})
			if err != nil {
				return 0, err
			}
			if len(rows) == 0 {
				continue // signal lead in no scoped workbook: no firing
			}
			targets := make([]target, len(rows))
			for i, r := range rows {
				targets[i] = target{WorkbookID: r.WorkbookID, RowID: strconv.FormatInt(int64(r.RowID), 10)}
			}
			var buf bytes.Buffer
			enc := json.NewEncoder(&buf)
			enc.SetEscapeHTML(false)
			if err := enc.Encode(payload{TriggerID: rule.ID, WorkspaceID: workspaceID, Targets: targets,
				FireSource: "signal", FireKey: "signal:" + sig.PK}); err != nil {
				return 0, err
			}
			if err := q.InsertTriggerEvalJob(ctx, gen.InsertTriggerEvalJobParams{
				Payload: bytes.TrimSpace(buf.Bytes()), WorkspaceID: workspaceID,
			}); err != nil {
				return 0, err
			}
			enqueued++
		}
	}
	return enqueued, nil
}

// signalTypes is set((rule.trigger_config or {}).get("signal_types") or []).
func signalTypes(cfg []byte) map[string]bool {
	var c struct {
		SignalTypes []string `json:"signal_types"`
	}
	if len(cfg) == 0 || json.Unmarshal(cfg, &c) != nil {
		return nil
	}
	m := make(map[string]bool, len(c.SignalTypes))
	for _, s := range c.SignalTypes {
		m[s] = true
	}
	return m
}

// scopeList is rule.scope_workbook_ids or []; non-list scopes count as empty.
func scopeList(scopeJSON []byte) []string {
	var scope []string
	if len(scopeJSON) == 0 || json.Unmarshal(scopeJSON, &scope) != nil || scope == nil {
		return []string{}
	}
	return scope
}
