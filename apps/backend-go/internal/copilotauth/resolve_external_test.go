package copilotauth_test

import (
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/copilotauth"
)

// Outside the package a Resolved can only be zero-valued or hand-built with
// exported fields; neither may yield "no user" for a tool call.
func TestHandBuiltResolvedFailsToolUserID(t *testing.T) {
	for _, r := range []copilotauth.Resolved{
		{},
		{WorkspaceID: "w", Slug: "s"},
	} {
		if id, err := r.ToolUserID(); err == nil {
			t.Fatalf("expected error, got %v", id)
		}
		if r.IsSelfHost() {
			t.Fatal("must not be self-host")
		}
	}
}
