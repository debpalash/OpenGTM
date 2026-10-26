package config

// CopilotSettings are the plan 12 (copilot chat and person intel) settings. COPILOT_GO_ENABLED is a
// Go-only rollout switch (default false). ChatRequireAuth keeps Python's CHAT_REQUIRE_AUTH
// semantics: unset defaults to PG_LEAD_STORE. LLM provider keys live only on the Python worker.
type CopilotSettings struct {
	GoCopilotEnabled   bool // COPILOT_GO_ENABLED
	ChatRequireAuth    bool // CHAT_REQUIRE_AUTH, default = PG_LEAD_STORE
	MaxBodyBytes       int  // COPILOT_MAX_BODY_BYTES (default 1 MiB)
	MaxStreamsPerUser  int  // COPILOT_MAX_STREAMS_PER_USER (default 3)
	TurnTimeoutSeconds int  // COPILOT_TURN_TIMEOUT_SECONDS (default 900)
	ToolTimeoutSeconds int  // COPILOT_TOOL_TIMEOUT_SECONDS (default 120)
}

func loadCopilot(get func(string) string, pgLeadStore bool) (CopilotSettings, error) {
	var c CopilotSettings
	var err error
	if c.GoCopilotEnabled, err = boolVar(get, "COPILOT_GO_ENABLED", false); err != nil {
		return c, err
	}
	if c.ChatRequireAuth, err = boolVar(get, "CHAT_REQUIRE_AUTH", pgLeadStore); err != nil {
		return c, err
	}
	for _, v := range []struct {
		dst      *int
		key      string
		def, max int
	}{
		{&c.MaxBodyBytes, "COPILOT_MAX_BODY_BYTES", 1048576, 64 << 20},
		{&c.MaxStreamsPerUser, "COPILOT_MAX_STREAMS_PER_USER", 3, 1000},
		{&c.TurnTimeoutSeconds, "COPILOT_TURN_TIMEOUT_SECONDS", 900, 86400},
		{&c.ToolTimeoutSeconds, "COPILOT_TOOL_TIMEOUT_SECONDS", 120, 86400},
	} {
		if *v.dst, err = intVar(get, v.key, v.def, 1, v.max); err != nil {
			return c, err
		}
	}
	return c, nil
}
