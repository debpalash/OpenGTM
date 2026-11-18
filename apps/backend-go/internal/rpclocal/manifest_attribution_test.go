package rpclocal

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/credentials"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providerrun"
)

// Issue 114 closure (declarative provider attribution): the outer provider-call result of a manifest provider names
// the manifest provider, on success and on a vendor failure, whatever "provider"/"src" the vendor's own body claims.
// The original defect omitted provider=self.name on the declarative path.
func TestDeclarativeProviderOuterResultCarriesProviderName(t *testing.T) {
	body := `{"message":"found","mobile_number":"+15550001","provider":"evil_vendor","src":"evil_vendor"}`
	status := http.StatusOK
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test server
	}}
	idx := credentials.ConnectorIndex(func(string) (string, bool) { return "", false }, nil)
	keys := wsKeys{"ws1": {"LEADMAGIC_API_KEY": "lm-secret-key", "PROSPEO_API_KEY": "pr-secret-key"}}
	run := providerrun.New(providerrun.Deps{HTTP: client, Secrets: keys, Manifests: idx, URLCheck: func(string) error { return nil }})
	c := New(Deps{Local: func(string) bool { return true }, Runner: run, Secrets: keys})

	type outer struct {
		Provider string         `json:"provider"`
		Success  bool           `json:"success"`
		Fields   map[string]any `json:"fields"`
		Error    string         `json:"error"`
	}
	decode := func(raw json.RawMessage) outer {
		t.Helper()
		var o outer
		if err := json.Unmarshal(raw, &o); err != nil {
			t.Fatalf("outer result %s: %v", raw, err)
		}
		return o
	}

	for _, name := range []string{"leadmagic_mobile", "prospeo_mobile"} {
		body, status = `{"message":"found","mobile_number":"+15550001","provider":"evil_vendor","src":"evil_vendor","response":{"raw_format":"+15550001"}}`, http.StatusOK
		res, err := c.Call(context.Background(), call(name, "attr-ok-"+name, `{"linkedin_url":"https://www.linkedin.com/in/x"}`))
		if err != nil || res.Status != "ok" {
			t.Fatalf("%s: %+v %v", name, res, err)
		}
		if o := decode(res.Output); o.Provider != name || !o.Success || o.Fields["mobile_phone"] != "+15550001" {
			t.Fatalf("%s success: outer provider %q success %v fields %v (raw %s)", name, o.Provider, o.Success, o.Fields, res.Output)
		}

		// A vendor failure still names the manifest provider (the caller records the miss against it).
		body, status = `{"error":"nope","provider":"evil_vendor"}`, http.StatusBadRequest
		res, err = c.Call(context.Background(), call(name, "attr-fail-"+name, `{"linkedin_url":"https://www.linkedin.com/in/x"}`))
		if err != nil || res.Status != "error" {
			t.Fatalf("%s failure: %+v %v", name, res, err)
		}
		if o := decode(res.Output); o.Provider != name || o.Success {
			t.Fatalf("%s failure: outer provider %q success %v (raw %s)", name, o.Provider, o.Success, res.Output)
		}
	}
}
