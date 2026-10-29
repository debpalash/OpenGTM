package destdeliver

import (
	"context"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/crmpush"
	"strings"
	"testing"
)

type fakeWebhook struct{}

func (fakeWebhook) Send(context.Context, WebhookRequest) (Result, error) { return Result{}, nil }

type fakeCRM struct{}

func (fakeCRM) Push(context.Context, string, map[string]any, map[string]any, string, int64) crmpush.Result {
	return crmpush.Result{}
}

type fakeSecrets struct{}

func (fakeSecrets) Get(context.Context, string, string) (string, string, error) { return "", "", nil }

// Every request carries the workspace; the CRM port mirrors crmpush.Push and Ports holds the secret resolver.
func TestPortShapes(t *testing.T) {
	_ = WebhookRequest{WorkspaceID: "w"}
	_ = CampaignRequest{WorkspaceID: "w", LeadID: 1}
	_ = RowUpsert{WorkspaceID: "w", LeadID: 1}
	var _ CRMPusher = fakeCRM{}
	_ = Ports{Secrets: fakeSecrets{}}
}

func TestPortsSupports(t *testing.T) {
	var none Ports
	for typ, want := range map[string]bool{
		"meta_ads": true, "google_ads": true, "linkedin_ads": true, "warehouse_http": true,
		"webhook": false, "slack": false, "hubspot": false, "salesforce": false,
		"instantly": false, "smartlead": false, "google_sheets": false, "airtable": false, "nope": false,
	} {
		if got := none.Supports(typ); got != want {
			t.Errorf("empty ports Supports(%q)=%v want %v", typ, got, want)
		}
	}
	p := Ports{Webhook: fakeWebhook{}, CRM: fakeCRM{}}
	for _, typ := range []string{"webhook", "slack", "hubspot", "salesforce"} {
		if !p.Supports(typ) {
			t.Errorf("Supports(%q) false with port set", typ)
		}
	}
	if !p.Supports(" Webhook ") {
		t.Error("type match must be case/space-insensitive")
	}
}

func TestGoDestTypesRequirePorts(t *testing.T) {
	p := Ports{Webhook: fakeWebhook{}}
	if err := p.ValidateTypes([]string{"webhook", "slack", "meta_ads"}); err != nil {
		t.Fatalf("supported types rejected: %v", err)
	}
	if err := p.ValidateTypes(nil); err != nil {
		t.Fatalf("empty list must pass: %v", err)
	}
	err := p.ValidateTypes([]string{"webhook", "hubspot"})
	if err == nil || !strings.Contains(err.Error(), "hubspot") {
		t.Fatalf("missing CRM port must be a fatal config error naming the type, got %v", err)
	}
	if err := p.ValidateTypes([]string{"bogus"}); err == nil {
		t.Fatal("unknown type accepted")
	}
}
