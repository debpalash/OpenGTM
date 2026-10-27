package workbook

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
)

// planMissingGoBlock returns the claim's payload, with a "go" block planned through deps.PlanGoBlock when the
// payload has none. Since the plan 15 cutover the Go worker runs every run_workbook job, including the ones whose
// enqueuer does not plan (refresh, source workbooks, the recompute fallback) and Python-era rows reassigned to Go.
// A payload that is not a JSON object, or that names no workbook, is left to parsePayload. A planner failure is a
// refusal (permanent): the workbook's columns cannot be planned now and will not plan on a retry either.
func planMissingGoBlock(ctx context.Context, deps Deps, c *queue.Claim) ([]byte, error) {
	var p map[string]json.RawMessage
	if err := json.Unmarshal(c.Payload, &p); err != nil || p == nil {
		return c.Payload, nil
	}
	if g, ok := p["go"]; ok && string(g) != "null" {
		return c.Payload, nil
	}
	if deps.PlanGoBlock == nil {
		return c.Payload, nil // parsePayload refuses: missing go block
	}
	var wb string
	if err := json.Unmarshal(p["workbook_id"], &wb); err != nil || wb == "" {
		return c.Payload, nil
	}
	block, err := deps.PlanGoBlock(ctx, c.WorkspaceID, wb)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, refuse("plan_failed: %v", err)
	}
	p["go"] = block
	out, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("workbook: re-encode planned payload: %w", err)
	}
	return out, nil
}
