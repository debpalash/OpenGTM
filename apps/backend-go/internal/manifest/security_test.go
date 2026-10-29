package manifest_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
)

func mkURL(t *testing.T, url, auth string) manifest.Manifest {
	t.Helper()
	return mk(t, `{"name":"vend","capability":"email","auth":`+auth+`,
 "request":{"method":"get","url":"`+url+`"},"response":{"mappings":{"email":"$.email"}}}`)
}

const bearerEnv = `{"type":"bearer","env_var":"VEND_KEY"}`

func TestSecurityProblem(t *testing.T) {
	for _, c := range []struct{ url, auth, want string }{
		{"https://api.example.com/v1/{{input.domain}}", bearerEnv, ""},
		{"https://{{input.domain}}/v1", bearerEnv, "templated_authority"},
		{"https://api.{{input.domain}}:443/v1", bearerEnv, "templated_authority"},
		{"{{input.base}}/v1", bearerEnv, "templated_authority"},
		{"https://api.example.com{{input.x}}/v1", bearerEnv, "templated_authority"},
		{"https://{{input.domain}}/v1", `{"type":"none"}`, ""},
		{"https://{{input.domain}}/${env:K_X}", `{"type":"none"}`, "templated_authority"},
		{"https://api.example.com/v1", `{"type":"header","param":"X-K","value":"sk-literal"}`, "literal_auth_value"},
		{"https://api.example.com/v1", `{"type":"header","param":"X-K","value":"${env:VEND_KEY}"}`, ""},
	} {
		if got := manifest.SecurityProblem(mkURL(t, c.url, c.auth)); got != c.want {
			t.Errorf("%s %s: got %q want %q", c.url, c.auth, got, c.want)
		}
	}
}

func TestExecuteRefusesBeforeCredentialOrNetwork(t *testing.T) {
	var hits, credCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	cred := func(string) (string, error) { credCalls.Add(1); return "secret", nil }
	for url, auth := range map[string]string{
		"https://{{input.domain}}/v1": bearerEnv,
	} {
		res := manifest.Execute(context.Background(), clientTo(srv), mkURL(t, url, auth), map[string]string{"website": "evil.test"}, cred)
		if res.Success || res.Error != "templated_authority" {
			t.Fatalf("%+v", res)
		}
	}
	res := manifest.Execute(context.Background(), clientTo(srv), mkURL(t, "https://api.example.com/v1", `{"type":"header","value":"sk-lit"}`), nil, cred)
	if res.Error != "literal_auth_value" {
		t.Fatalf("%+v", res)
	}
	if hits.Load() != 0 || credCalls.Load() != 0 {
		t.Fatalf("network=%d cred=%d", hits.Load(), credCalls.Load())
	}
}

func TestPrepareNoNetworkErrors(t *testing.T) {
	m := mkURL(t, "https://api.example.com/v1", bearerEnv)
	if _, e := manifest.Prepare(m, nil, creds(nil), nil); e != "vend: missing VEND_KEY" {
		t.Fatalf("got %q", e)
	}
	b := mkURL(t, "http://127.0.0.1/v1", bearerEnv)
	if _, e := manifest.Prepare(b, nil, creds(map[string]string{"VEND_KEY": "k"}), nil); !strings.HasPrefix(e, "blocked_url:") {
		t.Fatalf("got %q", e)
	}
	p, e := manifest.Prepare(m, nil, creds(map[string]string{"VEND_KEY": "k"}), nil)
	if e != "" || p.Headers["Authorization"] != "Bearer k" {
		t.Fatalf("%v %q", p, e)
	}
}

func TestExecuteCheckedAllowlistGate(t *testing.T) {
	m := mkURL(t, "http://10.0.0.5/v1", bearerEnv)
	c := creds(map[string]string{"VEND_KEY": "k"})
	prod := func(raw string) error { return egress.CheckURLFor("production", []string{"10.0.0.5"}, raw, true) }
	if _, e := manifest.Prepare(m, nil, c, prod); !strings.HasPrefix(e, "blocked_url:") {
		t.Fatalf("allowlist honoured in production: %q", e)
	}
	test := func(raw string) error { return egress.CheckURLFor("test", []string{"10.0.0.5"}, raw, true) }
	if _, e := manifest.Prepare(m, nil, c, test); e != "" {
		t.Fatalf("allowlist ignored in test: %q", e)
	}
}
