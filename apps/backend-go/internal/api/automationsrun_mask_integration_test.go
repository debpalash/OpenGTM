//go:build integration

package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
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

// R-targets: oversized explicit targets are a 422 even when AUTOMATIONS_MAX_ROWS_PER_EVAL is
// unset in Deps (the engine's default cap of 500 then applies), never a silent truncation.
func TestRunTargetsOverDefaultCapAre422(t *testing.T) {
	e := newArunEnv(t, true, nil)
	r := httpx.NewRouter()
	registerAutomationsRun(r, Deps{Pool: e.pool, Signer: testSigner, Identity: e.st,
		Flags: config.Flags{PGLeadStore: true, AutomationsEnabled: true,
			Automations: config.AutomationsSettings{GoExecutorTriggerEval: true}}})
	e.h = r.Handler()
	ids := make([]string, 501)
	for i := range ids {
		ids[i] = fmt.Sprint(i + 1)
	}
	b, _ := json.Marshal(map[string]any{"row_ids": ids})
	got := e.call("alice", "", "POST", "/api/automations/triggers/trg-a-01/run", string(b))
	if got.Status != 422 || !strings.Contains(string(got.Body), `"max_length":500`) {
		t.Fatalf("%d %s", got.Status, got.Body)
	}
}
