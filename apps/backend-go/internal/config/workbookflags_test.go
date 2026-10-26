package config

import "testing"

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
