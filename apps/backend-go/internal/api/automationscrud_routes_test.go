package api

import (
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// The seven CRUD routes of Task 17 register behind GO_AUTOMATION_ROUTES_ENABLED
// with exactly the Python paths and methods.
func TestAutoCRUDRoutesRegistered(t *testing.T) {
	want := []string{
		"POST /api/automations/triggers",
		"GET /api/automations/triggers",
		"GET /api/automations/triggers/{trigger_id}",
		"PATCH /api/automations/triggers/{trigger_id}",
		"DELETE /api/automations/triggers/{trigger_id}",
		"POST /api/automations/triggers/{trigger_id}/pause",
		"POST /api/automations/triggers/{trigger_id}/resume",
	}
	r := httpx.NewRouter()
	registerAutomationsCRUD(r, autoDeps(true, true, false))
	got := map[string]bool{}
	for _, p := range r.Patterns() {
		got[p] = true
	}
	for _, p := range want {
		if !got[p] {
			t.Errorf("route %q not registered", p)
		}
	}
	if len(got) != len(want) {
		t.Errorf("registered %d CRUD routes %v, want %d", len(got), r.Patterns(), len(want))
	}
}
