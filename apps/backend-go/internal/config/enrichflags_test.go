package config

import (
	"strings"
	"testing"
)

func TestConfigEnrichmentFlags(t *testing.T) {
	c, err := loadAPI(apiEnv(map[string]string{"APP_ENV": "test"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Flags.GoRunRoutesEnabled || c.PyWorkerURL != "" || c.PyWorkerToken != "" || c.WSMaxMessageBytes != 65536 {
		t.Fatalf("defaults wrong: %+v", c)
	}
	c, err = loadAPI(apiEnv(map[string]string{"APP_ENV": "test", "GO_RUN_ROUTES_ENABLED": "true",
		"PY_WORKER_URL": " http://py:8000 ", "PY_WORKER_TOKEN": "sekret-token", "WS_MAX_MESSAGE_BYTES": "1024"}))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Flags.GoRunRoutesEnabled || c.PyWorkerURL != "http://py:8000" || c.PyWorkerToken != "sekret-token" || c.WSMaxMessageBytes != 1024 {
		t.Fatalf("parsed wrong: %+v", c)
	}
	if strings.Contains(c.String(), "sekret-token") {
		t.Fatal("PY_WORKER_TOKEN leaked in String()")
	}
	for _, bad := range []string{"0", "-1", "abc", "99999999999"} {
		if _, err := loadAPI(apiEnv(map[string]string{"APP_ENV": "test", "WS_MAX_MESSAGE_BYTES": bad})); err == nil {
			t.Errorf("WS_MAX_MESSAGE_BYTES=%q accepted", bad)
		}
	}
}
