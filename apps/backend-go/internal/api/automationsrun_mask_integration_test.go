//go:build integration

package api

import (
	"strings"
	"testing"
)

// Final review M2 (S-L3): preview masks the resolved webhook URL for a caller without
// automations.manage, as GET and list mask the stored one; a manager sees it in full.
func TestPreviewMasksWebhookURLForNonManagers(t *testing.T) {
	e := newArunEnv(t, true, nil)
	body := `{"limit":1}`
	member := e.call("bob", "ws-alpha", "POST", "/api/automations/triggers/trg-a-01/preview", body)
	_, sample := arunMatched(t, member)
	if len(sample) != 1 {
		t.Fatalf("%s", member.Body)
	}
	if strings.Contains(string(member.Body), "/auto/") {
		t.Fatalf("member preview leaked the webhook path: %s", member.Body)
	}
	if !strings.Contains(string(member.Body), `"url":"https://hooks.example.com/***"`) {
		t.Fatalf("member preview not masked: %s", member.Body)
	}
	owner := e.call("alice", "ws-alpha", "POST", "/api/automations/triggers/trg-a-01/preview", body)
	if !strings.Contains(string(owner.Body), `"url":"https://hooks.example.com/auto/Hot Pursuit Labs"`) {
		t.Fatalf("manager preview masked: %s", owner.Body)
	}
}
