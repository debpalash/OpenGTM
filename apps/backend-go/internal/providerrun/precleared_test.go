package providerrun_test

import (
	"context"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providerrun"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

const preclearedVerdict = `{"verdict":"consent_needed","confidence":"medium",
 "reasons":[{"rule_key":"gb_unknown_form_default","text":"No company could be matched.","source_title":"PECR 2003 regulation 22","source_url":"https://www.legislation.gov.uk/uksi/2003/2426/regulation/22"}],
 "rules_applied":["gb_unknown_form_default"],"footer_text":null,"next_step":"Do not cold email.",
 "facts":{"recipient_country":"GB"},
 "receipt":{"id":"cc_rcpt_0000000000000009","created_at":"2026-10-29T12:00:00.000Z","verify_url":"https://precleared.dev/api/v1/receipt?id=cc_rcpt_0000000000000009"},
 "rules_version":"v1","disclaimer":"Decision support with sources, not legal advice."}`

// The compliance check runs only with the workspace's own key, sends the workspace's declared
// context verbatim, and returns the verdict as the column value with the rest in Extra.
func TestPreclearedRunsOnlyWithTheWorkspacesKey(t *testing.T) {
	sim := newVendorSim(t, func(string, map[string]any) string { return preclearedVerdict })
	r := providerrun.New(providerrun.Deps{
		HTTP: sim.client(),
		Secrets: wsSecrets{"ws1": {
			"PRECLEARED_API_KEY": "pc_live_ws1", "PRECLEARED_SENDER_COUNTRY": "GB", "PRECLEARED_ADDRESS_SOURCE": "published",
		}},
		Manifests: builtinIndex(t), URLCheck: func(string) error { return nil },
	})
	if !r.Serves("precleared_compliance") {
		t.Fatal("precleared_compliance is not served")
	}
	in := providers.Input{Lead: map[string]any{"email": "owner@example-plumbing.co.uk", "company": "Example Plumbing"}, Field: "compliance"}
	out, err := r.Run(context.Background(), "ws1", "precleared_compliance", in, 0)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Error != "" || out.Value != "consent_needed" || out.Extra == nil {
		t.Fatalf("output %+v", out)
	}
	for k, want := range map[string]string{
		"compliance_rule": "gb_unknown_form_default", "compliance_next_step": "Do not cold email.",
		"compliance_receipt_id": "cc_rcpt_0000000000000009", "compliance_rules_version": "v1",
		"compliance_checked_at": "2026-10-29T12:00:00.000Z",
	} {
		if v, _ := out.Extra.Get(k); v != want {
			t.Errorf("%s = %v, want %q", k, v, want)
		}
	}
	calls := sim.calls()
	if len(calls) != 1 || calls[0].host != "precleared.dev" || calls[0].path != "/api/v1/check" {
		t.Fatalf("calls %+v", calls)
	}
	c := calls[0]
	if c.header.Get("Authorization") != "Bearer pc_live_ws1" {
		t.Fatal("request did not carry the workspace's key")
	}
	wantBody := map[string]any{"recipient_email": "owner@example-plumbing.co.uk", "company_name": "Example Plumbing",
		"sender_country": "GB", "address_source": "published", "pitch": ""}
	for k, v := range wantBody {
		if c.body[k] != v {
			t.Errorf("body %s = %v, want %q", k, c.body[k], v)
		}
	}
	if len(c.body) != len(wantBody) {
		t.Errorf("body has %d fields, want %d: %v", len(c.body), len(wantBody), c.body)
	}

	// A workspace that has not saved its own key is refused before any network I/O.
	out, err = r.Run(context.Background(), "ws2", "precleared_compliance", in, 0)
	if err != nil || out.Error != "precleared_compliance: missing PRECLEARED_API_KEY" {
		t.Fatalf("ws2 output %+v %v", out, err)
	}
	if p, err := r.Precheck(context.Background(), "ws2", "precleared_compliance", in.Lead); err != nil || p == "" {
		t.Fatalf("precheck ws2 %q %v", p, err)
	}
	if ok, err := r.Configured(context.Background(), "ws2", "precleared_compliance"); err != nil || ok {
		t.Fatalf("configured ws2 %v %v", ok, err)
	}
	if n := len(sim.calls()); n != 1 {
		t.Fatalf("vendor calls %d, want 1", n)
	}
}
