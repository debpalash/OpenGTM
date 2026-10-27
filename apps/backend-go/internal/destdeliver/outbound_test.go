package destdeliver

import (
	"context"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

func TestCheckRef(t *testing.T) {
	p := secrets.OutboundPolicy{}
	for _, tc := range []struct {
		typ, ref string
		ok       bool
	}{
		{"slack", "SLACK_HOOK", true}, {"slack", "DEST_X", true},
		{"slack", "WEBHOOK_X", false}, // allowed for webhooks, not the Slack namespace
		{"slack", "HUBSPOT_API_KEY", false}, {"slack", "", false},
		{"warehouse_http", "DEST_WH", true}, {"warehouse_http", "WEBHOOK_WH", true},
		{"warehouse_http", "HUBSPOT_API_KEY", false}, {"warehouse_http", "OPENAI_API_KEY", false},
		{"webhook", "AUTOMATION_X", true}, {"webhook", "SMTP_PASSWORD", false},
	} {
		if err := CheckRef(tc.typ, tc.ref, p, nil); (err == nil) != tc.ok {
			t.Errorf("%s %q: %v", tc.typ, tc.ref, err)
		}
	}
}

// S-M2: a stored Slack destination whose ref sits outside the namespace sends nothing, even though
// the workspace holds a secret of that name.
func TestSlackRefOutsideNamespaceFailsClosed(t *testing.T) {
	w := &countWebhook{}
	d := New(Ports{Secrets: srcResolver{"workspace"}, Webhook: w}, time.Second) // production policy
	for _, ref := range []string{"HUBSPOT_API_KEY", "WEBHOOK_URL", "ANY"} {
		r := d.Deliver(context.Background(), Destination{ID: "d", WorkspaceID: "w", Type: "slack",
			Config: map[string]any{"webhook_secret_ref": ref}}, 1, nil, "i")
		if r.Success || r.Error != "Slack webhook secret not found" || w.n != 0 {
			t.Errorf("%s: %+v sent=%d", ref, r, w.n)
		}
	}
	if r := d.Deliver(context.Background(), Destination{ID: "d", WorkspaceID: "w", Type: "slack",
		Config: map[string]any{"webhook_secret_ref": "SLACK_OPS"}}, 1, nil, "i"); !r.Success || w.n != 1 {
		t.Errorf("allowed ref: %+v", r)
	}
}
