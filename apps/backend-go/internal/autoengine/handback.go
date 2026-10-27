package autoengine

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autoactions"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// legacyOutreach are the actions that need the outreach store and SMTP (D2):
// the Go worker never runs them.
var legacyOutreach = map[string]bool{"sequencer": true, "send_email": true}

// handBackReason decides, before any write, whether Python must run this eval
// (D2, D8): a rule with a legacy outreach action, or a re_enrich of a column the
// Go workbook runner cannot run. It returns "" when Go may run it. Mixed runs
// are never split.
func (e *engine) handBackReason(ctx context.Context, ws string, actions []map[string]any, scope []string, targets any, maxRows int) (string, error) {
	var reIDs map[string]bool
	for _, a := range actions {
		if a == nil {
			continue
		}
		t, _ := a["type"].(string)
		if legacyOutreach[t] {
			return "legacy_outreach_action: " + t, nil
		}
		if t == "re_enrich" {
			if reIDs == nil {
				reIDs = map[string]bool{}
			}
			ids, _ := autoactionsConfig(a)["column_ids"].([]any)
			for _, v := range ids {
				if s, ok := v.(string); ok {
					reIDs[s] = true
				}
			}
		}
	}
	if len(reIDs) == 0 {
		return "", nil
	}
	// The workbooks the eval touches: the targets' or the scoped sweep. Go drops an
	// explicit target outside the rule's scope (S-L4) when it runs the eval; a
	// handed-back eval is run by Python, which does not.
	var wbIDs []string
	if refs, ok := payloadTargets(targets, maxRows); ok {
		seen := map[string]bool{}
		for _, r := range refs {
			if r.workbookID != "" && !seen[r.workbookID] {
				seen[r.workbookID] = true
				wbIDs = append(wbIDs, r.workbookID)
			}
		}
		if len(wbIDs) == 0 {
			return "", nil
		}
	} else {
		wbIDs = scope
	}
	var reason string
	err := tenancy.InWorkspace(ctx, e.d.Pool, ws, func(tx pgx.Tx) error {
		wbs, err := gen.New(tx).AutoRunWorkbooksByIDs(ctx, gen.AutoRunWorkbooksByIDsParams{WorkspaceID: ws, Ids: nonNil(wbIDs)})
		if err != nil {
			return err
		}
		for _, wb := range wbs {
			cols, _ := parseColumns(wb.ColumnsConfig)
			for _, c := range cols {
				id, _ := c.Get("id").(string)
				if reIDs[id] && !e.goRunnable(c) {
					reason = "re_enrich_python_column: " + id
					return nil
				}
			}
		}
		return nil
	})
	return reason, err
}

// autoactionsConfig is action["config"] as a map (ordered values stay ordered).
func autoactionsConfig(action map[string]any) map[string]any {
	switch m := action["config"].(type) {
	case map[string]any:
		return m
	case interface {
		Keys() []string
		Get(string) (any, bool)
	}:
		out := map[string]any{}
		for _, k := range m.Keys() {
			out[k], _ = m.Get(k)
		}
		return out
	}
	return map[string]any{}
}

// goRunnable mirrors the columns workbook.Run accepts for a queued run: the
// enrichment, waterfall, formula and http types, plus ai_formula and research
// when the worker has the Python column RPC, and none with a column condition
// or an unverified-email target (the executor refuses those). Agent columns
// may spend outside the Go ledger and always stay on Python.
func (e *engine) goRunnable(c autoactions.Column) bool {
	t, _ := c.Get("type").(string)
	switch t {
	case "enrichment", "waterfall", "formula", "http":
	case "ai_formula", "research":
		if !e.d.PyColumns {
			return false
		}
	default:
		return false
	}
	if manifest.PyTruthy(c.Get("condition")) {
		return false
	}
	if t == "enrichment" || t == "waterfall" {
		if colTarget(c) == "email" {
			if v, ok := c.Lookup("verify"); !ok || manifest.PyTruthy(v) {
				return false
			}
		}
	}
	return true
}

// colTarget is target_field or lead_field or id (the first truthy).
func colTarget(c colgraph.Col) string {
	for _, k := range []string{"target_field", "lead_field", "id"} {
		if v := c.Get(k); manifest.PyTruthy(v) {
			return manifest.PyStr(v)
		}
	}
	return ""
}
