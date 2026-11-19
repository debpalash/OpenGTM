package providerrun_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/credentials"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest/builtin"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providerrun"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// vendorSim is a TLS server every vendor host is dialled to; requests are recorded by host.
type vendorSim struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []vendorReq
}

type vendorReq struct {
	host, path string
	header     http.Header
	body       map[string]any
}

func newVendorSim(t *testing.T, h func(host string, body map[string]any) string) *vendorSim {
	t.Helper()
	v := &vendorSim{}
	v.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		v.mu.Lock()
		v.reqs = append(v.reqs, vendorReq{host: r.Host, path: r.URL.Path, header: r.Header.Clone(), body: body})
		v.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, h(r.Host, body))
	}))
	t.Cleanup(v.srv.Close)
	return v
}

// client dials every host to the sim (the manifests keep their real vendor URLs).
func (v *vendorSim) client() *http.Client {
	addr := v.srv.Listener.Addr().String()
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test server
	}}
}

func (v *vendorSim) calls() []vendorReq {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]vendorReq(nil), v.reqs...)
}

// wsSecrets serves credentials per workspace only (no global fallback).
type wsSecrets map[string]map[string]string

func (s wsSecrets) Get(_ context.Context, ws, key string) (string, string, error) {
	return s[ws][key], "workspace", nil
}

func builtinIndex(t *testing.T) func() map[string]manifest.Entry {
	t.Helper()
	idx := credentials.ConnectorIndex(func(string) (string, bool) { return "", false }, nil)
	if _, ok := idx()["leadmagic_mobile"]; !ok {
		t.Fatal("built-in index lacks leadmagic_mobile")
	}
	return idx
}

func TestManifestProviderRunsFromConnectorIndex(t *testing.T) {
	sim := newVendorSim(t, func(string, map[string]any) string { return `{"message":"ok","mobile_number":"+15550001"}` })
	r := providerrun.New(providerrun.Deps{
		HTTP: sim.client(), Secrets: wsSecrets{"ws1": {"LEADMAGIC_API_KEY": "lm-key-ws1"}},
		Manifests: builtinIndex(t), URLCheck: func(string) error { return nil },
	})
	if !r.Serves("leadmagic_mobile") || !r.Serves("prospeo_mobile") || !r.Serves("leadmagic_email") || !r.Serves("leadmagic_company") {
		t.Fatal("built-in manifest providers are not served")
	}
	in := providers.Input{Lead: map[string]any{"linkedin_url": "https://www.linkedin.com/in/jdoe", "company": "Acme"}, Field: "mobile_phone"}
	out, err := r.Run(context.Background(), "ws1", "leadmagic_mobile", in, 0)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Error != "" || out.Value != "+15550001" || out.Extra == nil {
		t.Fatalf("output %+v", out)
	}
	if v, _ := out.Extra.Get("phone"); v != "+15550001" {
		t.Fatalf("phone mapping %v", v)
	}
	if out.Confidence != 0.8 {
		t.Fatalf("confidence %v", out.Confidence)
	}
	calls := sim.calls()
	if len(calls) != 1 || calls[0].host != "api.leadmagic.io" || calls[0].path != "/mobile-finder" {
		t.Fatalf("calls %+v", calls)
	}
	if calls[0].header.Get("X-API-Key") != "lm-key-ws1" || calls[0].body["profile_url"] != "https://www.linkedin.com/in/jdoe" {
		t.Fatalf("request %+v", calls[0])
	}

	// Another workspace never sees ws1's key: refused before any network I/O.
	out, err = r.Run(context.Background(), "ws2", "leadmagic_mobile", in, 0)
	if err != nil || out.Error != "leadmagic_mobile: missing LEADMAGIC_API_KEY" {
		t.Fatalf("ws2 output %+v %v", out, err)
	}
	if len(sim.calls()) != 1 {
		t.Fatal("a call without the workspace's key reached the vendor")
	}
	if p, err := r.Precheck(context.Background(), "ws2", "leadmagic_mobile", in.Lead); err != nil || p != "leadmagic_mobile: missing LEADMAGIC_API_KEY" {
		t.Fatalf("precheck ws2 %q %v", p, err)
	}
	if p, err := r.Precheck(context.Background(), "ws1", "leadmagic_mobile", in.Lead); err != nil || p != "" {
		t.Fatalf("precheck ws1 %q %v", p, err)
	}
	if ok, err := r.Configured(context.Background(), "ws2", "leadmagic_mobile"); err != nil || ok {
		t.Fatalf("configured ws2 %v %v", ok, err)
	}
	if envs := r.ManifestEnv("leadmagic_mobile"); len(envs) != 1 || envs[0] != "LEADMAGIC_API_KEY" {
		t.Fatalf("manifest env %v", envs)
	}
}

func TestManifestProviderVendorErrorAndCredentialFailure(t *testing.T) {
	sim := newVendorSim(t, func(string, map[string]any) string { return `{"error":true,"message":"NO_RESULTS"}` })
	r := providerrun.New(providerrun.Deps{
		HTTP: sim.client(), Secrets: wsSecrets{"ws1": {"PROSPEO_API_KEY": "pk"}},
		Manifests: builtinIndex(t), URLCheck: func(string) error { return nil },
	})
	in := providers.Input{Lead: map[string]any{"linkedin_url": "https://www.linkedin.com/in/jdoe"}, Field: "mobile_phone"}
	out, err := r.Run(context.Background(), "ws1", "prospeo_mobile", in, 0)
	if err != nil || out.Error != "NO_RESULTS" {
		t.Fatalf("output %+v %v", out, err)
	}
	// A credential lookup failure fails closed (infrastructure), before any network I/O.
	bad := providerrun.New(providerrun.Deps{HTTP: sim.client(), Secrets: failingSecrets{}, Manifests: builtinIndex(t),
		URLCheck: func(string) error { return nil }})
	if _, err := bad.Run(context.Background(), "ws1", "prospeo_mobile", in, 0); err == nil {
		t.Fatal("credential lookup failure was not an error")
	}
	if p, err := bad.Precheck(context.Background(), "ws1", "prospeo_mobile", in.Lead); err == nil {
		t.Fatalf("precheck with a failing lookup %q", p)
	}
	if n := len(sim.calls()); n != 1 {
		t.Fatalf("vendor calls %d", n)
	}
}

type failingSecrets struct{}

func (failingSecrets) Get(context.Context, string, string) (string, string, error) {
	return "", "", errors.New("store down")
}

func TestUnknownProviderStaysUnavailable(t *testing.T) {
	r := providerrun.New(providerrun.Deps{Secrets: noSecrets{}, Manifests: builtinIndex(t)})
	if r.Serves("no_such_vendor") {
		t.Fatal("unknown provider served")
	}
	in := providers.Input{Lead: map[string]any{"company": "x"}, Field: "email"}
	if _, err := r.Run(context.Background(), "ws1", "no_such_vendor", in, 0); !errors.Is(err, providerrun.ErrUnavailable) {
		t.Fatalf("err %v", err)
	}
}

// userConnector writes an unsigned user connector into a fresh connector directory.
func userConnector(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	yaml := `manifest_version: "1"
name: user_vendor
capability: email
default_confidence: 0.7
auth:
  type: none
request:
  method: POST
  url: https://user.vendor.example/find
  body:
    company: "{{input.company}}"
response:
  mappings:
    email: "$.email"
`
	if err := os.WriteFile(filepath.Join(dir, "user_vendor.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestUnsignedUserConnectorFollowsSignaturePolicy(t *testing.T) {
	_, trust, err := builtin.Paths()
	if err != nil {
		t.Fatal(err)
	}
	dir := userConnector(t)
	sim := newVendorSim(t, func(string, map[string]any) string { return `{"email":"a@user.vendor.example"}` })
	in := providers.Input{Lead: map[string]any{"company": "Acme"}, Field: "email"}
	for _, tc := range []struct {
		policy string
		served bool
	}{{"required", false}, {"optional", true}} {
		env := map[string]string{credentials.ConnectorManifestsEnv: dir, credentials.ConnectorTrustStoreEnv: trust, credentials.ConnectorPolicyEnv: tc.policy}
		idx := credentials.ConnectorIndex(func(k string) (string, bool) { v, ok := env[k]; return v, ok }, nil)
		r := providerrun.New(providerrun.Deps{HTTP: sim.client(), Secrets: noSecrets{}, Manifests: idx, URLCheck: func(string) error { return nil }})
		before := len(sim.calls())
		out, err := r.Run(context.Background(), "ws1", "user_vendor", in, 0)
		if !tc.served {
			if !errors.Is(err, providerrun.ErrUnavailable) || r.Serves("user_vendor") || len(sim.calls()) != before {
				t.Fatalf("policy %s: unsigned connector ran: %+v %v", tc.policy, out, err)
			}
			continue
		}
		if err != nil || out.Value != "a@user.vendor.example" || len(sim.calls()) != before+1 {
			t.Fatalf("policy %s: %+v %v", tc.policy, out, err)
		}
	}
}

func TestManifestURLGuardRefusesBeforeNetwork(t *testing.T) {
	sim := newVendorSim(t, func(string, map[string]any) string { return `{}` })
	r := providerrun.New(providerrun.Deps{HTTP: sim.client(), Secrets: wsSecrets{"ws1": {"LEADMAGIC_API_KEY": "k"}},
		Manifests: builtinIndex(t), URLCheck: func(string) error { return errors.New("host is not allowed") }})
	in := providers.Input{Lead: map[string]any{"linkedin_url": "https://www.linkedin.com/in/x"}, Field: "mobile_phone"}
	out, err := r.Run(context.Background(), "ws1", "leadmagic_mobile", in, 0)
	if err != nil || out.Error == "" || len(sim.calls()) != 0 {
		t.Fatalf("%+v %v calls=%d", out, err, len(sim.calls()))
	}
	if p, _ := r.Precheck(context.Background(), "ws1", "leadmagic_mobile", in.Lead); p == "" {
		t.Fatal("precheck passed a blocked URL")
	}
}

// pollConnector writes an unsigned connector that submits a lookup and polls for it.
func pollConnector(t *testing.T, maxTotal int) func() map[string]manifest.Entry {
	t.Helper()
	_, trust, err := builtin.Paths()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	yaml := `manifest_version: "1"
name: slow_vendor
capability: email
default_confidence: 0.7
auth:
  type: header
  param: X-Key
  env_var: SLOWVENDOR_API_KEY
request:
  method: POST
  url: https://slow.vendor.example/submit
  body:
    company: "{{input.company}}"
poll:
  request:
    method: GET
    url: https://slow.vendor.example/status/{{response.id}}
  interval: 1
  max_attempts: 60
  max_total: ` + strconv.Itoa(maxTotal) + `
  done_when: {path: $.status, equals: done}
response:
  mappings:
    email: "$.email"
`
	if err := os.WriteFile(filepath.Join(dir, "slow_vendor.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{credentials.ConnectorManifestsEnv: dir, credentials.ConnectorTrustStoreEnv: trust, credentials.ConnectorPolicyEnv: "optional"}
	return credentials.ConnectorIndex(func(k string) (string, bool) { v, ok := env[k]; return v, ok }, nil)
}

// slowVendor answers the submit request with an id and every poll with "running" until `after`
// has passed since the submit, then "done".
func slowVendor(t *testing.T, after time.Duration) *vendorSim {
	t.Helper()
	var start time.Time
	var mu sync.Mutex
	return newVendorSim(t, func(_ string, body map[string]any) string {
		mu.Lock()
		defer mu.Unlock()
		if body != nil { // the submit request carries a JSON body; polls are GETs
			start = time.Now()
			return `{"id":"job1"}`
		}
		if time.Since(start) < after {
			return `{"status":"running"}`
		}
		return `{"status":"done","email":"a@slow.vendor.example"}`
	})
}

// A polling manifest is bounded by its own poll.max_total, not by the workspace provider timeout:
// with the default 10 s provider timeout (timeout argument 0) a vendor that needs 12 s succeeds.
func TestPollingManifestSucceedsUnderDefaultProviderTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("takes about 13 s of real time")
	}
	sim := slowVendor(t, 12*time.Second)
	r := providerrun.New(providerrun.Deps{HTTP: sim.client(), Secrets: wsSecrets{"ws1": {"SLOWVENDOR_API_KEY": "k"}},
		Manifests: pollConnector(t, 30), URLCheck: func(string) error { return nil }})
	in := providers.Input{Lead: map[string]any{"company": "Acme"}, Field: "email"}
	start := time.Now()
	out, err := r.Run(context.Background(), "ws1", "slow_vendor", in, 0)
	if err != nil || out.Error != "" || out.Value != "a@slow.vendor.example" {
		t.Fatalf("output %+v %v", out, err)
	}
	if el := time.Since(start); el < 12*time.Second {
		t.Fatalf("finished after %v, before the vendor was done", el)
	}
}

// A poll that legitimately uses up max_total is one "timeout"; a cancelled context ends the poll
// as an infrastructure error, never as a timeout.
func TestPollingManifestTimeoutAndCancellation(t *testing.T) {
	sim := slowVendor(t, time.Hour)
	r := providerrun.New(providerrun.Deps{HTTP: sim.client(), Secrets: wsSecrets{"ws1": {"SLOWVENDOR_API_KEY": "k"}},
		Manifests: pollConnector(t, 2), URLCheck: func(string) error { return nil }})
	in := providers.Input{Lead: map[string]any{"company": "Acme"}, Field: "email"}
	out, err := r.Run(context.Background(), "ws1", "slow_vendor", in, 0)
	if err != nil || out.Error != "timeout" {
		t.Fatalf("exhausted poll: %+v %v", out, err)
	}
	if n := len(sim.calls()); n < 2 || n > 4 {
		t.Fatalf("a 2 s poll made %d requests", n)
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	if out, err := r.Run(ctx, "ws1", "slow_vendor", in, 0); err == nil {
		t.Fatalf("cancelled poll returned %+v, want an error", out)
	}
}
