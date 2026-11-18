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

type vendorCase struct {
	Docs       []string          `json:"docs"`
	Lead       map[string]string `json:"lead"`
	Credential string            `json:"credential"`
	Request    struct {
		Method  string            `json:"method"`
		Host    string            `json:"host"`
		Path    string            `json:"path"`
		Query   map[string]string `json:"query"`
		Headers map[string]string `json:"headers"`
		Body    json.RawMessage   `json:"body"`
	} `json:"request"`
	Responses []vendorResponse `json:"responses"`
}

type vendorResponse struct {
	Name   string         `json:"name"`
	Status int            `json:"status"`
	File   string         `json:"file"`
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
		if m.Auth.Type != "none" && m.Auth.Value != "" {
			t.Errorf("%s: auth.value must not be set; the key comes from auth.env_var", name)
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
	cred := func(k string) (string, error) {
		if k == envVar {
			return c.Credential, nil
		}
		return "", nil
	}

	var mu sync.Mutex
	var seen []seenRequest
	var cur vendorResponse
	var curBody []byte
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		q := map[string]string{}
		for k, v := range r.URL.Query() {
			q[k] = v[0]
		}
		mu.Lock()
		seen = append(seen, seenRequest{r.Method, r.Host, r.URL.Path, q, r.Header.Clone(), b, r.URL.String()})
		status, body := cur.Status, curBody
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
			body, err := os.ReadFile(filepath.Join(vendorsDir, name, resp.File))
			if err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			seen, cur, curBody = nil, resp, body
			mu.Unlock()

			// nil check func: the default egress guard (https/host rules) applies.
			res := manifest.ExecuteChecked(context.Background(), client, m, c.Lead, cred, nil)

			mu.Lock()
			got := append([]seenRequest(nil), seen...)
			mu.Unlock()
			if len(got) != 1 {
				t.Fatalf("vendor saw %d requests, want 1 (result %+v)", len(got), res)
			}
			assertRequest(t, c, got[0])

			if strings.Contains(res.Error, c.Credential) {
				t.Errorf("error text leaks the credential: %q", res.Error)
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

func assertRequest(t *testing.T, c vendorCase, r seenRequest) {
	t.Helper()
	if r.method != c.Request.Method || r.path != c.Request.Path {
		t.Errorf("request %s %s, want %s %s", r.method, r.path, c.Request.Method, c.Request.Path)
	}
	if r.host != c.Request.Host {
		t.Errorf("host %q, want %q", r.host, c.Request.Host)
	}
	if !reflect.DeepEqual(r.query, orEmpty(c.Request.Query)) {
		t.Errorf("query %v, want %v", r.query, c.Request.Query)
	}
	for k, v := range c.Request.Headers {
		if got := r.header.Get(k); got != v {
			t.Errorf("header %s = %q, want %q", k, got, v)
		}
	}
	// The credential appears in the one place the manifest declares and nowhere else.
	inHeader := false
	for _, vs := range r.header {
		for _, v := range vs {
			if strings.Contains(v, c.Credential) {
				inHeader = true
			}
		}
	}
	inQuery := strings.Contains(r.rawURL, c.Credential)
	inBody := strings.Contains(string(r.body), c.Credential)
	wantHeader, wantQuery := false, false
	for _, v := range c.Request.Headers {
		if strings.Contains(v, c.Credential) {
			wantHeader = true
		}
	}
	for _, v := range c.Request.Query {
		if strings.Contains(v, c.Credential) {
			wantQuery = true
		}
	}
	if inHeader != wantHeader || inQuery != wantQuery || inBody {
		t.Errorf("credential placement: header=%v query=%v body=%v, want header=%v query=%v body=false",
			inHeader, inQuery, inBody, wantHeader, wantQuery)
	}
	if len(c.Request.Body) == 0 || string(c.Request.Body) == "null" {
		if len(r.body) != 0 {
			t.Errorf("unexpected request body %s", r.body)
		}
		return
	}
	var want, got any
	if err := json.Unmarshal(c.Request.Body, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(r.body, &got); err != nil {
		t.Fatalf("request body is not JSON: %s", r.body)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("request body %s, want %s", r.body, c.Request.Body)
	}
}

func orEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
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
