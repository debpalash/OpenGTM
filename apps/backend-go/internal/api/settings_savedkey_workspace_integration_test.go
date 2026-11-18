//go:build integration

package api

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providerrun"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	_ "github.com/debpalash/OpenGTM/apps/backend-go/internal/providers/contact"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

// Issue 114 closure: a key saved through the settings API reaches the next provider run on the production credential
// path (providerrun.Runner -> secrets.Resolver -> PgStore), per workspace, and is never written to the process
// environment. Two production save paths exist:
//   - PUT /api/settings/enrichment-providers/{id}: the operator (global admin) key, stored in app_settings. Every
//     workspace resolves it, so an update is seen by all of them on their next run.
//   - PUT /api/settings/workspace-integrations/{id}: a workspace's own secret (workspace_settings), seen by that
//     workspace only; it shadows a global value there and never leaks to another workspace.

type keyProbeVendor struct {
	srv  *httptest.Server
	mu   sync.Mutex
	keys []string
}

func newKeyProbeVendor(t *testing.T) *keyProbeVendor {
	t.Helper()
	v := &keyProbeVendor{}
	v.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		v.mu.Lock()
		v.keys = append(v.keys, r.URL.Query().Get("api_key"))
		v.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"emails":[]}}`)
	}))
	t.Cleanup(v.srv.Close)
	return v
}

func (v *keyProbeVendor) client() *http.Client {
	addr := v.srv.Listener.Addr().String()
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test server
	}}
}

func (v *keyProbeVendor) last() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.keys) == 0 {
		return "<no call>"
	}
	return v.keys[len(v.keys)-1]
}

func (v *keyProbeVendor) calls() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.keys)
}

// probeProv is a keyed provider that records the key its runner resolved.
type probeProv struct {
	name, env string
	mu        sync.Mutex
	got       []string
}

func (p *probeProv) Name() string     { return p.name }
func (p *probeProv) KeyEnv() []string { return []string{p.env} }
func (p *probeProv) Enrich(_ context.Context, _ *http.Client, key func() (string, error), _ providers.Input) (providers.Output, error) {
	k, err := key()
	if err != nil {
		return providers.Output{}, err
	}
	p.mu.Lock()
	p.got = append(p.got, k)
	p.mu.Unlock()
	return providers.Output{}, nil
}

func (p *probeProv) last() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.got) == 0 {
		return "<no call>"
	}
	return p.got[len(p.got)-1]
}

func TestSavedGlobalProviderKeyReachesEveryWorkspaceNextRun(t *testing.T) {
	const envKey = "HUNTER_API_KEY"
	t.Setenv(envKey, "") // a developer shell value must not mask a write-through
	e := newSettingsEnv(t)
	_, admin := e.seedAdmin(t, "t114-adm")
	wsA, wsB := e.wsInteg(t, "gkeyA"), e.wsInteg(t, "gkeyB")
	t.Cleanup(func() { _, _ = e.pool.Exec(context.Background(), `DELETE FROM app_settings WHERE key=$1`, envKey) })
	_, _ = e.pool.Exec(context.Background(), `DELETE FROM app_settings WHERE key=$1`, envKey)

	envl, err := secrets.NewEnvelope(secrets.Config{MasterKey: settingsTestMaster, AppEnv: "test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	vendor := newKeyProbeVendor(t)
	run := providerrun.New(providerrun.Deps{
		HTTP:    vendor.client(),
		Secrets: secrets.NewResolver(envl, secrets.NewPgStore(e.pool), os.Getenv),
	})
	call := func(ws string) string {
		t.Helper()
		before := vendor.calls()
		if _, err := run.RunLocal(context.Background(), ws, "hunter_io", providers.Input{Lead: map[string]any{"website": "acme-114.example.com"}}, 10*time.Second); err != nil {
			t.Fatalf("run in %s: %v", ws, err)
		}
		if vendor.calls() == before {
			return "<no vendor call>"
		}
		return vendor.last()
	}

	if got := call(wsA.ws); got != "<no vendor call>" {
		t.Fatalf("unconfigured provider reached the vendor with key %q", got)
	}
	save := func(key string) {
		t.Helper()
		rec, _ := e.put(t, admin, "/api/settings/enrichment-providers/hunter_io", `{"api_key":"`+key+`"}`)
		if rec.Code != 200 {
			t.Fatalf("save %s: %d %s", key, rec.Code, rec.Body.String())
		}
	}
	save("hunter-key-one-1111")
	for _, ws := range []string{wsA.ws, wsB.ws} {
		if got := call(ws); got != "hunter-key-one-1111" {
			t.Fatalf("%s after first save: vendor saw %q", ws, got)
		}
	}
	save("hunter-key-two-2222")
	for _, ws := range []string{wsA.ws, wsB.ws} {
		if got := call(ws); got != "hunter-key-two-2222" {
			t.Fatalf("%s after update: vendor saw %q (stale key)", ws, got)
		}
	}
	if v, ok := os.LookupEnv(envKey); !ok || v != "" {
		t.Fatalf("saving a key wrote through to the process environment: %s=%q (set=%v)", envKey, v, ok)
	}
	if v, _ := e.raw(t, envKey); strings.Contains(v, "hunter-key") {
		t.Fatalf("key stored in plaintext: %q", v)
	}
}

func TestSavedWorkspaceKeyIsolatedAndUpdatedPerWorkspace(t *testing.T) {
	const envKey = "HUBSPOT_TOKEN" // a workspace-integration field: the production per-workspace save path
	t.Setenv(envKey, "")
	e := newSettingsEnv(t)
	e.cleanIntegrations(t)
	wsA, wsB := e.wsInteg(t, "wkeyA"), e.wsInteg(t, "wkeyB")

	envl, err := secrets.NewEnvelope(secrets.Config{MasterKey: settingsTestMaster, AppEnv: "test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	prov := &probeProv{name: "t114_probe_ws", env: envKey}
	providers.RegisterForTest(t, prov)
	run := providerrun.New(providerrun.Deps{Secrets: secrets.NewResolver(envl, secrets.NewPgStore(e.pool), os.Getenv)})
	call := func(ws string) string {
		t.Helper()
		if _, err := run.RunLocal(context.Background(), ws, prov.name, providers.Input{Lead: map[string]any{"website": "acme-114.example.com"}}, 10*time.Second); err != nil {
			t.Fatalf("run in %s: %v", ws, err)
		}
		return prov.last()
	}
	save := func(ws wsInteg, token string) {
		t.Helper()
		rec, _ := e.put(t, ws.editor, "/api/settings/workspace-integrations/hubspot?workspace_id="+ws.ws, `{"values":{"`+envKey+`":"`+token+`"}}`)
		if rec.Code != 200 {
			t.Fatalf("save %s in %s: %d %s", token, ws.ws, rec.Code, rec.Body.String())
		}
	}

	if got := call(wsA.ws); got != "" {
		t.Fatalf("unsaved workspace resolved %q", got)
	}
	save(wsA, "ws-a-token-one-1111")
	if got := call(wsA.ws); got != "ws-a-token-one-1111" {
		t.Fatalf("A after save: %q", got)
	}
	if got := call(wsB.ws); got != "" {
		t.Fatalf("workspace B resolved workspace A's saved key: %q", got)
	}
	save(wsA, "ws-a-token-two-2222")
	if got := call(wsA.ws); got != "ws-a-token-two-2222" {
		t.Fatalf("A after update: %q (stale key)", got)
	}
	save(wsB, "ws-b-token-one-3333")
	if got, a := call(wsB.ws), call(wsA.ws); got != "ws-b-token-one-3333" || a != "ws-a-token-two-2222" {
		t.Fatalf("B=%q A=%q, each workspace must keep its own key", got, a)
	}
	// A blank form field never clobbers a saved secret (secrets.Resolver.Set), and nothing reached the environment.
	rec, _ := e.put(t, wsA.editor, "/api/settings/workspace-integrations/hubspot?workspace_id="+wsA.ws, `{"values":{"`+envKey+`":"  "}}`)
	if rec.Code != 200 {
		t.Fatalf("blank save: %d %s", rec.Code, rec.Body.String())
	}
	if got := call(wsA.ws); got != "ws-a-token-two-2222" {
		t.Fatalf("A after blank save: %q", got)
	}
	if v, ok := os.LookupEnv(envKey); !ok || v != "" {
		t.Fatalf("saving a workspace key wrote through to the process environment: %s=%q (set=%v)", envKey, v, ok)
	}
	if raw, ok := e.rawWS(t, wsA.ws, envKey); !ok || strings.Contains(raw, "ws-a-token") {
		t.Fatalf("workspace secret stored %q (found=%v): want ciphertext", raw, ok)
	}
}
