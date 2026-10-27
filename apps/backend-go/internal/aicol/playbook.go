package aicol

import (
	"context"
	"math"
	"strings"
)

// PlaybookRequest is one research_column playbook step (playbooks/rpc_research.py).
type PlaybookRequest struct {
	PromptTemplate string
	Context        map[string]any
	MaxSteps       int
	OutputFormat   string
	CellBudgetUSD  float64
}

// PlaybookResult is research_column's answer. Metadata is {} unless the native path
// returned one (or {"truncated": true} past the 64 KiB cap).
type PlaybookResult struct {
	Success  bool
	Value    any
	Metadata *Obj
	Error    string
}

// MaxOutputBytes is the research_column cap of a value and of the metadata.
const MaxOutputBytes = 64 * 1024

func pbFail(msg string) PlaybookResult { return PlaybookResult{Metadata: NewObj(), Error: msg} }

// PlaybookResearch is rpc_research.research_column over the native research column
// (no columns: the context keys are the template's placeholders). A returned error is a
// cancelled context only.
func PlaybookResearch(ctx context.Context, d Deps, r PlaybookRequest) (PlaybookResult, error) {
	if math.IsNaN(r.CellBudgetUSD) || math.IsInf(r.CellBudgetUSD, 0) {
		return pbFail("invalid max_steps or cell_budget_usd"), nil
	}
	if strings.TrimSpace(r.PromptTemplate) == "" || pyStrip(r.PromptTemplate) == "" {
		return pbFail("prompt_template required"), nil
	}
	if r.OutputFormat != "text" && r.OutputFormat != "json" {
		return pbFail("output_format must be text or json"), nil
	}
	cx := r.Context
	if cx == nil {
		cx = map[string]any{}
	}
	budget := r.CellBudgetUSD
	res, err := Research(ctx, d, ResearchInput{Template: r.PromptTemplate, Row: cx, MaxSteps: r.MaxSteps, Budget: &budget})
	if ctx.Err() != nil {
		return PlaybookResult{}, ctx.Err()
	}
	if err != nil {
		return pbFail("ValueError: " + err.Error()), nil
	}
	meta := res.Metadata
	if meta == nil {
		meta = NewObj()
	}
	if pyJSONSize(res.Value) > MaxOutputBytes {
		return pbFail("value exceeds 64 KiB cap"), nil
	}
	if pyJSONSize(meta) > MaxOutputBytes {
		meta = NewObj("truncated", true)
	}
	return PlaybookResult{Success: res.Error == "" && res.Value != nil && res.Value != "", Value: res.Value, Metadata: meta,
		Error: res.Error}, nil
}
