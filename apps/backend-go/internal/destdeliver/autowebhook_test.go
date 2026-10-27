package destdeliver

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autoactions"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

type srcMap map[string][2]string // name -> value, source

func (s srcMap) Get(_ context.Context, _, key string) (string, string, error) {
	return s[key][0], s[key][1], nil
}

type hit struct {
	hdr  http.Header
	body string
}

func autoRig(t *testing.T, sec srcMap) (*Deliverer, *[]hit, func() int, string) {
	t.Helper()
	var mu sync.Mutex
	var hits []hit
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		hits = append(hits, hit{r.Header.Clone(), string(b)})
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	u := strings.TrimPrefix(srv.URL, "http://")
	host := u[:strings.LastIndex(u, ":")]
	wd := autoactions.NewWebhookDeps("test", []string{host}, config.AutomationsSettings{}, sec, nil)
	wd.Outbound = secrets.OutboundPolicy{}
	d := New(Ports{Secrets: sec, Webhook: AutoWebhook{Deps: wd}}, 5*time.Second)
	return d, &hits, func() int { mu.Lock(); defer mu.Unlock(); return len(hits) }, srv.URL
}

// F17: templates resolve against the field_map-mapped payload, so {org} works when company->org.
func TestWebhookTemplatesUseMappedPayload(t *testing.T) {
	d, hits, n, base := autoRig(t, srcMap{})
	dest := Destination{ID: "d", WorkspaceID: "w", AudienceID: "a", Type: "webhook",
		Config:   map[string]any{"url": base, "headers": map[string]any{"X-Org": "{org}"}},
		FieldMap: map[string]any{"company": "org"}}
	res := d.Deliver(context.Background(), dest, 7, map[string]any{"company": "Acme", "email": "a@b.co"}, "idem1")
	if !res.Success || n() != 1 {
		t.Fatalf("%+v hits=%d", res, n())
	}
	if got := (*hits)[0].hdr.Get("X-Org"); got != "Acme" {
		t.Errorf("X-Org %q", got)
	}
	if got := (*hits)[0].hdr.Get("Idempotency-Key"); got != "idem1" {
		t.Errorf("idempotency %q", got)
	}
}

// S-M3: a header ref that exists only globally sends no request; a workspace ref in the namespace is sent;
// a CRM-style ref (outside the namespace) sends nothing even when the workspace holds it.
func TestWebhookHeaderRefWorkspaceOnlyAndNamespaced(t *testing.T) {
	d, hits, n, base := autoRig(t, srcMap{
		"WEBHOOK_GLOBAL": {"gtok-1234", "global"}, "WEBHOOK_WS": {"wtok-1234", "workspace"}, "HUBSPOT_API_KEY": {"hub-1234", "workspace"}})
	mk := func(ref string) Destination {
		return Destination{ID: "d", WorkspaceID: "w", AudienceID: "a", Type: "webhook",
			Config: map[string]any{"url": base, "header_secret_ref": ref}}
	}
	for _, ref := range []string{"WEBHOOK_GLOBAL", "HUBSPOT_API_KEY", "WEBHOOK_NONE"} {
		if res := d.Deliver(context.Background(), mk(ref), 1, map[string]any{}, "i"); res.Success || n() != 0 {
			t.Errorf("%s: %+v hits=%d", ref, res, n())
		}
	}
	if res := d.Deliver(context.Background(), mk("WEBHOOK_WS"), 1, map[string]any{}, "i"); !res.Success || n() != 1 {
		t.Fatalf("workspace ref: %+v hits=%d", res, n())
	}
	if got := (*hits)[0].hdr.Get("Authorization"); got != "wtok-1234" {
		t.Errorf("Authorization %q", got)
	}
}

// A lead value that looks like a template ("{secret}", "{unknown}") is data: it is sent literally in the
// envelope and the Slack text, never substituted and never fails the delivery.
func TestLeadValuesAreNeverRetemplated(t *testing.T) {
	d, hits, n, base := autoRig(t, srcMap{"SLACK_HOOK": {"", "workspace"}})
	snap := map[string]any{"company": "{secret}", "note": "{unknown} and {company}", "secret": "LEAK"}
	res := d.Deliver(context.Background(), Destination{ID: "d", WorkspaceID: "w", AudienceID: "a", Type: "webhook",
		Config: map[string]any{"url": base}}, 1, snap, "i")
	if !res.Success || n() != 1 {
		t.Fatalf("%+v hits=%d", res, n())
	}
	b := (*hits)[0].body
	if !strings.Contains(b, `"company":"{secret}"`) || !strings.Contains(b, `{unknown} and {company}`) || strings.Contains(b, `"company":"LEAK"`) {
		t.Errorf("body %s", b)
	}
	// Slack: the message text is literal too.
	d2, hits2, n2, base2 := autoRig(t, srcMap{"SLACK_HOOK": {"", "workspace"}})
	d2.ports.Secrets = srcMap{"SLACK_HOOK": {base2, "workspace"}}
	res = d2.Deliver(context.Background(), Destination{ID: "d", WorkspaceID: "w", Type: "slack",
		Config: map[string]any{"webhook_secret_ref": "SLACK_HOOK", "message_template": "{note}"}}, 1, snap, "i")
	if !res.Success || n2() != 1 || !strings.Contains((*hits2)[0].body, "{unknown} and {company}") {
		t.Fatalf("slack %+v hits=%d", res, n2())
	}
}
