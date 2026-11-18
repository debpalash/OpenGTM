package manifest_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
)

// Tests for the Go-only manifest extensions: response.value_maps, request.omit_empty and URL
// escaping of substituted values. Manifests without them are covered by the older tests and the
// Python parity vectors.

func recorder(t *testing.T, body string) (*httptest.Server, *struct {
	uri, query, rawBody string
	header              http.Header
}) {
	t.Helper()
	seen := &struct {
		uri, query, rawBody string
		header              http.Header
	}{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen.uri, seen.query, seen.rawBody, seen.header = r.RequestURI, r.URL.RawQuery, string(b), r.Header.Clone()
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, seen
}

const verifyManifest = `{"name":"verif","capability":"email_verify","default_confidence":0.9,
 "auth":{"type":"query","param":"key","env_var":"VEND_KEY"},
 "request":{"method":"GET","url":"http://api.vendor.example/check"},
 "response":{"mappings":{"email_verify":"$.status","other":"$.note"},
  "value_maps":{"email_verify":{"values":{"deliverable":"valid","undeliverable":"invalid","1":"valid"},"default":"unknown"}}}}`

func runVerify(t *testing.T, m manifest.Manifest, resp string) manifest.Result {
	t.Helper()
	srv, _ := recorder(t, resp)
	return manifest.Execute(context.Background(), clientTo(srv), m, nil, creds(map[string]string{"VEND_KEY": "k"}))
}

func TestValueMapTranslatesAndDefaults(t *testing.T) {
	m := mk(t, verifyManifest)
	cases := []struct{ resp, want string }{
		{`{"status":"deliverable"}`, "valid"},
		{`{"status":"undeliverable"}`, "invalid"},
		{`{"status":1}`, "valid"},                  // a number matches its text
		{`{"status":"brand_new_word"}`, "unknown"}, // not listed: the explicit default
		{`{"status":"Deliverable"}`, "unknown"},    // matching is exact
	}
	for _, c := range cases {
		res := runVerify(t, m, c.resp)
		if !res.Success || res.Fields["email_verify"] != c.want {
			t.Errorf("%s: %+v, want %q", c.resp, res, c.want)
		}
	}
	// Only the mapped field is translated.
	res := runVerify(t, m, `{"status":"deliverable","note":"deliverable"}`)
	if res.Fields["other"] != "deliverable" {
		t.Errorf("unlisted field changed: %v", res.Fields)
	}
}

func TestValueMapMissingValueIsNotMapped(t *testing.T) {
	m := mk(t, verifyManifest)
	for _, resp := range []string{`{}`, `{"status":null}`, `{"status":""}`} {
		res := runVerify(t, m, resp)
		if res.Success || res.Error != "no_data" || len(res.Fields) != 0 {
			t.Errorf("%s: the default must not invent a value: %+v", resp, res)
		}
	}
}

func TestValueMapWithoutDefaultPassesThrough(t *testing.T) {
	m := mk(t, `{"name":"v","capability":"email_verify","request":{"url":"http://api.vendor.example/c"},
	 "response":{"mappings":{"email_verify":"$.s"},"value_maps":{"email_verify":{"values":{"ok":"valid"}}}}}`)
	if res := runVerify(t, m, `{"s":"ok"}`); res.Fields["email_verify"] != "valid" {
		t.Fatalf("%+v", res)
	}
	if res := runVerify(t, m, `{"s":"other"}`); res.Fields["email_verify"] != "other" {
		t.Fatalf("%+v", res)
	}
}

// A value map is data, not a template: neither side is ever rendered, so it cannot pull in lead
// values or credentials, and a vendor value is never treated as a template either.
func TestValueMapCannotInjectTemplates(t *testing.T) {
	m := mk(t, `{"name":"v","capability":"email_verify",
	 "auth":{"type":"header","param":"X-K","env_var":"VEND_KEY","extra_env":["VEND_KEY"]},
	 "request":{"url":"http://api.vendor.example/c"},
	 "response":{"mappings":{"email_verify":"$.s"},"value_maps":{"email_verify":{
	   "values":{"a":"{{input.email}}","b":"${env:VEND_KEY}","{{input.email}}":"c"},"default":"${env:VEND_KEY}"}}}}`)
	run := func(body string) manifest.Result {
		srv, _ := recorder(t, body)
		return manifest.Execute(context.Background(), clientTo(srv), m, map[string]string{"email": "lead@x.io"},
			creds(map[string]string{"VEND_KEY": "SECRET-VALUE"}))
	}
	for body, want := range map[string]string{
		`{"s":"a"}`:               "{{input.email}}",
		`{"s":"b"}`:               "${env:VEND_KEY}",
		`{"s":"{{input.email}}"}`: "c", // a vendor value is matched as text, never rendered
		`{"s":"zzz"}`:             "${env:VEND_KEY}",
	} {
		got, _ := run(body).Fields["email_verify"].(string)
		if got != want || strings.Contains(got, "SECRET-VALUE") || strings.Contains(got, "lead@x.io") {
			t.Errorf("%s -> %q, want literal %q", body, got, want)
		}
	}
}

func TestOmitEmptyQueryBodyHeader(t *testing.T) {
	m := mk(t, `{"name":"o","capability":"email","auth":{"type":"query","param":"api","env_var":"VEND_KEY"},
	 "request":{"method":"POST","url":"http://api.vendor.example/f",
	  "query":{"domain":"{{input.domain}}","company":"{{input.company}}","fixed":""},
	  "headers":{"X-Co":"{{input.company}}","X-Static":"yes"},
	  "body_template":{"company_name":"{{input.company}}","full_name":"{{input.full_name}}","note":""},
	  "omit_empty":["domain","company","X-Co","company_name","full_name"]},
	 "response":{"mappings":{"email":"$.e"}}}`)

	srv, seen := recorder(t, `{"e":"a@b.co"}`)
	manifest.Execute(context.Background(), clientTo(srv), m, map[string]string{"website": "acme.com"}, creds(map[string]string{"VEND_KEY": "k"}))
	// Empty company/full_name are gone; "fixed" and "note" are not listed so they are still sent empty.
	if seen.query != "api=k&domain=acme.com&fixed=" {
		t.Errorf("query %q", seen.query)
	}
	if seen.header.Get("X-Co") != "" || seen.header.Get("X-Static") != "yes" {
		t.Errorf("headers %v", seen.header)
	}
	if seen.rawBody != `{"note":""}` {
		t.Errorf("body %s", seen.rawBody)
	}

	manifest.Execute(context.Background(), clientTo(srv), m,
		map[string]string{"website": "acme.com", "company": "Acme", "contact_person": "Ada Lovelace"}, creds(map[string]string{"VEND_KEY": "k"}))
	if seen.query != "api=k&company=Acme&domain=acme.com&fixed=" {
		t.Errorf("query %q", seen.query)
	}
	if seen.header.Get("X-Co") != "Acme" {
		t.Errorf("headers %v", seen.header)
	}
	if !strings.Contains(seen.rawBody, `"company_name":"Acme"`) || !strings.Contains(seen.rawBody, `"full_name":"Ada Lovelace"`) {
		t.Errorf("body %s", seen.rawBody)
	}
}

// omit_empty never removes the credential, even if it names the auth parameter.
func TestOmitEmptyKeepsCredential(t *testing.T) {
	m := mk(t, `{"name":"o","capability":"email","auth":{"type":"query","param":"api","env_var":"VEND_KEY"},
	 "request":{"method":"GET","url":"http://api.vendor.example/f","omit_empty":["api"]},
	 "response":{"mappings":{"email":"$.e"}}}`)
	srv, seen := recorder(t, `{"e":"a@b.co"}`)
	manifest.Execute(context.Background(), clientTo(srv), m, nil, creds(map[string]string{"VEND_KEY": "k"}))
	if seen.query != "api=k" {
		t.Errorf("query %q", seen.query)
	}
}

func TestURLPathValuesAreEscaped(t *testing.T) {
	m := mk(t, `{"name":"u","capability":"email","request":{"method":"GET","url":"http://api.vendor.example/v1/c/{{input.company}}/info"},
	 "response":{"mappings":{"email":"$.e"}}}`)
	srv, seen := recorder(t, `{"e":"a@b.co"}`)
	for company, wantURI := range map[string]string{
		"acme":     "/v1/c/acme/info",
		"a/b":      "/v1/c/a%2Fb/info",
		"a?x=1":    "/v1/c/a%3Fx=1/info",
		"a#frag":   "/v1/c/a%23frag/info",
		"..":       "/v1/c/%2E%2E/info",
		".":        "/v1/c/%2E/info",
		"../admin": "/v1/c/.%2E%2Fadmin/info",
		"a b":      "/v1/c/a%20b/info",
		"100%":     "/v1/c/100%25/info",
		"acme.com": "/v1/c/acme.com/info",
	} {
		manifest.Execute(context.Background(), clientTo(srv), m, map[string]string{"company": company}, nil)
		if seen.uri != wantURI {
			t.Errorf("company %q: request URI %q, want %q", company, seen.uri, wantURI)
		}
		if strings.Contains(seen.uri, "/../") || strings.Contains(seen.uri, "/./") || strings.Contains(seen.uri, "?") {
			t.Errorf("company %q escaped the path: %q", company, seen.uri)
		}
	}
}

func TestURLQueryValuesAreEscaped(t *testing.T) {
	m := mk(t, `{"name":"u","capability":"email","request":{"method":"GET","url":"http://api.vendor.example/c?q={{input.company}}&v=2"},
	 "response":{"mappings":{"email":"$.e"}}}`)
	srv, seen := recorder(t, `{"e":"a@b.co"}`)
	manifest.Execute(context.Background(), clientTo(srv), m, map[string]string{"company": "a&admin=1#x y"}, nil)
	if seen.query != "q=a%26admin%3D1%23x+y&v=2" {
		t.Errorf("query %q", seen.query)
	}
}

// A default and manifest text are written as authored; only substituted lead values are escaped.
func TestURLDefaultIsNotEscaped(t *testing.T) {
	m := mk(t, `{"name":"u","capability":"email","request":{"method":"GET","url":"http://api.vendor.example/{{input.company | default: v1/x}}"},
	 "response":{"mappings":{"email":"$.e"}}}`)
	srv, seen := recorder(t, `{"e":"a@b.co"}`)
	manifest.Execute(context.Background(), clientTo(srv), m, nil, nil)
	if seen.uri != "/v1/x" {
		t.Errorf("uri %q", seen.uri)
	}
}

// The new fields are covered by the canonical form (so a signature covers them) and are
// absent from it when not declared (so existing signatures stay valid).
func TestCanonicalManifestCoversExtensions(t *testing.T) {
	plain := mk(t, `{"name":"c","capability":"email","request":{"url":"http://a.example/x"},"response":{"mappings":{"email":"$.e"}}}`)
	withVM := mk(t, `{"name":"c","capability":"email","request":{"url":"http://a.example/x"},"response":{"mappings":{"email":"$.e"},"value_maps":{"email":{"values":{"a":"b"},"default":"z"}}}}`)
	withOmit := mk(t, `{"name":"c","capability":"email","request":{"url":"http://a.example/x","omit_empty":["q"]},"response":{"mappings":{"email":"$.e"}}}`)
	p, _ := manifest.CanonicalManifest(plain)
	v, _ := manifest.CanonicalManifest(withVM)
	o, _ := manifest.CanonicalManifest(withOmit)
	if bytes.Contains(p, []byte("value_maps")) || bytes.Contains(p, []byte("omit_empty")) {
		t.Errorf("undeclared extensions leaked into the canonical form: %s", p)
	}
	if !bytes.Contains(v, []byte(`"value_maps"`)) || bytes.Equal(p, v) {
		t.Errorf("value_maps not in canonical form: %s", v)
	}
	if !bytes.Contains(o, []byte(`"omit_empty"`)) || bytes.Equal(p, o) {
		t.Errorf("omit_empty not in canonical form: %s", o)
	}
}
