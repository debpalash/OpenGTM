package collect

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
)

func TestTenantComesFromClaim(t *testing.T) {
	payload := func(ws string) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"job_id": "j1", "query": "q", "workspace_id": ws, "slug": "s"})
		return b
	}
	var p collectPayload
	if _, err := claimTenant(&queue.Claim{Payload: payload("ws1")}, &p, &p.WorkspaceID, &p.Slug); !queue.IsPermanent(err) {
		t.Fatalf("claim without workspace: err = %v, want permanent", err)
	}
	if _, err := claimTenant(&queue.Claim{WorkspaceID: "ws2", Payload: payload("ws1")}, &p, &p.WorkspaceID, &p.Slug); !queue.IsPermanent(err) {
		t.Fatalf("mismatched payload: err = %v, want permanent", err)
	}
	ws, err := claimTenant(&queue.Claim{WorkspaceID: "ws1", Payload: payload("")}, &p, &p.WorkspaceID, &p.Slug)
	if err != nil || ws != "ws1" {
		t.Fatalf("payload without workspace: ws %q err %v", ws, err)
	}
	ws, err = claimTenant(&queue.Claim{WorkspaceID: "ws1", Payload: payload("ws1")}, &p, &p.WorkspaceID, &p.Slug)
	if err != nil || ws != "ws1" {
		t.Fatalf("matching: ws %q err %v", ws, err)
	}
	// HandleCollect refuses before touching the database (nil pool).
	if err := HandleCollect(context.Background(), Deps{}, &queue.Claim{Payload: payload("ws1")}); !queue.IsPermanent(err) {
		t.Fatalf("HandleCollect: %v", err)
	}
	if err := HandleBulkEnrich(context.Background(), Deps{}, &queue.Claim{Payload: payload("ws1")}); !queue.IsPermanent(err) {
		t.Fatalf("HandleBulkEnrich: %v", err)
	}
}

func TestFailureHookNeedsJobWorkspace(t *testing.T) {
	h := failureHook(nil) // a nil pool would panic if the hook reached the database
	if err := h(context.Background(), queue.FailureNotice{JobType: JobTypeCollect,
		Payload: []byte(`{"job_id":"j1","workspace_id":"ws1","slug":"s"}`), Reason: "boom"}); err != nil {
		t.Fatalf("empty job workspace: err = %v, want nil (nothing done)", err)
	}
	if err := h(context.Background(), queue.FailureNotice{JobType: JobTypeCollect, WorkspaceID: "ws2",
		Payload: []byte(`{"job_id":"j1","workspace_id":"ws1","slug":"s"}`)}); err == nil {
		t.Fatal("mismatched payload workspace: want error")
	}
}
