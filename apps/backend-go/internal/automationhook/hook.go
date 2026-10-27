// Package automationhook ports the enqueue half of the Python automation
// events (services/automations/events.py emit_row_added): when rows are added to
// a workbook it enqueues one `trigger_eval` job per enabled `on_row_added`
// trigger whose scope covers the workbook, in the shape FastAPI queued them. Since the plan 15 cutover the jobs
// are executor go.
package automationhook

import (
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

// Emitter is the full emitter set: Hook plus on_rows_changed and audience membership. It is a
// separate interface so existing Hook fakes keep compiling.
type Emitter interface {
	Hook
	// RowsChanged is events.on_rows_changed: one trigger_eval per (on_row_changed rule, changed row).
	RowsChanged(ctx context.Context, tx pgx.Tx, workspaceID, workbookID string, changes []Change) (enqueued int, err error)
	// AudienceEvents is events.emit_audience_membership.
	AudienceEvents(ctx context.Context, tx pgx.Tx, workspaceID string, evs []AudienceEvent) (enqueued int, err error)
}

// SignalRef is one inserted signal: {signal_pk, signal_type, lead_id} of emit_signal_matches.
// A nil LeadID is skipped (the signal maps to no workbook row).
type SignalRef struct {
	PK, Type string
	LeadID   *int64
}

// DefaultMaxTargets is INTENT_POLLER_MAX_FIRES_PER_SIGNAL's default (D4 fan-out cap).
const DefaultMaxTargets = 200

// New returns a Hook that enqueues for the go executor. When enabled is false
// (AUTOMATIONS_ENABLED off) every call returns 0 without touching the database.
func New(enabled bool, log *slog.Logger) Hook {
	return NewWithOptions(enabled, nil, log, DefaultMaxTargets)
}

// NewWithExecutor is New with go_executor.trigger_eval_executor: exec(ws) names the executor
// column of each inserted job. Since the plan 15 cutover every job is "go" and exec is not consulted.
func NewWithExecutor(enabled bool, exec func(ws string) string, log *slog.Logger) Emitter {
	return NewWithOptions(enabled, exec, log, DefaultMaxTargets)
}

// NewWithOptions also sets the per-job target fan-out cap (INTENT_POLLER_MAX_FIRES_PER_SIGNAL).
func NewWithOptions(enabled bool, exec func(ws string) string, log *slog.Logger, maxTargets int) Emitter {
	if log == nil {
		log = slog.Default()
	}
	if maxTargets < 1 {
		maxTargets = DefaultMaxTargets
	}
	return &hook{enabled: enabled, exec: exec, maxTargets: maxTargets, log: log}
}

type hook struct {
	enabled    bool
	exec       func(ws string) string
	maxTargets int
	log        *slog.Logger
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
	return h.guarded(ctx, tx, "row-added", workspaceID, func(sp pgx.Tx) (int, error) {
		return h.run(ctx, sp, workspaceID, workbookID, rowIDs)
	}), nil
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
		if err := h.enqueue(ctx, q, workspaceID, rule.ID, targets, "row_added", fireKey); err != nil {
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

// scopeList is rule.scope_workbook_ids or []; non-list scopes count as empty.
func scopeList(scopeJSON []byte) []string {
	var scope []string
	if len(scopeJSON) == 0 || json.Unmarshal(scopeJSON, &scope) != nil || scope == nil {
		return []string{}
	}
	return scope
}
