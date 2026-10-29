package copilot

import (
	"context"
	_ "embed"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
)

// prompt.tmpl is services/copilot/prompt.py's f-string with its three holes ({{ICP}}, {{STATS}}, {{OPENUI}}),
// extracted from the recorded prompt (testdata/native_prompt.json pins the bytes).
//
//go:embed prompt.tmpl
var promptTemplate string

// openui_system_prompt.txt is a copy of the repository's data/openui_system_prompt.txt, which Python reads at
// every chat turn from its working directory (/app/data in the image). TestOpenUISpecMatchesData keeps them equal.
//
//go:embed openui_system_prompt.txt
var openUISpec string

// icpText is the ICP block rendered from services/leadgen/config.py ICP (a constant).
const icpText = "**Value Proposition:** Yupcha Superadmin Console — Unified multi-database CRM for candidate tracking, attendance, client management, and automated reporting\n" +
	"**Target Industries:** HR Services, IT Staffing, Recruitment, Workforce Solutions, Payroll, Recruitment Process Outsourcing, Contract Staffing, Executive Search\n" +
	"**Target Cities:** Bangalore, Mumbai, Delhi, Hyderabad, Pune, Chennai, Noida, Gurgaon\n" +
	"**Preferred Size:** 10+ employees\n" +
	"**Preferred Specializations:** IT Staffing, Workforce Management, Recruitment Process Outsourcing, HR SaaS, Staffing & Payroll"

// PromptStats is the part of get_stats() the prompt reads. Nil means no store (Python's store=None).
type PromptStats struct {
	Total  int64
	ByTier map[string]int64
}

// RenderSystemPrompt is build_system_prompt. err != nil renders "Pipeline stats unavailable".
func RenderSystemPrompt(st *PromptStats, err error) string {
	var stats string
	if err != nil {
		stats = "Pipeline stats unavailable"
	} else {
		if st == nil {
			st = &PromptStats{}
		}
		tier := func(k string) string { return strconv.FormatInt(st.ByTier[k], 10) }
		stats = "**Total Leads:** " + strconv.FormatInt(st.Total, 10) + "\n" +
			"**By Tier:** Hot: " + tier("hot") + ", Warm: " + tier("warm") + ", Cold: " + tier("cold") +
			", Unqualified: " + tier("unqualified")
	}
	return strings.NewReplacer("{{ICP}}", icpText, "{{STATS}}", stats, "{{OPENUI}}", openUISpec).Replace(promptTemplate)
}

// SystemPrompt builds the workspace system prompt from the lead store bound to tx (the
// /internal/copilot/system-prompt RPC). A stats failure is rendered, never returned, like Python.
func SystemPrompt(ctx context.Context, tx pgx.Tx) string {
	s, err := leadsread.Stats(ctx, tx)
	if err != nil {
		return RenderSystemPrompt(nil, err)
	}
	ps := &PromptStats{Total: s.Total, ByTier: map[string]int64{}}
	for _, c := range s.ByTier {
		if !c.Null {
			ps.ByTier[c.Key] = c.N
		}
	}
	return RenderSystemPrompt(ps, nil)
}
