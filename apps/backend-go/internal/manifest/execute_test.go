package manifest_test

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
)

// clientTo returns a client that sends every connection to srv, so manifests
// can use a public-looking hostname that passes egress.CheckURL.
func clientTo(srv *httptest.Server) *http.Client {
	addr := srv.Listener.Addr().String()
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}}
}

func mk(t *testing.T, js string) manifest.Manifest {
	t.Helper()
	var m manifest.Manifest
	if err := json.Unmarshal([]byte(js), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func creds(m map[string]string) func(string) (string, error) {
	return func(k string) (string, error) {
		if v, ok := m[k]; ok {
			return v, nil
		}
		return "", errors.New("unset")
	}
}

const base = `{"name":"vend","capability":"email","default_confidence":0.8,
 "auth":{"type":"bearer","env_var":"VEND_KEY","value":"${env:VEND_KEY}"},
 "request":{"method":"post","url":"http://api.vendor.example/v1/find?x=1",
   "query":{"domain":"{{input.domain}}"},
   "body_template":{"name":"{{input.full_name}}","n":3,"f":1.0},
   "timeout":2},
 "response":{"error_path":"error","error_message_path":"message",
   "mappings":{"email":"$.data.email","phone":"$.data.phone","src":"vendor"}}}`

func TestExecuteSuccess(t *testing.T) {
	var gotPath, gotAuth, gotBody, gotCT, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotCT, gotMethod = r.URL.RawQuery, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		io.WriteString(w, `{"data":{"email":"a@b.co","phone":"0044 20 7946 0958"}}`)
	}))
	defer srv.Close()
	res := manifest.Execute(context.Background(), clientTo(srv), mk(t, base),
		map[string]string{"website": "https://www.acme.com/x", "contact_person": "Ada Lovelace"},
		creds(map[string]string{"VEND_KEY": "k123"}))
	if !res.Success || res.Error != "" || res.Confidence != 0.8 || res.HTTPStatus != 200 || res.DurationMS <= 0 {
		t.Fatalf("%+v", res)
	}
	if res.Fields["email"] != "a@b.co" || res.Fields["phone"] != "+442079460958" || res.Fields["src"] != "vend"+"or" {
		t.Fatalf("fields %+v", res.Fields)
	}
	if gotPath != "x=1&domain=acme.com" || gotAuth != "Bearer k123" || gotMethod != "POST" || gotCT != "application/json" {
		t.Fatalf("req %q %q %q %q", gotPath, gotAuth, gotMethod, gotCT)
	}
	if gotBody != `{"f":1.0,"n":3,"name":"Ada Lovelace"}` {
		t.Fatalf("body %s", gotBody)
	}
}

func TestExecuteHTTP429(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(429) }))
	defer srv.Close()
	res := manifest.Execute(context.Background(), clientTo(srv), mk(t, base), nil, creds(map[string]string{"VEND_KEY": "k"}))
	if res.Success || res.Error != "http_429" || res.HTTPStatus != 429 {
		t.Fatalf("%+v", res)
	}
}

func TestExecuteMalformed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"data":`) }))
	defer srv.Close()
	res := manifest.Execute(context.Background(), clientTo(srv), mk(t, base), nil, creds(map[string]string{"VEND_KEY": "k"}))
	if res.Success || res.Error != "non_json_response" {
		t.Fatalf("%+v", res)
	}
}

func TestExecuteNoData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"data":{"email":""}}`) }))
	defer srv.Close()
	m := mk(t, base)
	m.Response.Mappings = map[string]string{"email": "$.data.email"}
	res := manifest.Execute(context.Background(), clientTo(srv), m, nil, creds(map[string]string{"VEND_KEY": "k"}))
	if res.Success || res.Error != "no_data" || res.Confidence != 0.8 {
		t.Fatalf("%+v", res)
	}
}

func TestExecuteTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	m := mk(t, base)
	m.Request.Timeout = 0.2
	start := time.Now()
	res := manifest.Execute(context.Background(), clientTo(srv), m, nil, creds(map[string]string{"VEND_KEY": "k"}))
	if res.Success || res.Error != "timeout" || time.Since(start) > 3*time.Second {
		t.Fatalf("%+v", res)
	}
}

func TestExecuteVendorErrorEnvelope(t *testing.T) {
	long := strings.Repeat("é", 130)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"error":true,"message":"`+long+`","data":{"email":"a@b.co"}}`)
	}))
	defer srv.Close()
	res := manifest.Execute(context.Background(), clientTo(srv), mk(t, base), nil, creds(map[string]string{"VEND_KEY": "k"}))
	if res.Success || res.Error != strings.Repeat("é", 120) {
		t.Fatalf("%+v", res)
	}
	// no message path -> provider_error; message path missing -> "None"
	m := mk(t, base)
	m.Response.ErrorMessagePath = ""
	if r := manifest.Execute(context.Background(), clientTo(srv), m, nil, creds(map[string]string{"VEND_KEY": "k"})); r.Error != "provider_error" {
		t.Fatalf("%+v", r)
	}
	m.Response.ErrorMessagePath = "nope"
	if r := manifest.Execute(context.Background(), clientTo(srv), m, nil, creds(map[string]string{"VEND_KEY": "k"})); r.Error != "None" {
		t.Fatalf("%+v", r)
	}
}

func TestExecuteMissingKeyNoNetwork(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	for _, c := range []func(string) (string, error){creds(nil), creds(map[string]string{"VEND_KEY": ""}), nil} {
		res := manifest.Execute(context.Background(), clientTo(srv), mk(t, base), nil, c)
		if res.Success || res.Error != "vend: missing VEND_KEY" {
			t.Fatalf("%+v", res)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("server hit %d times", hits.Load())
	}
}

func TestExecuteBlockedURL(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	m := mk(t, base)
	m.Request.URL = "http://169.254.169.254/latest/{{input.domain}}"
	res := manifest.Execute(context.Background(), clientTo(srv), m, nil, creds(map[string]string{"VEND_KEY": "k"}))
	if res.Success || !strings.HasPrefix(res.Error, "blocked_url: ") || len([]rune(res.Error)) > len("blocked_url: ")+60 || hits.Load() != 0 {
		t.Fatalf("%+v hits=%d", res, hits.Load())
	}
	// Lead-controlled host: refused outright for a secret-bearing manifest...
	m.Request.URL = "http://{{input.domain}}/x"
	res = manifest.Execute(context.Background(), clientTo(srv), m, map[string]string{"website": "127.0.0.1"}, creds(map[string]string{"VEND_KEY": "k"}))
	if res.Error != "templated_authority" || hits.Load() != 0 {
		t.Fatalf("%+v", res)
	}
	// ...and SSRF-guarded for a keyless one.
	m.Auth = manifest.AuthSpec{Type: "none"}
	res = manifest.Execute(context.Background(), clientTo(srv), m, map[string]string{"website": "127.0.0.1"}, nil)
	if !strings.HasPrefix(res.Error, "blocked_url: ") || hits.Load() != 0 {
		t.Fatalf("%+v", res)
	}
}

func TestExecuteOversizeBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":{"email":"`)
		io.WriteString(w, strings.Repeat("a", 1<<20))
		io.WriteString(w, `"}}`)
	}))
	defer srv.Close()
	res := manifest.Execute(context.Background(), clientTo(srv), mk(t, base), nil, creds(map[string]string{"VEND_KEY": "k"}))
	if res.Success || res.Error != "non_json_response" {
		t.Fatalf("%+v", res)
	}
}

func TestExecuteTransportErrorDoesNotLeakURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	addr := srv.Listener.Addr().String()
	srv.Close() // connection refused
	cl := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, n, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, n, addr)
	}}}
	m := mk(t, base)
	m.Auth = manifest.AuthSpec{Type: "query", Param: "api_key", EnvVar: "VEND_KEY"}
	res := manifest.Execute(context.Background(), cl, m, nil, creds(map[string]string{"VEND_KEY": "TOPSECRET"}))
	if res.Success || res.Error == "" || strings.Contains(res.Error, "TOPSECRET") || strings.Contains(res.Error, "api.vendor.example") {
		t.Fatalf("%+v", res)
	}
}

func TestExecuteEnvAllowlist(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = append(seen, r.URL.String(), r.Header.Get("Authorization"), r.Header.Get("X-Lit"), string(b))
		io.WriteString(w, `{"data":{"email":"a@b.co"}}`)
	}))
	defer srv.Close()
	m := mk(t, `{"name":"vend","capability":"email",
	 "auth":{"type":"bearer","env_var":"VEND_KEY"},
	 "request":{"url":"http://api.vendor.example/v1?c={{input.company}}",
	   "headers":{"X-Lit":"${env:NAMED_VAR}|${env:ALSO_UNSET}"},
	   "body_template":{"company":"{{input.company}}","city":"{{input.city | default: ${env:SECRET_X}}}"}},
	 "response":{"mappings":{"email":"$.data.email"}}}`)
	all := map[string]string{"VEND_KEY": "KEYVAL", "NAMED_VAR": "named", "UNNAMED_VAR": "SHOULD-NOT-APPEAR", "SECRET_X": "TOPSECRET"}
	res := manifest.Execute(context.Background(), clientTo(srv), m,
		map[string]string{"company": "${env:SECRET_X}", "city": "${env:UNNAMED_VAR}"}, creds(all))
	if !res.Success {
		t.Fatalf("%+v", res)
	}
	joined := strings.Join(seen, "\n")
	for _, bad := range []string{"TOPSECRET", "SHOULD-NOT-APPEAR"} {
		if strings.Contains(joined, bad) {
			t.Fatalf("secret %s leaked:\n%s", bad, joined)
		}
	}
	if seen[1] != "Bearer KEYVAL" || seen[2] != "named|" {
		t.Fatalf("seen %q", seen)
	}
	if !strings.Contains(seen[0], "c=%24%7Benv%3ASECRET_X%7D") && !strings.Contains(seen[0], "${env:SECRET_X}") {
		t.Fatalf("injected literal missing from url: %s", seen[0])
	}
}

func TestManifestDefaultsAndNumbers(t *testing.T) {
	m := mk(t, `{"name":"n","capability":"c","request":{"url":"https://x.example","headers":{"A":1,"B":1.0,"C":true,"D":null}}}`)
	if m.Request.Method != "POST" || m.Request.Timeout != 20.0 || m.DefaultConfidence != 0.7 || m.ManifestVersion != "1" || m.Auth.Type != "none" {
		t.Fatalf("%+v", m)
	}
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = []string{r.Header.Get("A"), r.Header.Get("B"), r.Header.Get("C"), r.Header.Get("D")}
		io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	m.Request.URL = "http://x.example/"
	manifest.Execute(context.Background(), clientTo(srv), m, nil, nil)
	if strings.Join(got, ",") != "1,1.0,True,None" {
		t.Fatalf("%v", got)
	}
}

type blockedVec struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	AllowHTTP bool   `json:"allow_http"`
	Reason    string `json:"reason"`
	Error     string `json:"error"`
}

func TestBlockedURLVectors(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/manifest_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Blocked []blockedVec `json:"blocked_url"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Blocked) < 20 {
		t.Fatalf("only %d vectors", len(f.Blocked))
	}
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	for _, v := range f.Blocked {
		// Guard level: Reason must equal Python's BlockedUrlError message.
		var be *egress.BlockedError
		if err := egress.CheckURL(v.URL, v.AllowHTTP); !errors.As(err, &be) || be.Reason != v.Reason || !errors.Is(err, egress.ErrBlocked) {
			t.Errorf("%s: CheckURL err=%v want reason %q", v.Name, err, v.Reason)
		}
		if !v.AllowHTTP {
			continue // Execute always passes allow_http=true
		}
		m := mk(t, `{"name":"v","capability":"c","request":{"url":"x"},"response":{"mappings":{"a":"$.a"}}}`)
		m.Request.URL = v.URL
		res := manifest.Execute(context.Background(), clientTo(srv), m, nil, nil)
		if res.Success || res.Error != v.Error {
			t.Errorf("%s: got %q want %q", v.Name, res.Error, v.Error)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("network hit %d times", hits.Load())
	}
}

func TestExecuteDeeplyNestedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Repeat("[", 1<<20))
	}))
	defer srv.Close()
	res := manifest.Execute(context.Background(), clientTo(srv), mk(t, base), nil, creds(map[string]string{"VEND_KEY": "k"}))
	if res.Success || res.Error != "non_json_response" {
		t.Fatalf("%+v", res)
	}
}

// A vendor error body that echoes the API key must not reach the cell error.
func TestExecuteScrubsEchoedCredential(t *testing.T) {
	const key = "sk_live_abc123XYZ"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"error":true,"message":"Invalid key `+key+` for account"}`)
	}))
	defer srv.Close()
	res := manifest.Execute(context.Background(), clientTo(srv), mk(t, base), nil, creds(map[string]string{"VEND_KEY": key}))
	if strings.Contains(res.Error, key) || !strings.Contains(res.Error, "Invalid key ***") {
		t.Fatalf("error %q", res.Error)
	}
}
