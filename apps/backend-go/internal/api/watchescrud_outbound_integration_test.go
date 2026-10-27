//go:build integration

package api

import (
	"context"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/settings"
)

// S-M1 for the watch webhook ref: OUTBOUND_SECRET_REF_PREFIXES and the registered-credential deny-list apply.
func TestWatchWebhookRefOutboundPolicy(t *testing.T) {
	reg, err := settings.Load()
	if err != nil || len(reg.SecretAppKeys) == 0 {
		t.Fatalf("registry: %v", err)
	}
	key := reg.SecretAppKeys[0]
	body := func(ref, target string) string {
		return `{"kind":"feed","target":"https://feeds.example.com/` + target + `.xml","create_webhook_rule":true,"webhook_url":"https://hooks.example.com/x","webhook_secret_ref":"` + ref + `"}`
	}
	// env prefixes replace the defaults: the default-namespace ref is refused
	e := newWcrudEnv(t, "default", func(d *Deps) {
		d.Registry = reg
		d.Getenv = func(k string) string {
			if k == "OUTBOUND_SECRET_REF_PREFIXES" {
				return "CUSTOM_," + key
			}
			return ""
		}
	})
	if code, m, raw := e.as("alice", "POST", "/api/watches", body("AUTOMATION_HOOK_SECRET", "a")); code != 422 || m["detail"] != "webhook_secret_ref not found" {
		t.Errorf("default-namespace ref with env prefixes: %d %s", code, raw)
	}
	// a registered credential is refused even when its prefix is allowed
	envl, _ := secrets.NewEnvelope(secrets.Config{MasterKey: "watch-crud-test-key", AppEnv: "test"}, nil)
	if err := secrets.NewResolver(envl, secrets.NewPgStore(e.pool), func(string) string { return "" }).Set(context.Background(), "ws-alpha", key, "v"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		wcrudOwnerExecQuiet(`DELETE FROM workspace_settings WHERE key = '` + strings.ReplaceAll(key, "'", "") + `'`)
	})
	if code, m, raw := e.as("alice", "POST", "/api/watches", body(key, "b")); code != 422 || m["detail"] != "webhook_secret_ref not found" {
		t.Errorf("registered credential %s: %d %s", key, code, raw)
	}
}
