package audiencerefresh

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/server/internal/jobs/jobkit"
	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
)

// evalJobType is the Python-executed job a membership event fires.
const evalJobType = "trigger_eval"

type membershipEvent struct {
	id       int64
	lead     int64
	kind     string // "entered" | "exited"
	audience string
}

type rule struct {
	id     string
	config any
	scope  any
}

// emitRefreshEvents ports _emit_refresh_events: the events of one refresh are
// emitted in keyset pages of PageSize. Everything is a no-op while automations
// are disabled, exactly as emit_audience_membership is.
func (w *Worker) emitRefreshEvents(ctx context.Context, tx pgx.Tx, workspaceID, audienceID, refreshID string, size int) error {
	if !w.opts.AutomationsEnabled {
		return nil
	}
	return jobkit.KeysetPages(ctx, size,
		func(ctx context.Context, after int64, limit int) ([]membershipEvent, error) {
			rows, err := tx.Query(ctx, `SELECT id, lead_id, event_type, audience_id FROM audience_membership_events
				WHERE workspace_id = $1 AND audience_id = $2 AND refresh_id = $3 AND id > $4 ORDER BY id LIMIT $5`,
				workspaceID, audienceID, refreshID, after, limit)
			if err != nil {
				return nil, err
			}
			return pgx.CollectRows(rows, func(r pgx.CollectableRow) (e membershipEvent, err error) {
				err = r.Scan(&e.id, &e.lead, &e.kind, &e.audience)
				return
			})
		},
		func(e membershipEvent) int64 { return e.id },
		func(page []membershipEvent) error {
			_, err := w.emitMembership(ctx, tx, workspaceID, page)
			return err
		})
}

// errMalformedRule stands for the Python exception (AttributeError, TypeError
// ...) that emit_audience_membership swallows when a rule's stored JSON has an
// unexpected shape: it abandons the rest of that page of events.
var errMalformedRule = errors.New("malformed automation rule")

// emitMembership ports emit_audience_membership: for each entered/exited event
// it enqueues one trigger_eval job per enabled on_audience_enter /
// on_audience_exit rule that covers the audience, targeting the workbook rows
// linked to the lead (within the rule's workbook scope). Events whose lead is
// in no scoped workbook fire nothing.
func (w *Worker) emitMembership(ctx context.Context, tx pgx.Tx, workspaceID string, events []membershipEvent) (int, error) {
	if !w.opts.AutomationsEnabled || len(events) == 0 {
		return 0, nil
	}
	rulesByKind := map[string][]rule{}
	for kind, triggerType := range map[string]string{"entered": "on_audience_enter", "exited": "on_audience_exit"} {
		rules, err := enabledRules(ctx, tx, workspaceID, triggerType)
		if err != nil {
			return 0, err
		}
		rulesByKind[kind] = rules
	}
	enqueued := 0
	for _, event := range events {
		for _, r := range rulesByKind[event.kind] {
			n, err := w.fire(ctx, tx, workspaceID, r, event)
			enqueued += n
			if errors.Is(err, errMalformedRule) {
				w.log.Warn("emit_audience_membership abandoned: malformed rule", "rule_id", r.id)
				return enqueued, nil
			}
			if err != nil {
				return enqueued, err
			}
		}
	}
	return enqueued, nil
}

func enabledRules(ctx context.Context, tx pgx.Tx, workspaceID, triggerType string) ([]rule, error) {
	rows, err := tx.Query(ctx, `SELECT id, trigger_config::text, scope_workbook_ids::text FROM triggers
		WHERE workspace_id = $1 AND trigger_type = $2 AND enabled IS TRUE ORDER BY created_at, id`, workspaceID, triggerType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []rule
	for rows.Next() {
		var id string
		var cfg, scope *string
		if err := rows.Scan(&id, &cfg, &scope); err != nil {
			return nil, err
		}
		r := rule{id: id}
		for _, f := range []struct {
			text *string
			dst  *any
		}{{cfg, &r.config}, {scope, &r.scope}} {
			if f.text == nil {
				continue
			}
			if *f.dst, err = jobkit.Decode([]byte(*f.text)); err != nil {
				return nil, err
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type evalTarget struct {
	WorkbookID string `json:"workbook_id"`
	RowID      string `json:"row_id"`
}

type evalPayload struct {
	TriggerID   string       `json:"trigger_id"`
	WorkspaceID string       `json:"workspace_id"`
	Targets     []evalTarget `json:"targets"`
	FireSource  string       `json:"fire_source"`
	FireKey     string       `json:"fire_key"`
	DryRun      bool         `json:"dry_run"`
}

func (w *Worker) fire(ctx context.Context, tx pgx.Tx, workspaceID string, r rule, event membershipEvent) (int, error) {
	// `(rule.trigger_config or {}).get("audience_ids") or []`
	config := r.config
	if !jobkit.Truthy(config) {
		config = &jobkit.Object{Vals: map[string]any{}}
	}
	cfg, ok := config.(*jobkit.Object)
	if !ok {
		return 0, errMalformedRule
	}
	audienceIDs, err := pySet(cfg.Vals["audience_ids"])
	if err != nil {
		return 0, errMalformedRule
	}
	if len(audienceIDs) > 0 && !slices.Contains(audienceIDs, event.audience) {
		return 0, nil
	}
	var scope []string
	if jobkit.Truthy(r.scope) {
		items, ok := r.scope.([]any)
		if !ok {
			return 0, errMalformedRule
		}
		for _, it := range items {
			s, ok := it.(string)
			if !ok {
				return 0, errMalformedRule
			}
			scope = append(scope, s)
		}
	}
	sql := `SELECT wr.id, wr.workbook_id FROM workbook_rows wr JOIN workbooks wb ON wb.id = wr.workbook_id
		WHERE wb.workspace_id = $1 AND wr.lead_id = $2`
	args := []any{workspaceID, event.lead}
	if len(scope) > 0 {
		sql += " AND wr.workbook_id = ANY($3::text[])"
		args = append(args, scope)
	}
	rows, err := tx.Query(ctx, sql+" ORDER BY wr.id", args...)
	if err != nil {
		return 0, err
	}
	var targets []evalTarget
	for rows.Next() {
		var rowID int64
		var workbookID string
		if err := rows.Scan(&rowID, &workbookID); err != nil {
			rows.Close()
			return 0, err
		}
		targets = append(targets, evalTarget{WorkbookID: workbookID, RowID: fmt.Sprint(rowID)})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(targets) == 0 {
		return 0, nil // the lead is in no scoped workbook: nothing to fire
	}
	_, err = queue.Enqueue(ctx, tx, evalJobType, evalPayload{
		TriggerID: r.id, WorkspaceID: workspaceID, Targets: targets,
		FireSource: "audience_" + event.kind,
		FireKey:    fmt.Sprintf("audience:%d:%s", event.id, event.kind),
	}, queue.EnqueueOptions{})
	if err != nil {
		return 0, err
	}
	return 1, nil
}

// pySet is set(v or []) for the membership of a decoded JSON value: a list
// contributes its items, an object its keys and a string its characters;
// other truthy values are not iterable.
func pySet(v any) ([]string, error) {
	if !jobkit.Truthy(v) {
		return nil, nil
	}
	var out []string
	switch t := v.(type) {
	case []any:
		for _, it := range t {
			// Only strings can equal an audience id; other items never match.
			if s, ok := it.(string); ok {
				out = append(out, s)
			} else {
				out = append(out, "\x00non-string")
			}
		}
	case *jobkit.Object:
		out = append(out, t.Keys...)
	case string:
		for _, r := range t {
			out = append(out, string(r))
		}
	default:
		return nil, errMalformedRule
	}
	return out, nil
}
