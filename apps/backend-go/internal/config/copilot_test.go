package config

import (
	"strings"
	"testing"
)

func TestChatRequireAuthDefaultsToTrue(t *testing.T) {
	for _, c := range []struct {
		env  map[string]string
		want bool
	}{
		{map[string]string{}, true},
		{map[string]string{"PG_LEAD_STORE": "false"}, true}, // unconditional default (S-L9)
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

// The production refusal covers only a deployment that serves the Go copilot (GoCopilotEnabled and PG_LEAD_STORE).
func TestAnonymousChatGuardScopedToGoCopilot(t *testing.T) {
	prod := func(m map[string]string) map[string]string {
		m["SECRET_KEY"] = strings.Repeat("k", 40)
		m["APP_ENV"] = "production"
		m["CHAT_REQUIRE_AUTH"] = "false"
		return m
	}
	for _, c := range []struct {
		env     map[string]string
		refused bool
	}{
		{map[string]string{}, false}, // copilot off
		{map[string]string{"COPILOT_GO_ENABLED": "true", "PG_LEAD_STORE": "false"}, false},
		{map[string]string{"COPILOT_GO_ENABLED": "true"}, true},
		{map[string]string{"COPILOT_GO_ENABLED": "true", "COPILOT_ALLOW_ANONYMOUS_CHAT": "true"}, false},
	} {
		_, err := loadAPI(apiEnv(prod(c.env)))
		if (err != nil) != c.refused {
			t.Errorf("%v: err=%v want refused=%v", c.env, err, c.refused)
		}
	}
}

func TestCopilotMaxAnonStreams(t *testing.T) {
	c, err := loadAPI(apiEnv(map[string]string{"APP_ENV": "test"}))
	if err != nil || c.Flags.Copilot.MaxAnonStreams != 4 {
		t.Fatalf("default %+v %v", c.Flags.Copilot, err)
	}
	c, err = loadAPI(apiEnv(map[string]string{"APP_ENV": "test", "COPILOT_MAX_ANON_STREAMS": "9"}))
	if err != nil || c.Flags.Copilot.MaxAnonStreams != 9 {
		t.Fatalf("parsed %+v %v", c.Flags.Copilot, err)
	}
	for _, bad := range []string{"0", "-1", "x"} {
		if _, err := loadAPI(apiEnv(map[string]string{"APP_ENV": "test", "COPILOT_MAX_ANON_STREAMS": bad})); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
