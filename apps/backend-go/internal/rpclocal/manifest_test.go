package rpclocal

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/credentials"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/provcall"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providerrun"
)

type wsKeys map[string]map[string]string

func (k wsKeys) Get(_ context.Context, ws, key string) (string, string, error) {
	return k[ws][key], "workspace", nil
}

// TestDeclarativeProviderServedInProcess: the provider-call runner behind MCP, the poller, signal
// scan, collect and copilot serves a manifest-only provider (no Go implementation) through
// providerrun, and scrubs the manifest's credential from what it returns.
func TestDeclarativeProviderServedInProcess(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.ReadAll(r.Body)
		// A vendor that echoes the key: the caller must never see it.
		_, _ = io.WriteString(w, `{"message":"key `+r.Header.Get("X-API-Key")+`","mobile_number":"+15550001"}`)
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
	keys := wsKeys{"ws1": {"LEADMAGIC_API_KEY": "lm-secret-key"}}
	run := providerrun.New(providerrun.Deps{HTTP: client, Secrets: keys, Manifests: idx, URLCheck: func(string) error { return nil }})
	c := New(Deps{Local: func(string) bool { return true }, Runner: run, Secrets: keys})
	if !c.Serves("leadmagic_mobile") || !c.Serves("prospeo_mobile") {
		t.Fatal("manifest providers not served")
	}
	res, err := c.Call(context.Background(), call("leadmagic_mobile", "m1", `{"linkedin_url":"https://www.linkedin.com/in/x"}`))
	if err != nil || res.Status != "ok" {
		t.Fatalf("%+v %v", res, err)
	}
	out := string(res.Output)
	if !strings.Contains(out, `"provider":"leadmagic_mobile","success":true`) || !strings.Contains(out, `"mobile_phone":"+15550001"`) {
		t.Fatalf("output %s", out)
	}
	if strings.Contains(out, "lm-secret-key") {
		t.Fatalf("credential leaked: %s", out)
	}
	if res.CostUSD != 0.05 || calls.Load() != 1 {
		t.Fatalf("cost %v calls %d", res.CostUSD, calls.Load())
	}

	// An index without the manifest (e.g. an unsigned connector under policy "required") serves nothing.
	none := providerrun.New(providerrun.Deps{HTTP: client, Secrets: keys, Manifests: func() map[string]manifest.Entry { return nil }})
	c2 := New(Deps{Local: func(string) bool { return true }, Runner: none, Secrets: keys})
	if _, err := c2.Call(context.Background(), call("leadmagic_mobile", "m2", `{}`)); !errors.Is(err, provcall.ErrUnavailable) {
		t.Fatalf("unverified manifest: %v", err)
	}
}
