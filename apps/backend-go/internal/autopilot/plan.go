// Package autopilot ports services/agent/autopilot*.py (plan 15 WP-COPILOT-TOOLS): goal -> plan (heuristic or
// the opt-in Anthropic structured-output planner), validation against the step allowlist and caps, the
// confirmation text, the server-side plan store with a single-use nonce, the per-workspace run memory and the
// executor that orchestrates the existing copilot tools.
package autopilot

import (
	"context"
	"strconv"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// Kinds is AUTOPILOT_TOOL_CATALOG in Python order with each kind's allowed params.
var Kinds = []struct {
	Kind   string
	Params []string
}{
	{"create_source_workbook", []string{"icp_description", "target_rows", "auto_run", "auto_enrich"}},
	{"add_agent_column", []string{"column_name", "goal", "target_field"}},
	{"add_research_column", []string{"column_name", "prompt", "max_steps"}},
	{"set_workbook_refresh", []string{"signals", "enabled"}},
	{"report", nil},
}

func allowedParams(kind string) ([]string, bool) {
	for _, k := range Kinds {
		if k.Kind == kind {
			return k.Params, true
		}
	}
	return nil, false
}

// PlannerSystem is _PLANNER_SYSTEM.
const PlannerSystem = `You are the Autopilot PLANNER for a B2B lead-research tool.
Turn the user's GOAL into an ordered PLAN of steps drawn ONLY from this catalog:
  - create_source_workbook {icp_description, target_rows, auto_run, auto_enrich}
      Create a live-sourcing workbook for an ideal-customer-profile description.
  - add_agent_column {column_name, goal, target_field}
      A goal-directed per-row enrichment column (e.g. find founder email).
  - add_research_column {column_name, prompt, max_steps}
      A per-row web-research column answering a question with citations.
  - set_workbook_refresh {signals, enabled}
      Auto-refresh the workbook on signals.
  - report {}
      Summarize the results.
Rules:
- Use ONLY the kinds and params above. Do NOT invent kinds or params.
- A typical plan creates a workbook, adds enrichment/research columns, then reports.
- It is fine to add columns to an existing workbook (no leading create needed).
- Keep it minimal — one column per distinct data field the goal asks for.
- estimated_rows is your best estimate of the row count.`

// PlanSchema is _PLAN_SCHEMA (the structured-output format of the planner).
func PlanSchema() *wbcrud.Obj {
	kinds := make([]any, len(Kinds))
	for i, k := range Kinds {
		kinds[i] = k.Kind
	}
	typ := func(t string) *wbcrud.Obj { o := wbcrud.NewObj(); o.Set("type", t); return o }
	kind := typ("string")
	kind.Set("enum", kinds)
	params := typ("object")
	params.Set("additionalProperties", true)
	stepProps := wbcrud.NewObj()
	stepProps.Set("kind", kind)
	stepProps.Set("description", typ("string"))
	stepProps.Set("params", params)
	step := typ("object")
	step.Set("properties", stepProps)
	step.Set("required", []any{"kind", "description", "params"})
	step.Set("additionalProperties", false)
	steps := typ("array")
	steps.Set("items", step)
	props := wbcrud.NewObj()
	props.Set("goal", typ("string"))
	props.Set("estimated_rows", typ("integer"))
	props.Set("steps", steps)
	s := typ("object")
	s.Set("properties", props)
	s.Set("required", []any{"goal", "estimated_rows", "steps"})
	s.Set("additionalProperties", false)
	return s
}

// Caps are AUTOPILOT_MAX_ROWS / AUTOPILOT_MAX_AGENT_COLUMNS / AUTOPILOT_MAX_CELLS.
type Caps struct{ Rows, Columns, Cells int64 }

func capInt(ctx context.Context, s Settings, key string, def int64) int64 {
	n, err := strconv.ParseInt(strip(setting(ctx, s, key, strconv.FormatInt(def, 10))), 10, 64)
	if err != nil {
		n = def
	}
	return max(1, n)
}

// CapsFrom reads the caps like _cap_int (a bad value is the default; every cap is at least 1).
func CapsFrom(ctx context.Context, s Settings) Caps {
	return Caps{capInt(ctx, s, "AUTOPILOT_MAX_ROWS", 500), capInt(ctx, s, "AUTOPILOT_MAX_AGENT_COLUMNS", 8),
		capInt(ctx, s, "AUTOPILOT_MAX_CELLS", 4000)}
}

type fieldSpec struct {
	keys               []string
	label, field, goal string
}

var fieldPatterns = []fieldSpec{
	{[]string{"founder", "ceo", "owner", "co-founder", "director", "promoter"}, "Founder Email", "email",
		"Find the verified email address of the founder, CEO, or owner."},
	{[]string{"email", "e-mail"}, "Email", "email", "Find the company's primary contact email address."},
	{[]string{"phone", "contact number", "mobile"}, "Phone", "phone", "Find the company's primary phone number."},
	{[]string{"linkedin"}, "LinkedIn", "linkedin_url", "Find the company's official LinkedIn page URL."},
}

func detectFields(goal string) []fieldSpec {
	g := strings.ToLower(goal)
	var out []fieldSpec
	seen := map[string]bool{}
	for _, f := range fieldPatterns {
		hit := false
		for _, k := range f.keys {
			if strings.Contains(g, k) {
				hit = true
				break
			}
		}
		if hit && !seen[f.field] {
			out = append(out, f)
			seen[f.field] = true
		}
	}
	return out
}

func step(kind, description string, params *wbcrud.Obj) *wbcrud.Obj {
	s := wbcrud.NewObj()
	s.Set("kind", kind)
	s.Set("description", description)
	s.Set("params", params)
	return s
}

// HeuristicPlan is _heuristic_plan; goal is already stripped by the caller (draft_plan strips it again).
func HeuristicPlan(goal string, targetCount int64) *wbcrud.Obj {
	g := strip(goal)
	fields := detectFields(g)
	desc := "Create a live-sourcing workbook for: " + g
	if targetCount != 0 {
		desc += " (target " + strconv.FormatInt(targetCount, 10) + " rows)"
	}
	p := wbcrud.NewObj()
	p.Set("icp_description", g)
	p.Set("target_rows", targetCount)
	p.Set("auto_run", true)
	p.Set("auto_enrich", len(fields) > 0)
	steps := []any{step("create_source_workbook", desc, p)}
	for _, f := range fields {
		cp := wbcrud.NewObj()
		cp.Set("column_name", f.label)
		cp.Set("goal", f.goal)
		cp.Set("target_field", f.field)
		steps = append(steps, step("add_agent_column", "Add an agent column \""+f.label+"\" — "+f.goal, cp))
	}
	steps = append(steps, step("report", "Summarize the results and link the workbook", wbcrud.NewObj()))
	plan := wbcrud.NewObj()
	plan.Set("goal", g)
	if targetCount != 0 {
		plan.Set("estimated_rows", targetCount)
	} else {
		plan.Set("estimated_rows", nil)
	}
	plan.Set("steps", steps)
	return plan
}

// intOr0 is `int(v or 0)` with any ValueError/TypeError treated as 0.
func intOr0(v any) int64 {
	if !truthy(v) {
		return 0
	}
	n, err := IntOf(v)
	if err != "" {
		return 0
	}
	return n
}

// ValidatePlan is _validate_plan: the plan clamped to the allowlist and caps, or nil when it is invalid.
func ValidatePlan(plan any, caps Caps) *wbcrud.Obj {
	p, ok := plan.(*wbcrud.Obj)
	if !ok {
		return nil
	}
	stepsIn, ok := lookup(p, "steps", nil).([]any)
	if !ok || len(stepsIn) == 0 {
		return nil
	}
	est := min(max(intOr0(lookup(p, "estimated_rows", nil)), 0), caps.Rows)
	var out []*wbcrud.Obj
	columns := int64(0)
	for _, raw := range stepsIn {
		s, ok := raw.(*wbcrud.Obj)
		if !ok {
			return nil
		}
		kind, isStr := lookup(s, "kind", nil).(string)
		if !isStr {
			return nil // Python: not in the allowlist (an unhashable kind raised; the planner fell back)
		}
		allowed, known := allowedParams(kind)
		if !known {
			return nil
		}
		params := lookup(s, "params", nil)
		if !truthy(params) {
			params = wbcrud.NewObj()
		}
		po, ok := params.(*wbcrud.Obj)
		if !ok {
			return nil
		}
		clean := wbcrud.NewObj()
		for _, k := range po.Keys() {
			if !contains(allowed, k) {
				return nil
			}
			v, _ := po.Get(k)
			clean.Set(k, v)
		}
		if kind == "create_source_workbook" {
			if v, has := clean.Get("target_rows"); has {
				clean.Set("target_rows", min(max(intOr0(v), 0), caps.Rows))
			}
		}
		if kind == "add_agent_column" || kind == "add_research_column" {
			columns++
			if columns > caps.Columns {
				continue
			}
		}
		desc := lookup(s, "description", "")
		descS := kind
		if truthy(desc) {
			descS = str(desc)
		}
		out = append(out, step(kind, descS, clean))
	}
	if len(out) == 0 {
		return nil
	}
	effective := est
	if columns > 0 && effective > 0 {
		cols := min(columns, caps.Columns)
		if effective*cols > caps.Cells {
			effective = max(1, caps.Cells/cols)
			for _, s := range out {
				if k, _ := s.Get("kind"); k != "create_source_workbook" {
					continue
				}
				pv, _ := s.Get("params")
				po := pv.(*wbcrud.Obj)
				if tr, has := po.Get("target_rows"); has {
					cur := tr.(int64)
					if cur == 0 {
						cur = effective
					}
					po.Set("target_rows", min(cur, effective))
				}
			}
		}
	}
	steps := make([]any, len(out))
	for i, s := range out {
		steps[i] = s
	}
	res := wbcrud.NewObj()
	goal := lookup(p, "goal", "")
	if truthy(goal) {
		res.Set("goal", str(goal))
	} else {
		res.Set("goal", "")
	}
	if effective != 0 {
		res.Set("estimated_rows", effective)
	} else {
		res.Set("estimated_rows", nil)
	}
	res.Set("steps", steps)
	return res
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// DescribePlan is describe_plan: the confirmation text, one step per line. ok=false where Python raises
// (a step that is not an object, steps that are not a list).
func DescribePlan(plan *wbcrud.Obj) (string, bool) {
	lines := []string{"🤖 Autopilot — " + str(lookup(plan, "goal", ""))}
	steps, ok := lookup(plan, "steps", []any{}).([]any)
	if !ok {
		return "", false
	}
	for i, raw := range steps {
		s, ok := raw.(*wbcrud.Obj)
		if !ok {
			return "", false
		}
		d, has := s.Get("description")
		if !has {
			d, _ = s.Get("kind")
		}
		lines = append(lines, strconv.Itoa(i+1)+". "+str(d))
	}
	if rows := lookup(plan, "estimated_rows", nil); truthy(rows) {
		lines = append(lines, "Target: ~"+str(rows)+" companies")
	}
	return strings.Join(lines, "\n"), true
}
