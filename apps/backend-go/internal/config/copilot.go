package config

import (
	"errors"
	"strings"
)

// CopilotSettings are the plan 12 (copilot chat and person intel) settings. COPILOT_GO_ENABLED is a
// Go-only rollout switch (default false). ChatRequireAuth keeps Python's CHAT_REQUIRE_AUTH
// semantics: unset defaults to PG_LEAD_STORE. LLM provider keys live only on the Python worker.
type CopilotSettings struct {
	GoCopilotEnabled   bool // COPILOT_GO_ENABLED
	ChatRequireAuth    bool // CHAT_REQUIRE_AUTH, default = PG_LEAD_STORE
	MaxBodyBytes       int  // COPILOT_MAX_BODY_BYTES (default 1 MiB)
	MaxStreamsPerUser  int  // COPILOT_MAX_STREAMS_PER_USER (default 3)
	MaxAnonStreams     int  // COPILOT_MAX_ANON_STREAMS (default 4): concurrent anonymous chat streams in total
	TurnTimeoutSeconds int  // COPILOT_TURN_TIMEOUT_SECONDS (default 900)
	ToolTimeoutSeconds int  // COPILOT_TOOL_TIMEOUT_SECONDS (default 120)
	// Abuse and spend limits (final security M3). Chat turns per minute per user and per workspace
	// (COPILOT_CHAT_PER_USER_PER_MINUTE default 20, COPILOT_CHAT_PER_WORKSPACE_PER_MINUTE default 60), and the
	// workspace's daily LLM budget in USD for chat rounds (COPILOT_LLM_DAILY_BUDGET_USD default 20; 0 disables).
	// A zero rate (only a hand-built struct) means no limit.
	ChatPerUserPerMinute      int
	ChatPerWorkspacePerMinute int
	LLMDailyBudgetUSD         float64
}

// defaultLLMDailyBudgetUSD is the per-workspace daily copilot LLM budget when COPILOT_LLM_DAILY_BUDGET_USD is unset.
const defaultLLMDailyBudgetUSD = 20

func loadCopilot(get func(string) string, pgLeadStore bool) (CopilotSettings, error) {
	var c CopilotSettings
	var err error
	if c.GoCopilotEnabled, err = boolVar(get, "COPILOT_GO_ENABLED", false); err != nil {
		return c, err
	}
	if c.ChatRequireAuth, err = boolVar(get, "CHAT_REQUIRE_AUTH", true); err != nil {
		return c, err
	}
	// Fail closed: anonymous chat (no user, dangerous tools approvable by anyone who reaches nginx) is
	// only honoured outside dev when the operator explicitly opts in. It matters only where the Go copilot
	// is served (COPILOT_GO_ENABLED and PG_LEAD_STORE); the default is true everywhere.
	if c.GoCopilotEnabled && pgLeadStore && strings.TrimSpace(get("CHAT_REQUIRE_AUTH")) != "" && !c.ChatRequireAuth && !IsDevEnv(get("APP_ENV")) {
		allow, aerr := boolVar(get, "COPILOT_ALLOW_ANONYMOUS_CHAT", false)
		if aerr != nil {
			return c, aerr
		}
		if !allow {
			return c, errors.New("config: refusing to start: CHAT_REQUIRE_AUTH=false outside a dev APP_ENV requires COPILOT_ALLOW_ANONYMOUS_CHAT=true")
		}
	}
	for _, v := range []struct {
		dst      *int
		key      string
		def, max int
	}{
		{&c.MaxBodyBytes, "COPILOT_MAX_BODY_BYTES", 1048576, 64 << 20},
		{&c.MaxStreamsPerUser, "COPILOT_MAX_STREAMS_PER_USER", 3, 1000},
		{&c.MaxAnonStreams, "COPILOT_MAX_ANON_STREAMS", 4, 1000},
		{&c.TurnTimeoutSeconds, "COPILOT_TURN_TIMEOUT_SECONDS", 900, 86400},
		{&c.ToolTimeoutSeconds, "COPILOT_TOOL_TIMEOUT_SECONDS", 120, 86400},
		{&c.ChatPerUserPerMinute, "COPILOT_CHAT_PER_USER_PER_MINUTE", 20, 100000},
		{&c.ChatPerWorkspacePerMinute, "COPILOT_CHAT_PER_WORKSPACE_PER_MINUTE", 60, 100000},
	} {
		if *v.dst, err = intVar(get, v.key, v.def, 1, v.max); err != nil {
			return c, err
		}
	}
	c.LLMDailyBudgetUSD = defaultLLMDailyBudgetUSD
	if strings.TrimSpace(get("COPILOT_LLM_DAILY_BUDGET_USD")) != "" {
		if c.LLMDailyBudgetUSD, err = usdVar(get, "COPILOT_LLM_DAILY_BUDGET_USD"); err != nil {
			return c, err
		}
	}
	return c, nil
}
