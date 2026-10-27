package autoactions

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

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

	// LegacyOutreach (GO_EXECUTOR_LEGACY_OUTREACH_ACTIONS) makes sequencer and
	// send_email executable in Go; they need Pool. They never send mail: send_email
	// enqueues a `send` job for the outreachsend handler. SendExecutor names that
	// job's executor ("go"/"python"; nil or anything else: python).
	LegacyOutreach bool
	Pool           *pgxpool.Pool
	SendExecutor   func(ws string) string
	Now            func() time.Time
}

// Execute is execute_action: run one action against one row. It does no
// billing. leadData must be decoded with UseNumber and ordered objects
// (field_map) as colgraph.Col or *wbcrud.Obj so the CRM field order survives.
//
// sequencer and send_email are executable only with Deps.LegacyOutreach; without
// it the worker hands such a run to Python before it starts (Task 14), so
// reaching them here answers the same failed/not_connected result as an unknown
// type. A crash inside an
// action is a failed result, never a panic.
func Execute(ctx context.Context, d Deps, ws string, action map[string]any, workbookID, rowID string,
	leadID *int64, leadData map[string]any, cols []Column, idem string) (res Result) {
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
	case "sequencer":
		if d.LegacyOutreach {
			return legacySequencer(ctx, d, ws, cfg, leadID, leadData)
		}
	case "send_email":
		if d.LegacyOutreach {
			return legacySendEmail(ctx, d, ws, cfg, leadID, leadData, idem)
		}
	}
	r := failed("action type '" + rowtmpl.PyStr(action["type"]) + "' not supported")
	r.SkipReason = "not_connected"
	return r
}
