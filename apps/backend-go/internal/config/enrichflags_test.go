package config

import (
	"testing"
)

func TestConfigEnrichmentFlags(t *testing.T) {
	c, err := loadAPI(apiEnv(map[string]string{"APP_ENV": "test"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Flags.GoRunRoutesEnabled || c.WSMaxMessageBytes != 65536 {
		t.Fatalf("defaults wrong: %+v", c)
	}
	c, err = loadAPI(apiEnv(map[string]string{"APP_ENV": "test", "GO_RUN_ROUTES_ENABLED": "true",
		"WS_MAX_MESSAGE_BYTES": "1024"}))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Flags.GoRunRoutesEnabled || c.WSMaxMessageBytes != 1024 {
		t.Fatalf("parsed wrong: %+v", c)
	}
	for _, bad := range []string{"0", "-1", "abc", "99999999999"} {
		if _, err := loadAPI(apiEnv(map[string]string{"APP_ENV": "test", "WS_MAX_MESSAGE_BYTES": bad})); err == nil {
			t.Errorf("WS_MAX_MESSAGE_BYTES=%q accepted", bad)
		}
	}
}
