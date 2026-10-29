package autosvc

import (
	"context"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

type fakeChecker map[string]bool

func (f fakeChecker) Has(_ context.Context, _, key string) (bool, error) { return f[key], nil }

func whCfg(kv ...any) *wbcrud.Obj {
	o := wbcrud.NewObj()
	for i := 0; i+1 < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

// S-M1: an allowed prefix passes; a CRM key, a provider key, a key outside the
// namespace and a missing key all get the same refusal text.
func TestWebhookSecretRefPolicy(t *testing.T) {
	have := fakeChecker{"WEBHOOK_TOKEN": true, "HUBSPOT_ACCESS_TOKEN": true, "OPENAI_API_KEY": true, "RANDOM": true, "WEBHOOK_REG": true}
	cfg := Config{
		Secrets: have,
		Outbound: secrets.OutboundPolicy{Registered: func(n string) bool {
			return n == "OPENAI_API_KEY" || n == "WEBHOOK_REG"
		}},
	}
	const want = "header_secret_ref is not an available webhook secret"
	cases := []struct {
		ref string
		ok  bool
	}{
		{"WEBHOOK_TOKEN", true},
		{"HUBSPOT_ACCESS_TOKEN", false},
		{"OPENAI_API_KEY", false},
		{"RANDOM", false},
		{"WEBHOOK_REG", false},
		{"WEBHOOK_MISSING", false},
	}
	for _, c := range cases {
		err := validateWebhookAction(context.Background(), nil, "ws", 0, whCfg("url", "https://h.example.com/x", "header_secret_ref", c.ref), cfg)
		if c.ok {
			if err != nil {
				t.Errorf("%s: %v", c.ref, err)
			}
			continue
		}
		he, ok := err.(*HTTPError)
		if !ok || he.Status != 422 || he.Detail != want {
			t.Errorf("%s: %#v", c.ref, err)
		}
	}
}

// S-L1: the scheme and authority must be literal; placeholders in the path
// and query stay allowed.
func TestWebhookTemplatedHostRefused(t *testing.T) {
	cases := []struct {
		url string
		ok  bool
	}{
		{"https://x.com/{id}", true},
		{"https://x.com/h?k={email}", true},
		{"https://{website}/h", false},
		{"https://x.{tld}/h", false},
		{"{scheme}://x.com/h", false},
		{"https://{user}@x.com/h", false},
		{"https://x.com:{port}/h", false},
		{"{url}", false},
		{"https://x.com{path}", false},
	}
	for _, c := range cases {
		err := validateWebhookAction(context.Background(), nil, "ws", 0, whCfg("url", c.url), Config{})
		if c.ok != (err == nil) {
			t.Errorf("%s: ok=%v err=%v", c.url, c.ok, err)
		}
		if !c.ok && err != nil && (err.(*HTTPError).Status != 422 || !strings.Contains(err.Error(), "literal")) {
			t.Errorf("%s: %v", c.url, err)
		}
	}
	// The allowlist runs on the literal host.
	err := validateWebhookAction(context.Background(), nil, "ws", 0, whCfg("url", "https://evil.com/{id}"), Config{WebhookAllowlist: []string{"x.com"}})
	if err == nil {
		t.Error("allowlist not applied to a literal host with a templated path")
	}
}
