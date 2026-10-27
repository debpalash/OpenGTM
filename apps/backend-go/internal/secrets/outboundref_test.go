package secrets

import (
	"errors"
	"testing"
)

func TestCheckOutboundRef(t *testing.T) {
	reg := func(n string) bool { return n == "WEBHOOK_PROVIDER_KEY" || n == "OPENAI_API_KEY" }
	cases := []struct {
		name string
		p    OutboundPolicy
		ok   bool
	}{
		{"WEBHOOK_TOKEN", OutboundPolicy{}, true},
		{"AUTOMATION_X", OutboundPolicy{}, true},
		{"DEST_X", OutboundPolicy{}, true},
		{"SLACK_HOOK", OutboundPolicy{}, true},
		{"HUBSPOT_ACCESS_TOKEN", OutboundPolicy{}, false},
		{"OPENAI_API_KEY", OutboundPolicy{Registered: reg}, false},
		{"WEBHOOK_PROVIDER_KEY", OutboundPolicy{Registered: reg}, false},
		{"WEBHOOK_SMTP_PASSWORD", OutboundPolicy{}, true},
		{"SMTP_PASSWORD", OutboundPolicy{Prefixes: []string{"SMTP_"}}, false},
		{"webhook_token", OutboundPolicy{}, false},
		{" WEBHOOK_X", OutboundPolicy{}, false},
		{"", OutboundPolicy{}, false},
		{"CUSTOM_X", OutboundPolicy{Prefixes: []string{"CUSTOM_"}}, true},
		{"WEBHOOK_X", OutboundPolicy{Prefixes: []string{"CUSTOM_"}}, false},
	}
	for _, c := range cases {
		err := CheckOutboundRef(c.name, c.p)
		if c.ok && err != nil || !c.ok && !errors.Is(err, ErrOutboundRefNotAllowed) {
			t.Errorf("%q: err=%v want ok=%v", c.name, err, c.ok)
		}
	}
	if ErrOutboundRefNotAllowed.Error() != "header_secret_ref is not an available webhook secret" {
		t.Fatal("text")
	}
}
