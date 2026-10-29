package retention

import (
	"context"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
)

// A claim whose payload lacks workspace_id fails like Python's ValueError, before any
// database access (the zero Deps has no pool).
func TestHandlerRequiresWorkspaceID(t *testing.T) {
	h := Handler(Deps{})
	for _, payload := range []string{`{}`, `{"workspace_id": ""}`, `{"workspace_id": null, "run_id": "r"}`, ``, `not json`} {
		err := h(context.Background(), &queue.Claim{ID: 1, Type: JobType, WorkspaceID: "ws", Payload: []byte(payload)})
		if err == nil || !strings.Contains(err.Error(), "requires workspace_id") {
			t.Errorf("payload %q: err = %v", payload, err)
		}
	}
}

// The payload workspace must be the job row's workspace; a mismatch is permanent and
// touches nothing.
func TestHandlerRejectsForeignWorkspace(t *testing.T) {
	h := Handler(Deps{})
	err := h(context.Background(), &queue.Claim{ID: 1, Type: JobType, WorkspaceID: "ws-a", Payload: []byte(`{"workspace_id":"ws-b"}`)})
	if err == nil || !queue.IsPermanent(err) {
		t.Fatalf("err = %v, want permanent mismatch", err)
	}
}

// The failure hook validates the payload workspace against the job row before acting;
// a foreign, missing or malformed payload is a no-op (the zero Deps would panic on use).
func TestFailureHookIgnoresMismatchedPayload(t *testing.T) {
	hook := FailureHook(Deps{})
	for name, n := range map[string]queue.FailureNotice{
		"foreign":   {JobID: 1, WorkspaceID: "ws-a", Payload: []byte(`{"workspace_id":"ws-b","run_id":"r"}`)},
		"malformed": {JobID: 1, WorkspaceID: "ws-a", Payload: []byte(`nope`)},
		"empty":     {JobID: 1, WorkspaceID: "ws-a", Payload: []byte(`{}`)},
	} {
		if err := hook(context.Background(), n); err != nil {
			t.Errorf("%s: err = %v, want nil no-op", name, err)
		}
	}
}
