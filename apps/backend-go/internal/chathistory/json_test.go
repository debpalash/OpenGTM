package chathistory

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
)

// Key order is Python's dict order (the recorded fixtures sort keys, so order
// is asserted against literal strings, shape against the fixtures).
func TestHistoryJSONMatchesFixtures(t *testing.T) {
	fx := contract.Load(t, "copilot/conv_get_owner_contacts")
	var body struct {
		Conversation Conversation `json:"conversation"`
		Messages     []Message    `json:"messages"`
	}
	if err := json.Unmarshal(fx.Response.Body, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Messages) < 4 || body.Conversation.ID != "conv-ed-2" {
		t.Fatalf("fixture not decoded: %+v", body)
	}
	// round trip: re-encoded structs equal the fixture structurally
	out, _ := json.Marshal(body)
	contract.AssertEqual(t, contract.Resp{Status: 200, Headers: fx.Response.Headers, Body: out}, fx.Response)

	c, _ := json.Marshal(body.Conversation)
	if want := `{"id":"conv-ed-2","title":"Saved contacts","created_at":"2026-08-29 08:00:00","updated_at":"2026-08-29 08:05:00"}`; string(c) != want {
		t.Fatalf("conversation key order: %s", c)
	}
	m, _ := json.Marshal(body.Messages[0])
	if !strings.HasPrefix(string(m), `{"id":"msg-ed2-1","conversation_id":"conv-ed-2","role":"user","content":"Find work emails for them","tool_data":null,"created_at":`) {
		t.Fatalf("message key order: %s", m)
	}
}
