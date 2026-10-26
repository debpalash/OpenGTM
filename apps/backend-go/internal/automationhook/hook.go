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
	enqueued := 0
	if err := h.run(ctx, tx, workspaceID, workbookID, rowIDs, &enqueued); err != nil {
		h.log.Warn("automation row-added hook failed", "workspace_id", workspaceID, "workbook_id", workbookID, "error", err.Error())
	}
	return enqueued, nil
}

func (h *hook) run(ctx context.Context, tx pgx.Tx, workspaceID, workbookID string, rowIDs []int64, enqueued *int) error {
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
		return err
	}
	for _, rule := range rules {
		if !covers(rule.ScopeWorkbookIds, workbookID) {
			continue
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(payload{TriggerID: rule.ID, WorkspaceID: workspaceID, Targets: targets,
			FireSource: "row_added", FireKey: fireKey}); err != nil {
			return err
		}
		// Savepoint: a failed insert must not abort the caller's transaction.
		sp, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		if err := gen.New(sp).InsertTriggerEvalJob(ctx, gen.InsertTriggerEvalJobParams{
			Payload: bytes.TrimSpace(buf.Bytes()), WorkspaceID: workspaceID,
		}); err != nil {
			_ = sp.Rollback(context.WithoutCancel(ctx))
			return err
		}
		if err := sp.Commit(ctx); err != nil {
			return err
		}
		*enqueued++
	}
	return nil
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
