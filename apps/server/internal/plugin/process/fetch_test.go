//go:build unix

package process

import (
	"net/http"
	"strings"
	"sync"
	"testing"
)

func fetchReplies(t *testing.T, out *Outcome) []map[string]any {
	t.Helper()
	var rows []map[string]any
	for _, r := range asMap(t, fieldsOf(t, out))["replies"].([]any) {
		rows = append(rows, asMap(t, r))
	}
	return rows
}

func errCode(row map[string]any) string {
	if e, ok := row["error"].(map[string]any); ok {
		return e["code"].(string)
	}
	return ""
}

func TestFetchGoesThroughTheGuardedClientAndRecordsEvidence(t *testing.T) {
	var mu sync.Mutex
	var seen []*http.Request
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		seen = append(seen, r)
		mu.Unlock()
		if r.URL.Path == "/robots.txt" {
			return respond(404, "", nil), nil
		}
		return respond(200, `{"ok":true}`, map[string]string{"Content-Type": "application/json", "X-Vendor": "v1"}), nil
	})
	s := newSup(t, Options{Client: testClient(t, rt)})
	p := pspec{kind: "provider", network: []string{"https://api.example.com/v1"}, secrets: []string{"API_KEY"}}.build(t)

	out := mustRun(t, s, p, map[string]any{"mode": "fetch", "requests": []any{
		map[string]any{"url": "https://api.example.com/v1/companies?domain=acme.example", "headers": map[string]any{"X-Api-Key": "sk-test-key-123"}},
	}}, map[string]string{"API_KEY": "sk-test-key-123"})
	rows := fetchReplies(t, out)
	if rows[0]["status"] != float64(200) || rows[0]["body"] != `{"ok":true}` {
		t.Fatalf("reply: %v", rows[0])
	}
	mu.Lock()
	defer mu.Unlock()
	last := seen[len(seen)-1]
	if got := last.Header.Get("X-Api-Key"); got != "sk-test-key-123" {
		t.Fatalf("header not forwarded: %q", got)
	}
	if ua := last.Header.Get("User-Agent"); !strings.HasPrefix(ua, "OpenGTM/") {
		t.Fatalf("user agent %q: plugins must not control it", ua)
	}
	// Host-observed evidence is attached to the record, independent of the plugin.
	fetches := out.Records[0].Evidence["fetches"].([]FetchEvidence)
	if len(fetches) != 1 || fetches[0].Status != 200 || len(fetches[0].SHA256) != 64 || !strings.HasPrefix(fetches[0].URL, "https://api.example.com/v1/companies") {
		t.Fatalf("evidence: %+v", fetches)
	}
	if out.Pages != 1 {
		t.Fatalf("pages: %d", out.Pages)
	}
}

func TestFetchEnforcesTheNetworkCapability(t *testing.T) {
	var hits int
	var mu sync.Mutex
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		hits++
		mu.Unlock()
		if r.URL.Path == "/v1/redirect-out" {
			return &http.Response{StatusCode: 302, Header: http.Header{"Location": {"https://evil.example.net/steal"}}, Body: http.NoBody}, nil
		}
		return respond(200, "ok", nil), nil
	})
	s := newSup(t, Options{Client: testClient(t, rt)})
	p := pspec{network: []string{"https://api.example.com/v1"}}.build(t)

	reqs := []any{
		map[string]any{"url": "https://api.example.com/v1/ok"},                    // allowed
		map[string]any{"url": "https://api.example.com/v10/other"},                // path prefix is not a substring match
		map[string]any{"url": "https://api.example.com/v1/../admin"},              // dot-dot
		map[string]any{"url": "https://evil.example.net/"},                        // other host
		map[string]any{"url": "http://api.example.com/v1/ok"},                     // downgrade to http
		map[string]any{"url": "https://api.example.com:8443/v1/ok"},               // other port
		map[string]any{"url": "https://api.example.com/v1/redirect-out"},          // redirect leaves the capability
		map[string]any{"url": "file:///etc/passwd"},                               // not http
		map[string]any{"url": "https://api.example.com/v1/ok", "method": "TRACE"}, // methods are validated by the protocol
	}
	out := mustRun(t, s, p, map[string]any{"mode": "fetch", "requests": reqs}, nil)
	rows := fetchReplies(t, out)
	if rows[0]["status"] != float64(200) {
		t.Fatalf("allowed request failed: %v", rows[0])
	}
	for i := 1; i < 8; i++ {
		if c := errCode(rows[i]); c != "capability_denied" {
			t.Errorf("request %d (%v): got %v, want capability_denied", i, reqs[i], rows[i])
		}
	}
	if c := errCode(rows[8]); c != "bad_request" {
		t.Errorf("TRACE: got %v, want bad_request", rows[8])
	}
	mu.Lock()
	defer mu.Unlock()
	// robots is not consulted for providers; only the allowed request and
	// the redirecting one (before its redirect was refused) reached the network.
	if hits != 2 {
		t.Fatalf("%d requests reached the transport, want 2", hits)
	}
}

func TestFetchStillAppliesTheSSRFGuard(t *testing.T) {
	s := newSup(t, Options{Client: testClient(t, okTransport())})
	// The capability allows any https host; the destination guard must still
	// refuse private, loopback, link-local and metadata addresses.
	p := pspec{network: []string{"https://*"}}.build(t)
	urls := []string{
		"https://127.0.0.1/", "https://localhost/", "https://169.254.169.254/latest/meta-data/",
		"https://10.0.0.5/", "https://[::1]/", "https://2130706433/", "https://0x7f.1/",
	}
	var reqs []any
	for _, u := range urls {
		reqs = append(reqs, map[string]any{"url": u})
	}
	rows := fetchReplies(t, mustRun(t, s, p, map[string]any{"mode": "fetch", "requests": reqs}, nil))
	for i, row := range rows {
		if c := errCode(row); c != "blocked_url" {
			t.Errorf("%s: got %v, want blocked_url", urls[i], row)
		}
	}
}

func TestFetchBudgetFollowsMaxPages(t *testing.T) {
	s := newSup(t, Options{Client: testClient(t, okTransport())})
	p := pspec{network: []string{"https://api.example.com"}, maxPages: 2}.build(t)
	var reqs []any
	for i := 0; i < 4; i++ {
		reqs = append(reqs, map[string]any{"url": "https://api.example.com/p"})
	}
	rows := fetchReplies(t, mustRun(t, s, p, map[string]any{"mode": "fetch", "requests": reqs}, nil))
	if rows[0]["status"] != float64(200) || rows[1]["status"] != float64(200) {
		t.Fatalf("first two fetches: %v", rows[:2])
	}
	if errCode(rows[2]) != "too_many_fetches" || errCode(rows[3]) != "too_many_fetches" {
		t.Fatalf("over budget: %v", rows[2:])
	}
}

func TestFetchBudgetHoldsUnderConcurrency(t *testing.T) {
	s := newSup(t, Options{Client: testClient(t, okTransport())})
	p := pspec{network: []string{"https://api.example.com"}, maxPages: 5}.build(t)
	out := mustRun(t, s, p, map[string]any{"mode": "fetch_concurrent", "n": 12, "url": "https://api.example.com/x"}, nil)
	ok := 0
	for _, st := range fieldsOf(t, out)["statuses"].([]any) {
		if st == float64(200) {
			ok++
		}
	}
	if ok != 5 {
		t.Fatalf("%d of 12 concurrent fetches succeeded with a budget of 5", ok)
	}
}

func TestScrapersHonourRobotsAndProvidersDoNot(t *testing.T) {
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/robots.txt" {
			return respond(200, "User-agent: *\nDisallow: /private\n", nil), nil
		}
		return respond(200, "page", nil), nil
	})
	s := newSup(t, Options{Client: testClient(t, rt)})
	in := map[string]any{"mode": "fetch", "requests": []any{map[string]any{"url": "https://site.example/private/a"}}}

	scraper := pspec{kind: "scraper", network: []string{"https://site.example"}}.build(t)
	rows := fetchReplies(t, mustRun(t, s, scraper, in, nil))
	if errCode(rows[0]) != "robots_disallowed" {
		t.Fatalf("scraper: %v", rows[0])
	}
	provider := pspec{kind: "provider", network: []string{"https://site.example"}}.build(t)
	rows = fetchReplies(t, mustRun(t, s, provider, in, nil))
	if rows[0]["status"] != float64(200) {
		t.Fatalf("provider: %v", rows[0])
	}
}

func TestFetchWithoutAClientIsRefused(t *testing.T) {
	s := newSup(t, Options{})
	p := pspec{network: []string{"https://api.example.com"}}.build(t)
	rows := fetchReplies(t, mustRun(t, s, p, map[string]any{"mode": "fetch", "requests": []any{map[string]any{"url": "https://api.example.com/"}}}, nil))
	if errCode(rows[0]) != "network_unavailable" {
		t.Fatalf("%v", rows[0])
	}
}

func TestConcurrentFetchesAreCorrelatedById(t *testing.T) {
	s := newSup(t, Options{Client: testClient(t, okTransport())})
	p := pspec{network: []string{"https://api.example.com"}, maxPages: 20}.build(t)
	out := mustRun(t, s, p, map[string]any{"mode": "fetch_concurrent", "n": 12, "url": "https://api.example.com/x"}, nil)
	f := fieldsOf(t, out)
	ids := f["ids"].([]any)
	if len(ids) != 12 {
		t.Fatalf("ids: %v", ids)
	}
	for i, id := range ids {
		if id != float64(i+1) {
			t.Fatalf("ids: %v", ids)
		}
	}
}

func TestSecretsAreRedactedFromFetchEvidence(t *testing.T) {
	s := newSup(t, Options{Client: testClient(t, okTransport())})
	const secret = "sk-evidence-secret-9"
	p := pspec{network: []string{"https://api.example.com"}, secrets: []string{"API_KEY"}}.build(t)
	out := mustRun(t, s, p, map[string]any{"mode": "fetch", "requests": []any{
		map[string]any{"url": "https://api.example.com/lookup?api_key=" + secret},
	}}, map[string]string{"API_KEY": secret})
	for _, ev := range out.Records[0].Evidence["fetches"].([]FetchEvidence) {
		if strings.Contains(ev.URL, secret) || !strings.Contains(ev.URL, "REDACTED") {
			t.Fatalf("evidence URL: %s", ev.URL)
		}
	}
}
