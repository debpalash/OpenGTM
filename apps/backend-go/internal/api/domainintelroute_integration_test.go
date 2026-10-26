//go:build integration

package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
)

// Plan 14 Task 19: POST /api/leads/domain-intel against the recorded Python fixtures
// (testdata/contract/leadutils/domain_intel_*). The recorder ran with every upstream down, so the 200 fixtures
// carry the empty analysis; the egress client here is a fake transport (never the internet) that records every
// host the route asks for.

// intelTransport answers by host: handlers[host] (nil entry or missing host: connection error). Every requested
// host:path is recorded.
type intelTransport struct {
	mu       sync.Mutex
	hosts    []string
	handlers map[string]func(*http.Request) (*http.Response, error)
}

func (t *intelTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.hosts = append(t.hosts, r.URL.Host)
	h := t.handlers[r.URL.Host]
	t.mu.Unlock()
	if h == nil {
		return nil, errors.New("upstream down")
	}
	return h(r)
}

func (t *intelTransport) requested() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.hosts...)
}

func intelReply(code int, body string) func(*http.Request) (*http.Response, error) {
	return func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: code, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	}
}

// intelUpstreams is crt.sh answering an empty list (so the lookup ends at once instead of retrying with real
// sleeps) while DoH and RDAP are down: the recorded "upstreams down" shape.
func intelUpstreams() *intelTransport {
	return &intelTransport{handlers: map[string]func(*http.Request) (*http.Response, error){"crt.sh": intelReply(200, `[]`)}}
}

func newIntelEnv(t *testing.T, tr *intelTransport) *utilsEnv {
	t.Helper()
	return newUtilsEnv(t, func(d *Deps) {
		d.Egress = &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	})
}

func intelActor(name string) (who, ws string) {
	switch {
	case strings.HasSuffix(name, "_no_auth"):
		return "", ""
	case strings.HasSuffix(name, "_viewer_403"):
		return "bob", "ws-alpha"
	}
	return "alice", ""
}

func TestDomainIntelMatchesFixtures(t *testing.T) {
	e := newIntelEnv(t, intelUpstreams())
	n := 0
	for _, full := range contractFixtureNames(t, "leadutils") {
		name := strings.TrimPrefix(full, "leadutils/")
		if !strings.HasPrefix(name, "domain_intel_") {
			continue
		}
		n++
		fx := contract.Load(t, full)
		who, ws := intelActor(name)
		body, _ := json.Marshal(fx.Request.Body)
		rec := e.post(who, ws, "/api/leads/domain-intel", string(body))
		got := contract.Resp{Status: rec.Code, Headers: map[string]string{}, Body: json.RawMessage(rec.Body.Bytes())}
		for _, k := range []string{"Content-Type", "WWW-Authenticate", "Allow"} {
			if v := rec.Header().Get(k); v != "" {
				got.Headers[k] = v
			}
		}
		t.Run(name, func(t *testing.T) { contract.AssertEqual(t, got, fx.Response) })
	}
	if n != 18 {
		t.Errorf("%d domain_intel fixtures replayed, want 18", n)
	}
}

func TestDomainIntelValidation400Variants(t *testing.T) {
	tr := intelUpstreams()
	e := newIntelEnv(t, tr)
	for _, c := range []struct{ body, detail string }{
		{`{}`, "domain is required"}, {`{"domain": null}`, "domain is required"}, {`{"domain": 5}`, "domain is required"},
		{`{"domain": true}`, "domain is required"}, {`{"domain": {}}`, "domain is required"}, {`{"domain": ""}`, "domain is required"},
		{`{"domain": " \t\n"}`, "domain is required"}, {`{"other": "example.com"}`, "domain is required"},
		{`{"domain": "localhost"}`, "domain must be a valid domain name or URL"},
		{`{"domain": "a..b.com"}`, "domain must be a valid domain name or URL"},
		{`{"domain": "http://"}`, "domain must be a valid domain name or URL"},
		{`{"domain": "https://["}`, "domain must be a valid domain name or URL"},
		{`{"domain": "example.com：443"}`, "domain must be a valid domain name or URL"},
	} {
		rec := e.post("alice", "", "/api/leads/domain-intel", c.body)
		var b map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &b)
		if rec.Code != 400 || b["detail"] != c.detail {
			t.Errorf("%s: %d %s", c.body, rec.Code, rec.Body)
		}
	}
	if h := tr.requested(); len(h) != 0 {
		t.Errorf("a rejected domain caused outbound requests: %v", h)
	}
}

func TestDomainIntelNonObjectBody422(t *testing.T) {
	tr := intelUpstreams()
	e := newIntelEnv(t, tr)
	for _, body := range []string{`["example.com"]`, `"example.com"`, `123`, `null`, `true`, `not json`, ``} {
		rec := e.post("alice", "", "/api/leads/domain-intel", body)
		if rec.Code != 422 {
			t.Errorf("%q: %d %s", body, rec.Code, rec.Body)
		}
	}
	// the recorded list body keeps FastAPI's dict_type error
	fx := contract.Load(t, "leadutils/domain_intel_not_json_object_422")
	rec := e.post("alice", "", "/api/leads/domain-intel", `["example.com"]`)
	contract.AssertEqual(t, contract.Resp{Status: rec.Code, Headers: map[string]string{"Content-Type": rec.Header().Get("Content-Type")}, Body: json.RawMessage(rec.Body.Bytes())}, fx.Response)
	if len(tr.requested()) != 0 {
		t.Error("422 caused outbound requests")
	}
}

func TestDomainIntelViewer403(t *testing.T) {
	tr := intelUpstreams()
	e := newIntelEnv(t, tr)
	rec := e.post("bob", "ws-alpha", "/api/leads/domain-intel", `{"domain": "example.com"}`)
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "tables.write") {
		t.Errorf("viewer: %d %s", rec.Code, rec.Body)
	}
	if rec = e.post("erin", "ws-alpha", "/api/leads/domain-intel", `{"domain": "example.com"}`); rec.Code != 200 {
		t.Errorf("editor: %d %s", rec.Code, rec.Body)
	}
	if len(tr.requested()) == 0 {
		t.Error("editor request did not analyse")
	}
}

func TestDomainIntelPartialResultOnDNSFailure(t *testing.T) {
	tr := &intelTransport{handlers: map[string]func(*http.Request) (*http.Response, error){
		"crt.sh":   intelReply(200, `[{"name_value": "mail.example.com\n*.careers.example.com\nwww.example.com"}]`),
		"rdap.org": intelReply(500, `oops`),
	}} // DoH (cloudflare-dns.com) is down, RDAP errors: the answer is 200 with what crt.sh gave
	e := newIntelEnv(t, tr)
	rec := e.post("alice", "", "/api/leads/domain-intel", `{"domain": "Example.COM"}`)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var b struct {
		Domain string `json:"domain"`
		DNS    struct {
			A []string `json:"a_records"`
		} `json:"dns"`
		CT struct {
			Count int      `json:"subdomain_count"`
			Subs  []string `json:"subdomains"`
			Inter []string `json:"interesting"`
		} `json:"cert_transparency"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if b.Domain != "example.com" || len(b.DNS.A) != 0 || b.CT.Count != 3 || len(b.CT.Inter) != 2 {
		t.Errorf("partial result: %s", rec.Body)
	}
	if !strings.HasPrefix(rec.Body.String(), `{"domain":"example.com",`) {
		t.Errorf("key order: %.80s", rec.Body)
	}
}

// TestDomainIntelNonObjectCRTRow500: a crt.sh row that is not an object makes Python's analyze_domain raise (an
// AttributeError outside its try block), which FastAPI answers 500; Go keeps the status and does not panic.
func TestDomainIntelNonObjectCRTRow500(t *testing.T) {
	tr := &intelTransport{handlers: map[string]func(*http.Request) (*http.Response, error){"crt.sh": intelReply(200, `[1, {"name_value": "a.example.com"}]`)}}
	e := newIntelEnv(t, tr)
	rec := e.post("alice", "", "/api/leads/domain-intel", `{"domain": "example.com"}`)
	if rec.Code != 500 || strings.Contains(rec.Body.String(), "crt.sh") {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}

// TestDomainIntelNoPrivateHostFetch: whatever the domain looks like, the only hosts the server contacts are the
// three fixed upstreams; the user's value is data in their query, never a destination.
func TestDomainIntelNoPrivateHostFetch(t *testing.T) {
	tr := intelUpstreams()
	e := newIntelEnv(t, tr)
	for _, d := range []string{"localhost", "10.0.0.1", "[::1]", "169.254.169.254", "127.0.0.1", "http://10.0.0.1/admin", "user@169.254.169.254", "example.com@evil.test", "evil.test#@example.com", "internal.corp", "example.com/../../etc"} {
		body, _ := json.Marshal(map[string]string{"domain": d})
		if rec := e.post("alice", "", "/api/leads/domain-intel", string(body)); rec.Code != 200 && rec.Code != 400 {
			t.Errorf("%s: %d", d, rec.Code)
		}
	}
	allowed := map[string]bool{"cloudflare-dns.com": true, "rdap.org": true, "crt.sh": true}
	for _, h := range tr.requested() {
		if !allowed[h] {
			t.Errorf("outbound request to %q", h)
		}
	}
}

// TestDomainIntelRateLimited: the analysis is expensive outbound work (about 15 requests), so the route is limited
// to 30/minute per workspace key (Python has no limiter here: deviation); validation failures do not count.
func TestDomainIntelRateLimited(t *testing.T) {
	tr := intelUpstreams()
	e := newIntelEnv(t, tr)
	for i := 0; i < 5; i++ {
		if rec := e.post("alice", "", "/api/leads/domain-intel", `{}`); rec.Code != 400 {
			t.Fatalf("400 expected: %d", rec.Code)
		}
	}
	for i := 0; i < 30; i++ {
		if rec := e.post("alice", "", "/api/leads/domain-intel", `{"domain": "example.com"}`); rec.Code != 200 {
			t.Fatalf("request %d: %d %s", i+1, rec.Code, rec.Body)
		}
	}
	rec := e.post("alice", "", "/api/leads/domain-intel", `{"domain": "example.com"}`)
	if rec.Code != 429 {
		t.Errorf("31st request: %d %s", rec.Code, rec.Body)
	}
	if rec = e.post("alice", "", "/api/leads/domain-intel", `{}`); rec.Code != 400 {
		t.Errorf("validation after limit: %d", rec.Code)
	}
}
