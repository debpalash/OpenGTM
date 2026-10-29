package autopilot

import (
	"context"
	"log/slog"
	"strconv"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/llm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// LLM is the slice of *llm.Client the planner uses.
type LLM interface {
	AnthropicProvider(ctx context.Context) (llm.Provider, bool)
	AnthropicTurn(ctx context.Context, r llm.AnthropicRequest) (*llm.AnthropicResponse, error)
}

// Planner drafts plans (autopilot.draft_plan).
type Planner struct {
	LLM      LLM      // nil: heuristic only
	Settings Settings // global app settings (Python _read_setting)
	Memory   Memory   // nil: no recalled runs
	Log      *slog.Logger
}

func (p *Planner) logger() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

// nativeEnabled is _native_enabled: AUTOPILOT_LLM_PLANNER is on AND Anthropic leads the provider chain.
func (p *Planner) nativeEnabled(ctx context.Context) bool {
	switch strings.ToLower(strip(setting(ctx, p.Settings, "AUTOPILOT_LLM_PLANNER", "0"))) {
	case "0", "false", "off", "no", "":
		return false
	}
	if p.LLM == nil {
		return false
	}
	_, ok := p.LLM.AnthropicProvider(ctx)
	return ok
}

// SystemPrompt is _planner_system().
func (p *Planner) SystemPrompt(ctx context.Context) string {
	if n := SystemNotice(ctx, p.Settings); n != "" {
		return PlannerSystem + "\n\n" + n
	}
	return PlannerSystem
}

// UserPrompt is the planner's user turn: the goal, the target, the workspace's recent runs (guarded as
// untrusted) and the output instruction.
func (p *Planner) UserPrompt(ctx context.Context, goal string, targetCount int64, ws string) string {
	var memLines []string
	if p.Memory != nil && ws != "" {
		recs, err := p.Memory.Recent(ctx, ws, 3)
		if err != nil {
			p.logger().Info("autopilot memory unavailable", "reason", err.Error())
		}
		for _, m := range recs {
			memLines = append(memLines, "- "+Guard(ctx, p.Settings, m.Goal, "prior goal")+" → outcome="+m.Outcome)
		}
	}
	var b strings.Builder
	b.WriteString("GOAL: " + goal + "\n")
	if targetCount != 0 {
		b.WriteString("Target row count: " + strconv.FormatInt(targetCount, 10) + "\n")
	}
	if len(memLines) > 0 {
		b.WriteString("\n\nRecent autopilot runs in this workspace (for context only):\n" + strings.Join(memLines, "\n"))
	}
	b.WriteString("\n\nProduce the plan as JSON {goal, estimated_rows, steps:[{kind, description, params}]}.")
	return b.String()
}

func (p *Planner) draftLLM(ctx context.Context, goal string, targetCount int64, ws string) *wbcrud.Obj {
	req := llm.AnthropicRequest{
		Messages:     []any{map[string]any{"role": "user", "content": p.UserPrompt(ctx, goal, targetCount, ws)}},
		System:       p.SystemPrompt(ctx),
		OutputFormat: map[string]any{"type": "json_schema", "schema": PlanSchema()},
		MaxTokens:    1024,
	}
	resp, err := p.LLM.AnthropicTurn(ctx, req)
	if err != nil {
		p.logger().Info("autopilot planner LLM call failed", "reason", err.Error())
		return nil
	}
	text := strip(resp.Text())
	if text == "" {
		return nil
	}
	raw, err := wbcrud.ParseObjectCanon([]byte(text))
	if err != nil {
		return nil
	}
	return ValidatePlan(raw, CapsFrom(ctx, p.Settings))
}

// Draft is draft_plan: the LLM planner when enabled (validated), else the heuristic. It never fails.
func (p *Planner) Draft(ctx context.Context, goal string, targetCount int64, ws string) *wbcrud.Obj {
	g := strip(goal)
	if p.nativeEnabled(ctx) {
		if plan := p.draftLLM(ctx, g, targetCount, ws); plan != nil {
			if steps, _ := plan.Get("steps"); truthy(steps) {
				p.logger().Info("autopilot_llm_plans")
				return plan
			}
		}
		p.logger().Info("autopilot_heuristic_fallbacks", "reason", "invalid_or_empty")
	}
	return HeuristicPlan(g, targetCount)
}
