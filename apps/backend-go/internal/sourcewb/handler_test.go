package sourcewb

import (
	"context"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
)

// The tenant comes only from the claim (jobs.workspace_id); the payload may not widen or
// change it.
func TestHandleRefusesUntrustedTenant(t *testing.T) {
	for _, c := range []struct {
		name, claimWS, payload string
	}{
		{"empty_claim_workspace", "", `{"workbook_id":"wb","column_id":"c","workspace_id":"ws-a"}`},
		{"empty_claim_and_payload", "", `{"workbook_id":"wb","column_id":"c"}`},
		{"mismatched_payload", "ws-a", `{"workbook_id":"wb","column_id":"c","workspace_id":"ws-b"}`},
	} {
		err := Handle(context.Background(), Deps{}, &queue.Claim{ID: 1, Type: JobType, WorkspaceID: c.claimWS, Payload: []byte(c.payload)})
		if !queue.IsPermanent(err) {
			t.Errorf("%s: err %v, want a permanent refusal", c.name, err)
		}
	}
}
