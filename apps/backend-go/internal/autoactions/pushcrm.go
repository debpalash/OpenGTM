package autoactions

import (
	"context"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/crmpush"
)

// PushFunc is output._push_crm; Deps.Push overrides it (tests).
type PushFunc func(ctx context.Context, ws string, cfg, leadData map[string]any, workbookID string, leadID int64) crmpush.Result

// PushCRM is _act_push_crm: push the row's lead to the workspace's CRM through
// the same code the output column uses. The workbook id is empty (no row
// stamp), exactly like Python. A failure is `failed` carrying the CRM error
// (or "crm push failed"); one that says "not connected" also carries
// skip_reason "not_connected". Every CRM call uses ws's own credentials. A
// cancelled ctx answers failed "context canceled" so the engine can tell a
// shutdown from a CRM failure (it must not record that as terminal).
func PushCRM(ctx context.Context, d Deps, ws string, cfg, leadData map[string]any, cols []Column, leadID int64) Result {
	push := d.Push
	if push == nil {
		push = func(ctx context.Context, ws string, cfg, lead map[string]any, wb string, id int64) crmpush.Result {
			return crmpush.Push(ctx, d.CRM, ws, cfg, lead, wb, id)
		}
	}
	res := push(ctx, ws, cfg, leadData, "", leadID)
	if res.Success {
		return Result{Status: "success", Summary: res.Value}
	}
	if err := ctx.Err(); err != nil {
		return failed(err.Error())
	}
	msg := res.Error
	if msg == "" {
		msg = "crm push failed"
	}
	out := failed(msg)
	if strings.Contains(strings.ToLower(msg), "not connected") {
		out.SkipReason = "not_connected"
	}
	return out
}
