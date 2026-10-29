package leadweb

import (
	"context"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/browserclient"
)

// Fetch facade behaviour (legacy leadgen/http.py StealthClient.fetch); the
// engines are the browser service's (tests/test_browser_tier_escalation.py
// covered the same paths in Python).

var realPage = "<html><body>" + strings.Repeat("<p>Acme Staffing, Pune. Call +91 98765 43210, hello@acme.in</p>", 20) + "</body></html>"

const cfWall = "<html><head><title>Just a moment...</title></head><body>cf-challenge</body></html>"

func engines(m map[string]func() (browserclient.RenderResult, error)) func(browserclient.RenderRequest) (browserclient.RenderResult, error) {
	return func(r browserclient.RenderRequest) (browserclient.RenderResult, error) {
		if f, ok := m[r.Engine]; ok {
			return f()
		}
		return browserclient.RenderResult{}, &browserclient.Error{Code: "engine_unavailable"}
	}
}

func page(status int, html string) func() (browserclient.RenderResult, error) {
	return func() (browserclient.RenderResult, error) {
		return browserclient.RenderResult{Status: status, HTML: html, FinalURL: "https://acme.in/final"}, nil
	}
}

func fail(code string) func() (browserclient.RenderResult, error) {
	return func() (browserclient.RenderResult, error) {
		return browserclient.RenderResult{}, &browserclient.Error{Code: code}
	}
}

func newTestFetcher(fb *fakeBrowser, proxyList string) *Fetcher {
	pool := NewProxyPool(proxyList)
	pool.choose = func(int) int { return 0 }
	return NewFetcher(fb, NewRateLimiter(), pool, "", nil)
}

func enginesUsed(fb *fakeBrowser) []string {
	var out []string
	for _, r := range fb.renders {
		out = append(out, r.Engine)
	}
	return out
}

func TestFetchPlainPageStaysOnTier2(t *testing.T) {
	fb := &fakeBrowser{render: engines(map[string]func() (browserclient.RenderResult, error){"impersonate": page(200, realPage)})}
	f := newTestFetcher(fb, "1.1.1.1:8080")
	res := f.Fetch(context.Background(), "https://acme.in/", FetchOptions{})
	if !res.OK() || res.TierUsed != 2 || res.URL != "https://acme.in/" || res.ProxyUsed != "http://1.1.1.1:8080" {
		t.Fatalf("%+v", res)
	}
	if got := enginesUsed(fb); len(got) != 1 || fb.renders[0].UpstreamProxy != "http://1.1.1.1:8080" ||
		!fb.renders[0].IncludeHTML || fb.renders[0].Timeout.Seconds() != 15 {
		t.Fatalf("%v %+v", got, fb.renders[0])
	}
	if res.Emails()[0] != "hello@acme.in" || res.Phones()[0] != "+91 98765 43210" {
		t.Fatal(res.Emails(), res.Phones())
	}
	if st := f.pool.Stats(); st.DomainAssignments != 1 || st.Blocked != 0 {
		t.Fatalf("%+v", st)
	}
}

func TestFetchEscalatesChallengesToTheBrowser(t *testing.T) {
	fb := &fakeBrowser{render: engines(map[string]func() (browserclient.RenderResult, error){
		"impersonate": page(200, cfWall), "browser": page(200, realPage)})}
	f := newTestFetcher(fb, "1.1.1.1:8080,socks5-a.example:1080")
	res := f.Fetch(context.Background(), "https://acme.in/", FetchOptions{})
	if !res.OK() || res.TierUsed != 3 || res.Text != realPage {
		t.Fatalf("%+v", res)
	}
	if got := enginesUsed(fb); strings.Join(got, ",") != "impersonate,browser" {
		t.Fatal(got)
	}
	// tier 3 asks the pool for a socks5 proxy; the browser gets 10 s more
	if fb.renders[1].UpstreamProxy != "http://1.1.1.1:8080" || fb.renders[1].Timeout.Seconds() != 25 {
		t.Fatalf("%+v", fb.renders[1])
	}
}

func TestFetchKeepsTheHTTPBodyWhenTheBrowserFails(t *testing.T) {
	fb := &fakeBrowser{render: engines(map[string]func() (browserclient.RenderResult, error){
		"impersonate": page(200, cfWall), "browser": fail("navigation_failed")})}
	res := newTestFetcher(fb, "").Fetch(context.Background(), "https://acme.in/", FetchOptions{})
	if res.TierUsed != 2 || res.Text != cfWall {
		t.Fatalf("%+v", res)
	}
}

func TestFetchFallsBackToFlareSolverrWhenStillBlocked(t *testing.T) {
	fb := &fakeBrowser{render: engines(map[string]func() (browserclient.RenderResult, error){
		"impersonate": page(403, cfWall), "browser": page(403, cfWall), "flaresolverr": page(0, realPage)})}
	res := newTestFetcher(fb, "").Fetch(context.Background(), "https://acme.in/", FetchOptions{})
	if !res.OK() || res.TierUsed != 4 || res.StatusCode != 200 || res.URL != "https://acme.in/final" {
		t.Fatalf("%+v", res)
	}
	if got := enginesUsed(fb); strings.Join(got, ",") != "impersonate,browser,flaresolverr" {
		t.Fatal(got)
	}
}

func TestFetchWithoutFlareSolverrKeepsTheBlockedResultAndReportsFailure(t *testing.T) {
	fb := &fakeBrowser{render: engines(map[string]func() (browserclient.RenderResult, error){
		"impersonate": page(403, cfWall), "browser": page(403, cfWall)})}
	f := newTestFetcher(fb, "1.1.1.1:8080")
	res := f.Fetch(context.Background(), "https://acme.in/", FetchOptions{})
	// the failed browser answer is not adopted: legacy keeps the tier-2 body
	if res.OK() || res.StatusCode != 403 || res.TierUsed != 2 {
		t.Fatalf("%+v", res)
	}
	if st := f.pool.Stats(); st.Blocked != 1 {
		t.Fatalf("the proxy must go into cooldown: %+v", st)
	}
}

func TestFetchTier3GoesStraightToTheBrowser(t *testing.T) {
	fb := &fakeBrowser{render: engines(map[string]func() (browserclient.RenderResult, error){"browser": page(200, realPage)})}
	res := newTestFetcher(fb, "").Fetch(context.Background(), "https://acme.in/", FetchOptions{Tier: 3, NoProxy: true})
	if !res.OK() || res.TierUsed != 3 || strings.Join(enginesUsed(fb), ",") != "browser" || fb.renders[0].UpstreamProxy != "" {
		t.Fatalf("%+v %v", res, enginesUsed(fb))
	}
}

func TestFetchCircuitBreaker(t *testing.T) {
	fb := &fakeBrowser{render: engines(map[string]func() (browserclient.RenderResult, error){"impersonate": fail("navigation_failed")})}
	f := newTestFetcher(fb, "")
	for range breakerThreshold {
		f.limiter.ReportFailure("acme.in")
	}
	res := f.Fetch(context.Background(), "https://acme.in/", FetchOptions{})
	if res.Error != "Circuit breaker tripped for acme.in" || len(fb.renders) != 0 {
		t.Fatalf("%+v", res)
	}
}

func TestFetchRefusesUnsafeURLsWithoutCallingTheService(t *testing.T) {
	fb := &fakeBrowser{render: engines(nil)}
	f := newTestFetcher(fb, "")
	for _, u := range []string{"http://127.0.0.1/", "file:///etc/passwd", "http://169.254.169.254/", "http://localhost:8000/"} {
		if res := f.Fetch(context.Background(), u, FetchOptions{}); res.Error != "blocked url" {
			t.Errorf("%s: %+v", u, res)
		}
	}
	if len(fb.renders) != 0 {
		t.Fatal("service called")
	}
}

func TestFetchErrorsCarryOnlyTheCode(t *testing.T) {
	fb := &fakeBrowser{render: engines(map[string]func() (browserclient.RenderResult, error){"impersonate": fail("egress_blocked")})}
	res := newTestFetcher(fb, "").Fetch(context.Background(), "https://acme.in/", FetchOptions{})
	if res.Error != "egress_blocked" || res.OK() {
		t.Fatalf("%+v", res)
	}
}
