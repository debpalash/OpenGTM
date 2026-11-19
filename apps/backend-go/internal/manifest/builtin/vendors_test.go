package builtin_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/connectors"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest/builtin"
)

// Every manifest added after the first four has a directory under
// testdata/vendors/<manifest name>/ with a case.json and the vendor response
// bodies it names. This one test loads each manifest from the embedded
// directory, runs it against a local TLS server that answers with those
// bodies, and checks the request and the projected result.
//
// Adding a manifest: add the directory, nothing else. See
// apps/docs/src/content/docs/guides/provider-manifests.md.

const vendorsDir = "testdata/vendors"

// legacyManifests were shipped before the fixture rule and are covered by the
// Python parity vectors and the providerrun tests instead.
var legacyManifests = map[string]bool{
	"leadmagic_company": true, "leadmagic_email": true, "leadmagic_mobile": true, "prospeo_mobile": true,
}

type vendorRequest struct {
	Method  string            `json:"method"`
	Host    string            `json:"host"`
	Path    string            `json:"path"`
	Query   map[string]string `json:"query"`
	Headers map[string]string `json:"headers"`
	Body    json.RawMessage   `json:"body"`
}

type vendorCase struct {
	Docs       []string          `json:"docs"`
	Lead       map[string]string `json:"lead"`
	Credential string            `json:"credential"`
	// SecondaryCredential is the value served for auth.secondary.env_var (key plus secret vendors).
	SecondaryCredential string        `json:"secondary_credential"`
	Request             vendorRequest `json:"request"`
	// Poll is the request every poll step must send (asynchronous vendors).
	Poll      *vendorRequest   `json:"poll"`
	Responses []vendorResponse `json:"responses"`
}

type vendorStep struct {
	Status int    `json:"status"`
	File   string `json:"file"`
}

// vendorResponse is one scripted vendor answer. A synchronous vendor answers with Status and
// File. An asynchronous one lists Steps: step 0 answers the submit request and every later step
// answers one poll request, in order; the vendor must see exactly that many requests.
type vendorResponse struct {
	Name   string         `json:"name"`
	Status int            `json:"status"`
	File   string         `json:"file"`
	Steps  []vendorStep   `json:"steps"`
	Fields map[string]any `json:"fields"` // expected mapped fields on a hit
	Error  string         `json:"error"`  // expected Result.Error on a miss or failure
}

type seenRequest struct {
	method, host, path string
	query              map[string]string
	header             http.Header
	body               []byte
	rawURL             string
}

func builtinIndex(t *testing.T) map[string]manifest.Entry {
	t.Helper()
	dir, trust, err := builtin.Paths()
	if err != nil {
		t.Fatal(err)
	}
	idx, err := manifest.LoadIndex(dir, "optional", manifest.LoadTrustStore(trust))
	if err != nil {
		t.Fatal(err)
	}
	return idx
}

func caseDirs(t *testing.T) []string {
	t.Helper()
	ents, err := os.ReadDir(vendorsDir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names
}

func TestVendorManifestsHaveFixtures(t *testing.T) {
	idx := builtinIndex(t)
	have := map[string]bool{}
	for _, n := range caseDirs(t) {
		have[n] = true
		if _, ok := idx[n]; !ok {
			t.Errorf("fixture directory %s has no embedded manifest of that name", n)
		}
	}
	for name := range idx {
		if !legacyManifests[name] && !have[name] {
			t.Errorf("manifest %s has no fixture directory %s/%s", name, vendorsDir, name)
		}
	}
}

// Rules every shipped manifest must meet, beyond what the loader checks.
func TestBuiltinManifestPolicy(t *testing.T) {
	dir, trust, err := builtin.Paths()
	if err != nil {
		t.Fatal(err)
	}
	rep := connectors.ValidateDirectory(dir, "optional", manifest.LoadTrustStore(trust))
	if !rep.OK {
		t.Fatalf("connector validation failed: %+v", rep.Errors)
	}
	for name, e := range builtinIndex(t) {
		m := e.Manifest
		if !strings.HasPrefix(m.Request.URL, "https://") {
			t.Errorf("%s: request.url must be https", name)
		}
		if m.Homepage == "" {
			t.Errorf("%s: homepage must point at the vendor docs", name)
		}
		if m.Auth.Type != "none" && m.Auth.Value != "" && !strings.Contains(m.Auth.Value, "${env:"+m.Auth.EnvVar+"}") {
			t.Errorf("%s: auth.value may only wrap ${env:%s}; the key comes from auth.env_var", name, m.Auth.EnvVar)
		}
		if m.Poll != nil && !strings.HasPrefix(m.Poll.Request.URL, "https://") {
			t.Errorf("%s: poll.request.url must be https", name)
		}
	}
	// No secret-looking literal in any manifest file.
	err = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := os.ReadFile(p)
		for _, bad := range []string{"sk_live", "Bearer ey", "api_key: \""} {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s contains %q", p, bad)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestVendorManifestsAgainstDocsSamples(t *testing.T) {
	idx := builtinIndex(t)
	for _, name := range caseDirs(t) {
		t.Run(name, func(t *testing.T) {
			entry, ok := idx[name]
			if !ok {
				t.Fatalf("manifest %s is not in the embedded index", name)
			}
			runVendorCase(t, name, entry.Manifest)
		})
	}
}

func runVendorCase(t *testing.T, name string, m manifest.Manifest) {
	raw, err := os.ReadFile(filepath.Join(vendorsDir, name, "case.json"))
	if err != nil {
		t.Fatal(err)
	}
	var c vendorCase
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("case.json: %v", err)
	}
	if len(c.Docs) == 0 || c.Credential == "" || len(c.Responses) == 0 {
		t.Fatal("case.json needs docs, credential and responses")
	}
	envVar := m.Auth.EnvVar
	if envVar == "" {
		t.Fatal("manifest has no auth.env_var")
	}
	secEnv := ""
	if m.Auth.Secondary != nil {
		secEnv = m.Auth.Secondary.EnvVar
		if c.SecondaryCredential == "" {
			t.Fatal("manifest has auth.secondary but case.json has no secondary_credential")
		}
	}
	cred := func(k string) (string, error) {
		if k == envVar {
			return c.Credential, nil
		}
		if secEnv != "" && k == secEnv {
			return c.SecondaryCredential, nil
		}
		return "", nil
	}
	if m.Poll != nil {
		if c.Poll == nil {
			t.Fatal("manifest has poll but case.json has no poll request")
		}
		// The test server answers at once, so run the poll at the engine's shortest interval.
		ps := *m.Poll
		ps.Interval = manifest.MinPollInterval.Seconds()
		m.Poll = &ps
	}

	var mu sync.Mutex
	var seen []seenRequest
	var cur vendorResponse
	var curBody []byte
	var stepBodies [][]byte
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		q := map[string]string{}
		for k, v := range r.URL.Query() {
			q[k] = v[0]
		}
		mu.Lock()
		seen = append(seen, seenRequest{r.Method, r.Host, r.URL.Path, q, r.Header.Clone(), b, r.URL.String()})
		status, body := cur.Status, curBody
		if len(cur.Steps) > 0 {
			i := len(seen) - 1
			if i >= len(cur.Steps) {
				i = len(cur.Steps) - 1
			}
			status, body = cur.Steps[i].Status, stepBodies[i]
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	addr := srv.Listener.Addr().String()
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // local test server
	}}

	for _, resp := range c.Responses {
		t.Run(resp.Name, func(t *testing.T) {
			var body []byte
			var bodies [][]byte
			if len(resp.Steps) > 0 {
				for _, st := range resp.Steps {
					b, err := os.ReadFile(filepath.Join(vendorsDir, name, st.File))
					if err != nil {
						t.Fatal(err)
					}
					bodies = append(bodies, b)
				}
			} else {
				b, err := os.ReadFile(filepath.Join(vendorsDir, name, resp.File))
				if err != nil {
					t.Fatal(err)
				}
				body = b
			}
			mu.Lock()
			seen, cur, curBody, stepBodies = nil, resp, body, bodies
			mu.Unlock()

			// nil check func: the default egress guard (https/host rules) applies.
			res := manifest.ExecuteChecked(context.Background(), client, m, c.Lead, cred, nil)

			mu.Lock()
			got := append([]seenRequest(nil), seen...)
			mu.Unlock()
			wantReqs := 1
			if len(resp.Steps) > 0 {
				wantReqs = len(resp.Steps)
			}
			if len(got) != wantReqs || res.Requests != wantReqs {
				t.Fatalf("vendor saw %d requests (engine counted %d), want %d (result %+v)", len(got), res.Requests, wantReqs, res)
			}
			for i, r := range got {
				want := c.Request
				if i > 0 {
					want = *c.Poll
				}
				assertRequest(t, c, want, r)
			}

			for _, secret := range []string{c.Credential, c.SecondaryCredential} {
				if secret != "" && strings.Contains(res.Error, secret) {
					t.Errorf("error text leaks a credential: %q", res.Error)
				}
			}
			if resp.Error != "" {
				if res.Success || res.Error != resp.Error {
					t.Fatalf("want failure %q, got success=%v error=%q fields=%v", resp.Error, res.Success, res.Error, res.Fields)
				}
				if len(res.Fields) != 0 && resp.Error != "no_data" {
					t.Errorf("a failed call returned fields %v", res.Fields)
				}
				return
			}
			if !res.Success || res.Error != "" {
				t.Fatalf("want hit, got success=%v error=%q", res.Success, res.Error)
			}
			if res.Confidence != m.DefaultConfidence {
				t.Errorf("confidence %v, manifest says %v", res.Confidence, m.DefaultConfidence)
			}
			want := resp.Fields
			if !reflect.DeepEqual(res.Fields, want) {
				t.Errorf("fields %v, want %v", res.Fields, want)
			}
			for k := range want {
				if _, mapped := m.Response.Mappings[k]; !mapped {
					t.Errorf("expected field %q is not in response.mappings", k)
				}
			}
		})
	}

	if secEnv != "" {
		t.Run("missing_secondary_makes_no_call", func(t *testing.T) {
			mu.Lock()
			seen = nil
			mu.Unlock()
			res := manifest.ExecuteChecked(context.Background(), client, m, c.Lead,
				func(k string) (string, error) {
					if k == envVar {
						return c.Credential, nil
					}
					return "", nil
				}, nil)
			if res.Success || res.Error != name+": missing "+secEnv {
				t.Fatalf("result %+v", res)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(seen) != 0 {
				t.Fatalf("a request went out without the second credential")
			}
		})
	}

	t.Run("missing_key_makes_no_call", func(t *testing.T) {
		mu.Lock()
		seen = nil
		mu.Unlock()
		res := manifest.ExecuteChecked(context.Background(), client, m, c.Lead,
			func(string) (string, error) { return "", nil }, nil)
		if res.Success || res.Error != name+": missing "+envVar {
			t.Fatalf("result %+v", res)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(seen) != 0 {
			t.Fatalf("a request went out without a key")
		}
	})
}

func assertRequest(t *testing.T, c vendorCase, want vendorRequest, r seenRequest) {
	t.Helper()
	if r.method != want.Method || r.path != want.Path {
		t.Errorf("request %s %s, want %s %s", r.method, r.path, want.Method, want.Path)
	}
	if r.host != want.Host {
		t.Errorf("host %q, want %q", r.host, want.Host)
	}
	if !reflect.DeepEqual(r.query, orEmpty(want.Query)) {
		t.Errorf("query %v, want %v", r.query, want.Query)
	}
	for k, v := range want.Headers {
		if got := r.header.Get(k); got != v {
			t.Errorf("header %s = %q, want %q", k, got, v)
		}
	}
	// Each credential appears in the one place the case declares and nowhere else; neither is
	// ever in the URL path or the body.
	for _, secret := range []string{c.Credential, c.SecondaryCredential} {
		if secret == "" {
			continue
		}
		inHeader := false
		for _, vs := range r.header {
			for _, v := range vs {
				if strings.Contains(v, secret) {
					inHeader = true
				}
			}
		}
		inQuery := strings.Contains(r.rawURL, secret)
		inBody := strings.Contains(string(r.body), secret)
		wantHeader, wantQuery := false, false
		for _, v := range want.Headers {
			if strings.Contains(v, secret) {
				wantHeader = true
			}
		}
		for _, v := range want.Query {
			if strings.Contains(v, secret) {
				wantQuery = true
			}
		}
		if inHeader != wantHeader || inQuery != wantQuery || inBody || strings.Contains(r.path, secret) {
			t.Errorf("credential placement: header=%v query=%v body=%v, want header=%v query=%v body=false and not in the path",
				inHeader, inQuery, inBody, wantHeader, wantQuery)
		}
	}
	if len(want.Body) == 0 || string(want.Body) == "null" {
		if len(r.body) != 0 {
			t.Errorf("unexpected request body %s", r.body)
		}
		return
	}
	var wantBody, gotBody any
	if err := json.Unmarshal(want.Body, &wantBody); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(r.body, &gotBody); err != nil {
		t.Fatalf("request body is not JSON: %s", r.body)
	}
	if !reflect.DeepEqual(gotBody, wantBody) {
		t.Errorf("request body %s, want %s", r.body, want.Body)
	}
}

func orEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// Downstream code keys on these words for email_verify (emailverify.StatusFromVendor, the
// native verifiers), so a manifest may not write the vendor's own vocabulary there.
func TestEmailVerifyManifestsEmitNativeWords(t *testing.T) {
	native := map[string]bool{"valid": true, "invalid": true, "catch_all": true, "risky": true, "unknown": true}
	n := 0
	for name, e := range builtinIndex(t) {
		if _, ok := e.Manifest.Response.Mappings["email_verify"]; !ok {
			continue
		}
		n++
		vm, ok := e.Manifest.Response.ValueMaps["email_verify"]
		if !ok || vm.Default == nil || *vm.Default != "unknown" {
			t.Errorf("%s: response.value_maps.email_verify needs default: unknown", name)
			continue
		}
		for from, to := range vm.Values {
			if !native[to] {
				t.Errorf("%s: %q maps to %q, which is not a native email_verify word", name, from, to)
			}
		}
	}
	if n < 3 {
		t.Errorf("found %d email_verify manifests, expected at least 3", n)
	}
}

func TestValueMapsMustNameMappedFields(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "email"), 0o700); err != nil {
		t.Fatal(err)
	}
	yaml := `manifest_version: "1"
name: vm_bad
capability: email
auth: {type: none}
request: {method: GET, url: "https://api.example.com/x", timeout: 5}
response:
  mappings: {email: "$.e"}
  value_maps:
    email_verify: {values: {a: b}}
`
	if err := os.WriteFile(filepath.Join(dir, "email", "vm_bad.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	rep := connectors.ValidateDirectory(dir, "optional", manifest.TrustStore{})
	if rep.OK || len(rep.Errors) != 1 || !strings.Contains(rep.Errors[0].Error, "value_maps") {
		t.Fatalf("%+v", rep)
	}
}

// docs/connectors/connector.template.yaml is what contributors copy; it must stay valid.
func TestConnectorTemplateIsValid(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "..", "docs", "connectors", "connector.template.yaml"))
	if err != nil {
		t.Skipf("template not present: %v", err)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "email"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "email", "acme_email.yaml"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	rep := connectors.ValidateDirectory(dir, "optional", manifest.TrustStore{})
	if !rep.OK || rep.Count != 1 {
		t.Fatalf("template does not validate: %+v", rep)
	}
}
