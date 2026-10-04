package declarative

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/egress"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/manifest"
)

type recordingSecrets struct {
	mu     sync.Mutex
	values map[string]string
	asked  []string
}

func (r *recordingSecrets) Secret(_ context.Context, name string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked = append(r.asked, name)
	return r.values[name], nil
}

func mustLoad(t *testing.T, text string) *manifest.Plugin {
	t.Helper()
	p, err := manifest.LoadBytes("test.yaml", []byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func jsonClient(t *testing.T, body string, seen *[]string) *egress.Client {
	t.Helper()
	c, err := egress.New(egress.Options{DefaultRPS: 1000, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if seen != nil {
			*seen = append(*seen, r.URL.String()+" "+r.Header.Get("Authorization"))
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const queryAuthV2 = `manifest_version: "2"
name: query_auth_v2
kind: provider
runtime: declarative
version: 0.1.0
capabilities: {network: ["https://api.vendor.example"], secrets: [VENDOR_KEY]}
inputs: {domain: string}
capability: email
cost_per_lookup: 0.25
auth: {type: query, env_var: VENDOR_KEY}
request: {method: GET, url: "https://api.vendor.example/find?d={{input.domain}}"}
response: {mappings: {email: $.email}}
`

func TestEvidenceRedactsSecretsAndRecordsMappings(t *testing.T) {
	p := mustLoad(t, queryAuthV2)
	secrets := &recordingSecrets{values: map[string]string{"VENDOR_KEY": "sk_live_SECRET", "OTHER": "nope"}}
	res, err := RunProvider(context.Background(), p, map[string]any{"domain": "acme.example"}, secrets, jsonClient(t, `{"email":"a@acme.example"}`, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Success || res.CostUSD != 0.25 || res.Evidence == nil {
		t.Fatalf("result %+v", res)
	}
	if strings.Contains(res.Evidence.SourceURL, "sk_live") || !strings.Contains(res.Evidence.SourceURL, "api_key=REDACTED") {
		t.Fatalf("evidence url not redacted: %s", res.Evidence.SourceURL)
	}
	if res.Evidence.Mappings["email"] != "$.email" || res.Evidence.SHA256 == "" || res.Evidence.Status != 200 {
		t.Fatalf("evidence %+v", res.Evidence)
	}
	if len(secrets.asked) != 1 || secrets.asked[0] != "VENDOR_KEY" {
		t.Fatalf("only declared secrets may be resolved, asked %v", secrets.asked)
	}
	// No data: no cost.
	res, _ = RunProvider(context.Background(), p, map[string]any{"domain": "acme.example"}, secrets, jsonClient(t, `{}`, nil))
	if res.Success || res.CostUSD != 0 || res.Error != "no_data" {
		t.Fatalf("miss %+v", res)
	}
}

func TestUndeclaredEnvReferenceRendersEmptyForV1(t *testing.T) {
	p := mustLoad(t, `manifest_version: "1"
name: v1_other_env
capability: email
auth: {type: bearer, env_var: KEY}
request:
  url: https://api.vendor.example/find
  headers: {X-Other: "${env:OTHER_SECRET}"}
response: {mappings: {email: $.email}}
`)
	var seen []string
	secrets := &recordingSecrets{values: map[string]string{"KEY": "k1", "OTHER_SECRET": "leak"}}
	client, _ := egress.New(egress.Options{DefaultRPS: 1000, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen = append(seen, r.Header.Get("X-Other"))
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"email":"x@y.z"}`))}, nil
	})})
	if _, err := RunProvider(context.Background(), p, nil, secrets, client); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != "" {
		t.Fatalf("undeclared secret rendered: %v", seen)
	}
	for _, n := range secrets.asked {
		if n == "OTHER_SECRET" {
			t.Fatal("undeclared secret was resolved")
		}
	}
}

func TestErrorsNeverContainSecrets(t *testing.T) {
	p := mustLoad(t, queryAuthV2)
	client, _ := egress.New(egress.Options{DefaultRPS: 1000, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("dial failed for %s", r.URL)
	})})
	res, err := RunProvider(context.Background(), p, map[string]any{"domain": "acme.example"}, SecretMap{"VENDOR_KEY": "sk_live_SECRET"}, client)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Error, "sk_live") || !strings.Contains(res.Error, "REDACTED") {
		t.Fatalf("error leaks secret: %q", res.Error)
	}
}

func TestCapabilityAndSSRFBlockTemplatedHosts(t *testing.T) {
	v2 := `manifest_version: "2"
name: templated_host
kind: provider
runtime: declarative
version: 0.1.0
capabilities: {network: ["https://*.acme.example"]}
inputs: {domain: string}
capability: email
request: {method: GET, url: "https://{{input.domain}}/api"}
response: {mappings: {email: $.email}}
`
	var seen []string
	p := mustLoad(t, v2)
	res, _ := RunProvider(context.Background(), p, map[string]any{"domain": "evil.example"}, nil, jsonClient(t, `{}`, &seen))
	if !strings.HasPrefix(res.Error, "blocked_url:") || len(seen) != 0 {
		t.Fatalf("undeclared host must be blocked before any request: %q %v", res.Error, seen)
	}
	res, _ = RunProvider(context.Background(), p, map[string]any{"domain": "api.acme.example"}, nil, jsonClient(t, `{"email":"ok@acme.example"}`, &seen))
	if !res.Success {
		t.Fatalf("declared host: %+v", res)
	}
	// v1 templated hosts get https://*, but the SSRF guard still applies.
	v1 := mustLoad(t, "manifest_version: \"1\"\nname: v1_templated\ncapability: email\nrequest: {url: 'https://{{input.domain}}/find'}\nresponse: {mappings: {email: $.email}}\n")
	real, _ := egress.New(egress.Options{})
	for _, host := range []string{"127.0.0.1", "169.254.169.254", "2130706433", "localhost"} {
		res, _ = RunProvider(context.Background(), v1, map[string]any{"domain": host}, nil, real)
		if !strings.HasPrefix(res.Error, "blocked_url:") {
			t.Fatalf("%s: expected blocked_url, got %q", host, res.Error)
		}
	}
}

func TestProviderTimeoutAndInputsSchema(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	p := mustLoad(t, fmt.Sprintf("manifest_version: \"1\"\nname: slow_vendor\ncapability: email\nrequest: {url: '%s/find', timeout: 0.2}\nresponse: {mappings: {email: $.email}}\n", srv.URL))
	client, _ := egress.New(egress.Options{AllowPrivateForTesting: true, DefaultRPS: 1000, TLSConfig: srv.Client().Transport.(*http.Transport).TLSClientConfig})
	start := time.Now()
	res, err := RunProvider(context.Background(), p, nil, nil, client)
	if err != nil || res.Error != "timeout" || time.Since(start) > time.Second {
		t.Fatalf("timeout: %+v %v after %s", res, err, time.Since(start))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := RunProvider(ctx, p, nil, nil, client); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled context: %v", err)
	}
	v2 := mustLoad(t, queryAuthV2)
	if _, err := RunProvider(context.Background(), v2, map[string]any{"domain": 5}, nil, client); err == nil {
		t.Fatal("inputs schema must be enforced")
	}
}

func TestNormalizePhone(t *testing.T) {
	cases := map[string]string{
		"+1 (415) 555-0132": "+14155550132", "0044 20 7946 0958": "+442079460958", "415.555.0132": "4155550132",
		"": "", "  ": "  ", "n/a": "n/a", "+1234567890123456": "+123456789012345", "+123456789012345678": "+123456789012345678", "12345678901234567890": "12345678901234567890",
		"0012345678": "+12345678", "00123456": "00123456", "٤١٥ ٥٥٥": "٤١٥٥٥٥",
	}
	for in, want := range cases {
		if got := NormalizePhone(in); got != want {
			t.Errorf("%q: got %v want %q", in, got, want)
		}
	}
	if NormalizePhone(5) != 5 {
		t.Error("non-strings pass through")
	}
}
