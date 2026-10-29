package playbooks

import (
	"context"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
)

func TestRunHandlerRequiresWorkspaceAndRun(t *testing.T) {
	h := RunHandler(Deps{}, nil)
	for _, payload := range []string{`{}`, `{"workspace_id":"ws"}`, `{"run_id":"r"}`, `{"workspace_id":"","run_id":"r"}`, `x`} {
		err := h(context.Background(), &queue.Claim{ID: 1, Type: RunJobType, WorkspaceID: "ws", Payload: []byte(payload)})
		if err == nil || !strings.Contains(err.Error(), "requires workspace_id and run_id") {
			t.Errorf("payload %q: err = %v", payload, err)
		}
	}
}

func TestScheduleHandlerRequiresWorkspaceAndPlaybook(t *testing.T) {
	h := ScheduleHandler(Deps{})
	for _, payload := range []string{`{}`, `{"workspace_id":"ws"}`, `{"playbook_id":"p"}`, `x`} {
		err := h(context.Background(), &queue.Claim{ID: 1, Type: ScheduleJobType, WorkspaceID: "ws", Payload: []byte(payload)})
		if err == nil || !strings.Contains(err.Error(), "requires workspace_id and playbook_id") {
			t.Errorf("payload %q: err = %v", payload, err)
		}
	}
}

func TestHandlersRejectForeignWorkspace(t *testing.T) {
	c := &queue.Claim{ID: 1, WorkspaceID: "ws-a", Payload: []byte(`{"workspace_id":"ws-b","run_id":"r","playbook_id":"p"}`)}
	if err := RunHandler(Deps{}, nil)(context.Background(), c); err == nil || !queue.IsPermanent(err) {
		t.Errorf("run: err = %v, want permanent mismatch", err)
	}
	if err := ScheduleHandler(Deps{})(context.Background(), c); err == nil || !queue.IsPermanent(err) {
		t.Errorf("schedule: err = %v, want permanent mismatch", err)
	}
}

func TestFailureHookIgnoresMismatchedPayload(t *testing.T) {
	hook := FailureHook(Deps{})
	for name, n := range map[string]queue.FailureNotice{
		"foreign":   {JobID: 1, WorkspaceID: "ws-a", Payload: []byte(`{"workspace_id":"ws-b","run_id":"r"}`)},
		"malformed": {JobID: 1, WorkspaceID: "ws-a", Payload: []byte(`nope`)},
		"no run":    {JobID: 1, WorkspaceID: "ws-a", Payload: []byte(`{"workspace_id":"ws-a"}`)},
	} {
		if err := hook(context.Background(), n); err != nil {
			t.Errorf("%s: err = %v, want nil no-op", name, err)
		}
	}
}
