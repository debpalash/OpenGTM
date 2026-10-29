package autopilot

import (
	"context"
	"encoding/json"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// ToolFunc runs one copilot tool in the plan's workspace (the chat's execute_tool bound to the tenant and
// user, role checks included) and returns its JSON result string. An error ends the run (cancelled turn).
type ToolFunc func(ctx context.Context, name string, args map[string]any) (string, error)

// decodeResult is json.loads(result) for a tool result object (a non-object reads as empty).
func decodeResult(s string) map[string]any {
	v, err := canonjson.Decode([]byte(s))
	if err != nil {
		return map[string]any{}
	}
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

// toArgs converts an ordered params object to the tool argument map (canonjson value shapes).
func toArgs(params any) map[string]any {
	o, ok := params.(*wbcrud.Obj)
	if !ok || o == nil {
		return map[string]any{}
	}
	b, err := o.MarshalJSON()
	if err != nil {
		return map[string]any{}
	}
	v, err := canonjson.Decode(b)
	if err != nil {
		return map[string]any{}
	}
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

// Execute is autopilot.execute_plan: it orchestrates create_source_workbook, add_agent_column and
// set_workbook_refresh through tool; report and unknown kinds are no-ops. The result is an ordered object
// (ok, workbook_id, goal, steps_done, message).
func Execute(ctx context.Context, plan *wbcrud.Obj, tool ToolFunc) (*wbcrud.Obj, error) {
	var wbID any // None until a create step answers
	stepsDone := []any{}
	steps, _ := lookup(plan, "steps", []any{}).([]any)
	for _, raw := range steps {
		s, ok := raw.(*wbcrud.Obj)
		if !ok {
			continue
		}
		kind, _ := s.Get("kind")
		args := toArgs(lookup(s, "params", nil))
		done := wbcrud.NewObj()
		switch kind {
		case "create_source_workbook":
			res, err := tool(ctx, "create_source_workbook", args)
			if err != nil {
				return nil, err
			}
			wbID = decodeResult(res)["workbook_id"]
			done.Set("kind", kind)
			done.Set("workbook_id", wbID)
			done.Set("ok", truthy(wbID))
		case "add_agent_column":
			if !truthy(wbID) {
				done.Set("kind", kind)
				done.Set("ok", false)
				done.Set("error", "no workbook")
				break
			}
			args["workbook_id"] = wbID
			res, err := tool(ctx, "add_agent_column", args)
			if err != nil {
				return nil, err
			}
			_, failed := decodeResult(res)["error"]
			done.Set("kind", kind)
			done.Set("column", args["column_name"])
			done.Set("ok", !failed)
		case "set_workbook_refresh":
			if !truthy(wbID) {
				continue
			}
			args["workbook_id"] = wbID
			if _, err := tool(ctx, "set_workbook_refresh", args); err != nil {
				return nil, err
			}
			done.Set("kind", kind)
			done.Set("ok", true)
		default:
			continue
		}
		stepsDone = append(stepsDone, done)
	}
	msg := "Autopilot could not create the workbook."
	if truthy(wbID) {
		id := str(wbID)
		msg = "Autopilot built workbook " + id + ". Sourcing and enrichment are running in the background — open /workbooks/" +
			id + " to watch rows and agent columns fill in."
	}
	out := wbcrud.NewObj()
	out.Set("ok", truthy(wbID))
	out.Set("workbook_id", jsonValue(wbID))
	out.Set("goal", lookup(plan, "goal", ""))
	out.Set("steps_done", stepsDone)
	out.Set("message", msg)
	return out, nil
}

// jsonValue keeps a decoded scalar writable by wbcrud (maps from canonjson are written key-sorted).
func jsonValue(v any) any {
	if m, ok := v.(map[string]any); ok {
		b, err := json.Marshal(m)
		if err == nil {
			if o, err := wbcrud.ParseObjectCanon(b); err == nil {
				return o
			}
		}
	}
	return v
}
