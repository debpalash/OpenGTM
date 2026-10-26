package config

import (
	"strings"
	"testing"
)

func TestConfigWorkbookFlagsDefaults(t *testing.T) {
	c, err := loadAPI(apiEnv(map[string]string{"APP_ENV": "test"}))
	if err != nil {
		t.Fatal(err)
	}
	f := c.Flags
	if !f.PGLeadStore {
		t.Error("PG_LEAD_STORE default must be true (Python default)")
	}
	if f.AutomationsEnabled || f.IntentPollerEnabled || f.AutomationsAllowLegacyOutreach {
		t.Errorf("automation flags must default false: %+v", f)
	}
	if f.FilesAPIMode != FilesAPIModeHardened {
		t.Errorf("FILES_API_MODE default = %q, want hardened", f.FilesAPIMode)
	}
	if c.DataDir == "" {
		t.Error("DATA_DIR must have a default")
	}

	c, err = loadAPI(apiEnv(map[string]string{
		"APP_ENV": "test", "PG_LEAD_STORE": "false", "AUTOMATIONS_ENABLED": "true",
		"INTENT_POLLER_ENABLED": "1", "AUTOMATIONS_ALLOW_LEGACY_OUTREACH": "yes",
		"DATA_DIR": "/srv/data", "FILES_API_MODE": " LEGACY ",
	}))
	if err != nil {
		t.Fatal(err)
	}
	f = c.Flags
	if f.PGLeadStore || !f.AutomationsEnabled || !f.IntentPollerEnabled || !f.AutomationsAllowLegacyOutreach {
		t.Errorf("flags not parsed: %+v", f)
	}
	if f.FilesAPIMode != FilesAPIModeLegacy || c.DataDir != "/srv/data" {
		t.Errorf("mode/dir = %q %q", f.FilesAPIMode, c.DataDir)
	}
}

func TestConfigFilesAPIModeValidation(t *testing.T) {
	for _, bad := range []string{"strict", "hardened2", "off", "true"} {
		if _, err := loadAPI(apiEnv(map[string]string{"APP_ENV": "test", "FILES_API_MODE": bad})); err == nil {
			t.Errorf("FILES_API_MODE=%q accepted", bad)
		}
	}
	for _, ok := range []string{"hardened", "legacy", ""} {
		if _, err := loadAPI(apiEnv(map[string]string{"APP_ENV": "test", "FILES_API_MODE": ok})); err != nil {
			t.Errorf("FILES_API_MODE=%q rejected: %v", ok, err)
		}
	}
	if _, err := loadAPI(apiEnv(map[string]string{"APP_ENV": "test", "PG_LEAD_STORE": "maybe"})); err == nil {
		t.Error("bad PG_LEAD_STORE accepted")
	}
}

func TestConfigSSEAndRedis(t *testing.T) {
	c, err := loadAPI(apiEnv(map[string]string{"APP_ENV": "test"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.SSEMaxStreams != 1000 || c.RedisURL != "" {
		t.Errorf("defaults = %d %q", c.SSEMaxStreams, c.RedisURL)
	}
	c, err = loadAPI(apiEnv(map[string]string{"APP_ENV": "test", "SSE_MAX_STREAMS": "5", "REDIS_URL": "redis://:pw-secret@h:6379/0"}))
	if err != nil || c.SSEMaxStreams != 5 || c.RedisURL == "" {
		t.Fatalf("parsed = %v %+v", err, c)
	}
	if s := c.String(); strings.Contains(s, "pw-secret") {
		t.Error("REDIS_URL password leaked in String()")
	}
	for _, bad := range []string{"0", "-1", "x"} {
		if _, err := loadAPI(apiEnv(map[string]string{"APP_ENV": "test", "SSE_MAX_STREAMS": bad})); err == nil {
			t.Errorf("SSE_MAX_STREAMS=%q accepted", bad)
		}
	}
}
