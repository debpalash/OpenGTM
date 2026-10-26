package autoactions

import (
	"context"
	"fmt"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/crmpush"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/rowtmpl"
)

// Deps are the collaborators of the executable actions. Billing is not here:
// the engine reserves and debits before Execute and settles or releases after.
type Deps struct {
	Webhook WebhookDeps
	CRM     crmpush.Deps
	// Push overrides crmpush.Push (tests).
	Push    PushFunc
	RunRows RowRunner
}

// Execute is execute_action: run one action against one row. It does no
// billing. leadData must be decoded with UseNumber and ordered objects
// (field_map) as colgraph.Col or *wbcrud.Obj so the CRM field order survives.
//
// sequencer and send_email are not executable in Go: the worker hands such a
// run to Python before it starts (Task 14), so reaching them here answers the
// same failed/not_connected result as an unknown type. A crash inside an
// action is a failed result, never a panic.
func Execute(ctx context.Context, d Deps, ws string, action map[string]any, workbookID, rowID string,
	leadID *int64, leadData map[string]any, cols []Column, idem string) (res Result) {
	_ = idem // only send_email uses it, and that runs on Python
	defer func() {
		if r := recover(); r != nil {
			res = failed(fmt.Sprint(r))
		}
	}()
	atype, _ := action["type"].(string)
	cfg := configOf(action["config"])
	switch atype {
	case "webhook":
		return Webhook(ctx, d.Webhook, ws, cfg, leadData, cols)
	case "push_crm":
		var id int64
		if leadID != nil {
			id = *leadID
		}
		return PushCRM(ctx, d, ws, cfg, leadData, cols, id)
	case "re_enrich":
		return ReEnrich(ctx, d, ws, workbookID, rowID, cfg, cols)
	}
	r := failed("action type '" + rowtmpl.PyStr(action["type"]) + "' not supported")
	r.SkipReason = "not_connected"
	return r
}
