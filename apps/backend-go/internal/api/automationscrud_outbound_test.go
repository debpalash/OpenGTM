//go:build integration

package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

func putAutoSecret(t *testing.T, e autoEnv, key string) {
	t.Helper()
	err := tenancy.InWorkspace(context.Background(), e.pool, "ws-alpha", func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `INSERT INTO workspace_settings (workspace_id, key, value) VALUES ('ws-alpha', $1, 'x')`, key)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

const outboundBody = `{"name":"wh","trigger_type":"on_row_added","actions":[{"type":"webhook","config":{"url":"https://hooks.example.com/services/T0/SECRETPATH?k=v","headers":{"Authorization":"Bearer abc"},"header_secret_ref":"AUTOMATION_HOOK_SECRET"}}]}`

// S-M1: a ref outside the outbound namespace, a registered credential and a missing one all get the same 422.
func TestAutoCreateOutboundSecretRefPolicy(t *testing.T) {
	e := newAutoEnv(t, "default")
	putAutoSecret(t, e, "HUBSPOT_ACCESS_TOKEN")
	putAutoSecret(t, e, "OPENAI_API_KEY")
	putAutoSecret(t, e, "RANDOM")
	for _, ref := range []string{"HUBSPOT_ACCESS_TOKEN", "OPENAI_API_KEY", "AUTOMATION_NOPE", "RANDOM"} {
		body := strings.Replace(outboundBody, "AUTOMATION_HOOK_SECRET", ref, 1)
		code, out, _ := e.send("alice", "ws-alpha", "POST", "/api/automations/triggers", body)
		if code != 422 || !strings.Contains(string(out), "header_secret_ref is not an available webhook secret") || strings.Contains(string(out), ref) {
			t.Errorf("%s: %d %.200s", ref, code, out)
		}
	}
	if code, out, _ := e.send("alice", "ws-alpha", "POST", "/api/automations/triggers", outboundBody); code != 200 {
		t.Fatalf("allowed ref: %d %.200s", code, out)
	}
}

// S-L3: members without automations.manage see headers and URL path masked; admins see them in full.
func TestAutoReadMasksWebhookConfigForViewers(t *testing.T) {
	e := newAutoEnv(t, "default")
	code, out, _ := e.send("alice", "ws-alpha", "POST", "/api/automations/triggers", outboundBody)
	if code != 200 {
		t.Fatalf("create: %d %.200s", code, out)
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(out, &created)
	get := "/api/automations/triggers/" + created.ID
	for _, who := range []string{"bob", "erin"} {
		for _, p := range []string{get, "/api/automations/triggers"} {
			_, b, _ := e.send(who, "ws-alpha", "GET", p, "")
			s := string(b)
			if strings.Contains(s, "SECRETPATH") || strings.Contains(s, "Bearer abc") || strings.Contains(s, "k=v") || !strings.Contains(s, "AUTOMATION_HOOK_SECRET") {
				t.Errorf("%s GET %s: %.400s", who, p, s)
			}
			if strings.Contains(s, "hooks.example.com/services") {
				t.Errorf("%s: path visible: %.400s", who, s)
			}
		}
	}
	for _, p := range []string{get, "/api/automations/triggers"} {
		_, b, _ := e.send("alice", "ws-alpha", "GET", p, "")
		if s := string(b); !strings.Contains(s, "SECRETPATH") || !strings.Contains(s, "Bearer abc") {
			t.Errorf("admin GET %s masked: %.400s", p, s)
		}
	}
}
