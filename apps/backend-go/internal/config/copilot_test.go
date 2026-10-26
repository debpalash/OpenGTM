package config

import (
	"strings"
	"testing"
)

func TestChatRequireAuthDefaultsToPGLeadStore(t *testing.T) {
	for _, c := range []struct {
		env  map[string]string
		want bool
	}{
		{map[string]string{}, true}, // PG_LEAD_STORE defaults true
		{map[string]string{"PG_LEAD_STORE": "false"}, false},
		{map[string]string{"PG_LEAD_STORE": "false", "CHAT_REQUIRE_AUTH": "true"}, true},
		{map[string]string{"PG_LEAD_STORE": "true", "CHAT_REQUIRE_AUTH": "false"}, false},
	} {
		c.env["APP_ENV"] = "test"
		got, err := loadAPI(apiEnv(c.env))
		if err != nil {
			t.Fatal(err)
		}
		if got.Flags.Copilot.ChatRequireAuth != c.want {
			t.Errorf("%v: ChatRequireAuth=%v want %v", c.env, got.Flags.Copilot.ChatRequireAuth, c.want)
		}
	}
}

func TestCopilotSettingsDefaultsAndParse(t *testing.T) {
	c, err := loadAPI(apiEnv(map[string]string{"APP_ENV": "test"}))
	if err != nil {
		t.Fatal(err)
	}
	f := c.Flags.Copilot
	if f.GoCopilotEnabled || f.MaxBodyBytes != 1048576 || f.MaxStreamsPerUser != 3 || f.TurnTimeoutSeconds != 900 || f.ToolTimeoutSeconds != 120 {
		t.Fatalf("defaults wrong: %+v", f)
	}
	c, err = loadAPI(apiEnv(map[string]string{"APP_ENV": "test", "COPILOT_GO_ENABLED": "true", "COPILOT_MAX_BODY_BYTES": "2048",
		"COPILOT_MAX_STREAMS_PER_USER": "5", "COPILOT_TURN_TIMEOUT_SECONDS": "60", "COPILOT_TOOL_TIMEOUT_SECONDS": "30"}))
	if err != nil {
		t.Fatal(err)
	}
	f = c.Flags.Copilot
	if !f.GoCopilotEnabled || f.MaxBodyBytes != 2048 || f.MaxStreamsPerUser != 5 || f.TurnTimeoutSeconds != 60 || f.ToolTimeoutSeconds != 30 {
		t.Fatalf("parsed wrong: %+v", f)
	}
	for _, k := range []string{"COPILOT_MAX_BODY_BYTES", "COPILOT_MAX_STREAMS_PER_USER", "COPILOT_TURN_TIMEOUT_SECONDS", "COPILOT_TOOL_TIMEOUT_SECONDS"} {
		for _, bad := range []string{"0", "-1", "abc"} {
			if _, err := loadAPI(apiEnv(map[string]string{"APP_ENV": "test", k: bad})); err == nil {
				t.Errorf("%s=%q accepted", k, bad)
			} else if strings.Contains(err.Error(), "panic") {
				t.Error(err)
			}
		}
	}
}

// Fail closed: an explicit CHAT_REQUIRE_AUTH=false outside a dev APP_ENV needs COPILOT_ALLOW_ANONYMOUS_CHAT=true.
func TestChatRequireAuthFalseRefusedOutsideDev(t *testing.T) {
	base := func(m map[string]string) map[string]string {
		m["SECRET_KEY"] = strings.Repeat("k", 40)
		return m
	}
	if _, err := loadAPI(apiEnv(base(map[string]string{"APP_ENV": "production", "CHAT_REQUIRE_AUTH": "false"}))); err == nil {
		t.Fatal("CHAT_REQUIRE_AUTH=false accepted in production")
	}
	if _, err := loadAPI(apiEnv(base(map[string]string{"APP_ENV": "", "CHAT_REQUIRE_AUTH": "false"}))); err == nil {
		t.Fatal("CHAT_REQUIRE_AUTH=false accepted with unset APP_ENV")
	}
	for _, env := range []map[string]string{
		{"APP_ENV": "dev", "CHAT_REQUIRE_AUTH": "false"},
		{"APP_ENV": "production", "CHAT_REQUIRE_AUTH": "false", "COPILOT_ALLOW_ANONYMOUS_CHAT": "true"},
		{"APP_ENV": "production", "CHAT_REQUIRE_AUTH": "true"},
		{"APP_ENV": "production"},
	} {
		if _, err := loadAPI(apiEnv(base(env))); err != nil {
			t.Errorf("%v refused: %v", env, err)
		}
	}
}
