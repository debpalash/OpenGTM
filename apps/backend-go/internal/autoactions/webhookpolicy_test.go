package autoactions

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

// S-M1 at delivery: a stored rule whose ref is outside the outbound namespace or
// is a registered credential sends nothing and fails.
func TestWebhookDeliveryRefusesDisallowedRef(t *testing.T) {
	for _, ref := range []string{"HUBSPOT_ACCESS_TOKEN", "OPENAI_API_KEY", "RANDOM", "WEBHOOK_REG"} {
		h := newHarness(t, nil)
		h.deps.Outbound = secrets.OutboundPolicy{Registered: func(n string) bool { return n == "OPENAI_API_KEY" || n == "WEBHOOK_REG" }}
		h.sec.byWS = map[string]map[string]string{"ws-1": {ref: "ctk_secret_value_9"}}
		got := h.run(map[string]any{"url": "https://hooks.example.com/x", "header_secret_ref": ref})
		if got.Status != "failed" || h.hits.Load() != 0 || len(h.dials) != 0 {
			t.Errorf("%s: %+v hits=%d", ref, got, h.hits.Load())
		}
		if strings.Contains(got.Error+got.Summary, ref) || strings.Contains(got.Error+got.Summary, "ctk_secret") {
			t.Errorf("%s: result names the ref or value: %+v", ref, got)
		}
	}
	h := newHarness(t, nil)
	h.deps.Outbound = secrets.OutboundPolicy{}
	h.sec.byWS = map[string]map[string]string{"ws-1": {"WEBHOOK_TOKEN": "ctk_secret_value_9"}}
	if got := h.run(map[string]any{"url": "https://hooks.example.com/x", "header_secret_ref": "WEBHOOK_TOKEN"}); got.Status != "success" || h.last().Header.Get("Authorization") != "ctk_secret_value_9" {
		t.Fatalf("allowed ref: %+v", got)
	}
}

// S-L1 at delivery: a stored rule with a templated authority fails closed, and
// substituted path/query values are percent-encoded so they cannot move the host.
func TestWebhookTemplatedURLHostLiteral(t *testing.T) {
	h := newHarness(t, nil)
	lead := map[string]any{"company": "x", "email": "hooks.example.com"}
	got := Webhook(context.Background(), h.deps, "ws-1", map[string]any{"url": "https://{email}/x"}, lead, testCols)
	if got.Status != "failed" || h.hits.Load() != 0 || len(h.dials) != 0 {
		t.Fatalf("templated host: %+v", got)
	}

	var seen string
	h = newHarness(t, nil)
	h.handler = func(w http.ResponseWriter, r *http.Request) { seen = r.URL.RequestURI(); w.WriteHeader(200) }
	lead = map[string]any{"company": "a/../@evil", "email": "a b&c=d#e?f"}
	got = Webhook(context.Background(), h.deps, "ws-1", map[string]any{"url": "https://hooks.example.com/p/{company}?q={email}"}, lead, testCols)
	if got.Status != "success" || h.last().Host != "hooks.example.com" {
		t.Fatalf("encoded: %+v host=%q", got, h.last().Host)
	}
	u, _ := url.ParseRequestURI(seen)
	if u.EscapedPath() != "/p/a%2F..%2F%40evil" || u.Query().Get("q") != "a b&c=d#e?f" || len(u.Query()) != 1 {
		t.Fatalf("request uri %q", seen)
	}
}

// R2: a ref that is allowed but missing (or empty) at delivery sends nothing and fails with the shared text.
func TestWebhookDeliveryMissingRefFailsClosed(t *testing.T) {
	for _, byWS := range []map[string]map[string]string{nil, {"ws-1": {"WEBHOOK_GONE": ""}}, {"ws-2": {"WEBHOOK_GONE": "other"}}} {
		h := newHarness(t, nil)
		h.deps.Outbound = secrets.OutboundPolicy{}
		h.sec.byWS = byWS
		got := h.run(map[string]any{"url": "https://hooks.example.com/x", "header_secret_ref": "WEBHOOK_GONE"})
		if got.Status != "failed" || got.Error != secrets.OutboundRefMessage || h.hits.Load() != 0 || len(h.dials) != 0 {
			t.Errorf("%v: %+v", byWS, got)
		}
	}
	h := newHarness(t, nil)
	h.deps.Outbound = secrets.OutboundPolicy{}
	h.deps.Secrets = nil
	if got := h.run(map[string]any{"url": "https://hooks.example.com/x", "header_secret_ref": "WEBHOOK_X"}); got.Status != "failed" || h.hits.Load() != 0 {
		t.Errorf("nil getter: %+v", got)
	}
}
