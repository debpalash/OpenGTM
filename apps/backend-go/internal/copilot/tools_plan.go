package copilot

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autopilot"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// This file ports the autopilot tools (plan 15 WP-COPILOT-TOOLS): draft_plan (safe) stores the plan
// server-side with a single-use nonce; execute_plan (dangerous) runs only the stored plan, once, after the
// approval gate, orchestrating the Go tools in this turn's workspace and user (role checks included); the
// confirmation card renders the stored plan.

var (
	defaultPlansOnce sync.Once
	defaultPlans     *autopilot.MemoryPlanStore
	defaultMemory    = &autopilot.InProcessMemory{}
)

// planStore is Engine.Plans, else one in-process store for this API process (Python's behaviour).
func (t *Turn) planStore() autopilot.PlanStore {
	if t.Engine != nil && t.Engine.Plans != nil {
		return t.Engine.Plans
	}
	defaultPlansOnce.Do(func() {
		s := t.Settings
		defaultPlans = &autopilot.MemoryPlanStore{TTL: func(ctx context.Context) time.Duration { return autopilot.PlanTTL(ctx, s) }}
	})
	return defaultPlans
}

func (t *Turn) planMemory() autopilot.Memory {
	if t.Engine != nil && t.Engine.Memory != nil {
		return t.Engine.Memory
	}
	return defaultMemory
}

func (t *Turn) planner() *autopilot.Planner {
	p := &autopilot.Planner{Settings: t.Settings, Memory: t.planMemory(), Log: t.Log}
	if t.Engine != nil && t.Engine.LLM != nil {
		p.LLM = t.Engine.LLM
		if t.Budget != nil {
			// The planner's llm call spends the same workspace daily budget as the chat rounds.
			p.LLM = &meteredLLM{inner: t.Engine.LLM, budget: t.Budget, ws: t.WS, conv: t.ConvID}
		}
	}
	return p
}

// planUser is the plan store's user key: str(user_id), "" for a self-host chat.
func (t *Turn) planUser() string {
	if t.UID == nil {
		return ""
	}
	return strconv.FormatInt(int64(*t.UID), 10)
}

// pyTypeName is type(v).__name__ for a decoded argument.
func pyTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case string:
		return "str"
	case int64, int, json.Number:
		return "int"
	case float64:
		return "float"
	case []any:
		return "list"
	}
	return "dict"
}

func (t *Turn) draftPlan(ctx context.Context, a map[string]any) (string, bool, error) {
	const name = "draft_plan"
	goalV, present := a["goal"]
	if !present {
		return keyErr(name, "goal"), true, nil
	}
	var target int64
	if v := a["target_count"]; pyTruthy(v) {
		n, msg := autopilot.IntOf(v)
		if msg != "" {
			return toolErr(name, msg), true, nil
		}
		target = n
	}
	goal := ""
	if pyTruthy(goalV) {
		s, ok := goalV.(string)
		if !ok {
			return toolErr(name, "'"+pyTypeName(goalV)+"' object has no attribute 'strip'"), true, nil
		}
		goal = s
	}
	plan := t.planner().Draft(ctx, goal, target, t.WS)
	pj, err := plan.MarshalJSON()
	if err != nil {
		return "", false, err
	}
	id, nonce, err := t.planStore().Put(ctx, t.WS, t.planUser(), pj)
	if err != nil {
		return "", false, err
	}
	o := wbcrud.NewObj()
	o.Set("plan", plan)
	o.Set("plan_id", id)
	o.Set("nonce", nonce)
	o.Set("message", "Drafted a plan. Call execute_plan with {plan_id, nonce} to run it (the user will be asked to approve).")
	s, err := objJSON(o)
	return s, err == nil, err
}

func (t *Turn) executePlan(ctx context.Context, a map[string]any) (string, bool, error) {
	planID, _ := a["plan_id"].(string)
	nonce, _ := a["nonce"].(string)
	var stored []byte
	if planID != "" {
		var err error
		if stored, err = t.planStore().Consume(ctx, t.WS, t.planUser(), planID, nonce); err != nil {
			return "", false, err
		}
	}
	if stored == nil {
		return errObj("Plan not found or already executed. Re-draft and approve the plan."), true, nil
	}
	raw, err := wbcrud.ParseObjectCanon(stored)
	if err != nil {
		return "", false, err
	}
	plan := autopilot.ValidatePlan(raw, autopilot.CapsFrom(ctx, t.Settings))
	if plan == nil {
		plan = raw
	}
	tool := func(ctx context.Context, name string, args map[string]any) (string, error) {
		return t.callToolRaw(ctx, parsedCall{Name: name, Args: args, ArgsJSON: []byte("{}"), Dangerous: IsDangerous(name)})
	}
	res, err := autopilot.Execute(ctx, plan, tool)
	if err != nil {
		return "", false, err
	}
	var wbID *string
	if v, _ := res.Get("workbook_id"); pyTruthy(v) {
		s := pyStr(v)
		wbID = &s
	}
	outcome := "failed"
	if ok, _ := res.Get("ok"); ok == true {
		outcome = "ok"
	}
	goal, _ := plan.Get("goal")
	if err := t.planMemory().Record(ctx, t.WS, pyStr(orNil(goal)), wbID, outcome); err != nil {
		t.logger().Info("autopilot memory write failed", "reason", err.Error())
	}
	s, err := objJSON(res)
	return s, err == nil, err
}

func orNil(v any) any {
	if v == nil {
		return ""
	}
	return v
}

// describePlan is describe_action("execute_plan"): the server-stored plan (by plan_id, this workspace), else
// the echoed plan body, one step per line; "label\nreason" when there is no plan with steps.
func (t *Turn) describePlan(ctx context.Context, args map[string]any) string {
	var plan *wbcrud.Obj
	if id, ok := args["plan_id"].(string); ok && id != "" {
		if b, err := t.planStore().Peek(ctx, t.WS, id); err == nil && b != nil {
			plan, _ = wbcrud.ParseObjectCanon(b)
		}
	}
	if plan == nil || plan.Len() == 0 {
		if m, ok := args["plan"].(map[string]any); ok {
			if b, err := json.Marshal(m); err == nil {
				plan, _ = wbcrud.ParseObjectCanon(b)
			}
		}
	}
	if plan != nil {
		if steps, _ := plan.Get("steps"); pyTruthy(steps) {
			if d, ok := autopilot.DescribePlan(plan); ok {
				return d
			}
		}
	}
	return t.describeFallback("execute_plan")
}
