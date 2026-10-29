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

// Final review L9/L10: the fixed deny-list covers the server's own secret families whatever the
// prefix list allows, and a policy built without a credential registry fails closed.
func TestOutboundDenyListAndNilRegistry(t *testing.T) {
	wide := OutboundPolicy{Prefixes: []string{"O", "S", "J", "D", "R", "I", "A"}, Registered: func(string) bool { return false }}
	for _, n := range []string{"OIDC_CLIENT_SECRET", "STRIPE_SECRET_KEY", "SECRET_KEY", "JWT_SECRET", "DATABASE_URL",
		"REDIS_URL", "INSTANTLY_API_KEY", "SMARTLEAD_API_KEY", "AIRTABLE_TOKEN"} {
		if err := CheckOutboundRef(n, wide); !errors.Is(err, ErrOutboundRefNotAllowed) {
			t.Errorf("%s allowed", n)
		}
	}
	none := NewOutboundPolicy(nil, nil)
	for _, n := range []string{"WEBHOOK_TOKEN", "SLACK_HOOK"} {
		if err := CheckOutboundRef(n, none); !errors.Is(err, ErrOutboundRefNotAllowed) {
			t.Errorf("%s allowed without a registry", n)
		}
	}
	reg := NewOutboundPolicy([]string{"HOOK_"}, func(n string) bool { return n == "HOOK_REG" })
	if CheckOutboundRef("HOOK_OK", reg) != nil || CheckOutboundRef("HOOK_REG", reg) == nil || CheckOutboundRef("WEBHOOK_X", reg) == nil {
		t.Error("registry policy")
	}
	if p := NewOutboundPolicy(nil, func(string) bool { return false }); CheckOutboundRef("WEBHOOK_X", p) != nil {
		t.Error("default prefixes")
	}
}
