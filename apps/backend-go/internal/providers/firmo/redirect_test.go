package firmo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

func redirect(status int, loc string) *http.Response {
	r := reply(status, "")
	r.StatusCode = status
	r.Header.Set("Location", loc)
	return r
}

// mixedClient serves the vendor hosts from fake and sends every other host
// through the production egress transport (which refuses loopback).
func mixedClient(fake roundTripFunc, vendorHosts ...string) *http.Client {
	guarded := egress.NewClientForEnv("production", nil).Transport
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		for _, h := range vendorHosts {
			if r.URL.Host == h {
				return fake(r)
			}
		}
		return guarded.RoundTrip(r)
	})}
}

func loopback(t *testing.T) (*httptest.Server, *atomic.Int32) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func enrichWith(t *testing.T, name string, c *http.Client, lead map[string]any) providers.Output {
	t.Helper()
	p, _ := providers.Get(name)
	out, err := p.Enrich(context.Background(), c, func() (string, error) { return "", nil }, providers.Input{Lead: lead})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return out
}

func TestGleifRedirects(t *testing.T) {
	fastLimiters(t)
	srv, hits := loopback(t)
	var seen []string
	var mu sync.Mutex
	followed := func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		seen = append(seen, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/fuzzycompletions":
			return redirect(302, "/api/v1/fuzzy2"), nil
		case "/api/v1/fuzzy2":
			return reply(200, `{"data":[{"relationships":{"lei-records":{"data":{"id":"L1"}}},"attributes":{"value":"Acme"}}]}`), nil
		}
		return reply(200, `{"data":{"id":"L1","attributes":{"entity":{"legalName":{"name":"ACME"}}}}}`), nil
	}
	out := enrichWith(t, "gleif", mixedClient(followed, "api.gleif.org"), map[string]any{"company": "Acme"})
	if out.Error != "" {
		t.Fatalf("redirect not followed: %q (seen %v)", out.Error, seen)
	}
	if !strings.Contains(strings.Join(seen, " "), "/api/v1/fuzzy2") {
		t.Errorf("redirect target never requested: %v", seen)
	}
	toLoop := func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/v1/fuzzycompletions" {
			return redirect(302, srv.URL+"/x"), nil
		}
		return reply(200, `{"data":[]}`), nil
	}
	out = enrichWith(t, "gleif", mixedClient(toLoop, "api.gleif.org"), map[string]any{"company": "Acme"})
	if out.Error != "no_lei_match" || hits.Load() != 0 {
		t.Errorf("redirect to loopback: error %q hits %d", out.Error, hits.Load())
	}
}

func TestSECRedirects(t *testing.T) {
	fastLimiters(t)
	srv, hits := loopback(t)
	var mu sync.Mutex
	var agents []string
	followed := func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		agents = append(agents, r.Header.Get("User-Agent"))
		mu.Unlock()
		switch r.URL.Path {
		case "/files/company_tickers.json":
			return redirect(301, "/files/tickers2.json"), nil
		case "/files/tickers2.json":
			return reply(200, `{"0":{"cik_str":1234,"title":"Acme Inc"}}`), nil
		}
		return reply(200, `{}`), nil
	}
	out := enrichWith(t, "sec_edgar", mixedClient(followed, "www.sec.gov", "data.sec.gov", "efts.sec.gov"), map[string]any{"company": "Acme Inc"})
	if v, _ := out.Extra.Get("sec_cik"); v != "0000001234" {
		t.Errorf("redirect not followed: cik %v err %q", v, out.Error)
	}
	for _, a := range agents {
		if a != secDefaultUA {
			t.Errorf("hop without the SEC User-Agent: %q", a)
		}
	}
	toLoop := func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/files/company_tickers.json" {
			return redirect(302, srv.URL+"/x"), nil
		}
		return reply(200, `{}`), nil
	}
	out = enrichWith(t, "sec_edgar", mixedClient(toLoop, "www.sec.gov", "data.sec.gov", "efts.sec.gov"), map[string]any{"company": "Acme Inc"})
	if out.Error != "no_cik_match" || hits.Load() != 0 {
		t.Errorf("redirect to loopback: error %q hits %d", out.Error, hits.Load())
	}
}

// company_tickers.json is about 1 MB: SEC raises the body cap to 4 MiB.
func TestSECTickersBodyAboveDefaultCap(t *testing.T) {
	fastLimiters(t)
	var b strings.Builder
	b.WriteString("{")
	for i := 0; i < 20000; i++ {
		b.WriteString(`"` + strings.Repeat("k", 5) + string(rune('a'+i%26)) + `":{"cik_str":9,"title":"Padding Company Number ` + strings.Repeat("x", 40) + `"},`)
	}
	b.WriteString(`"z":{"cik_str":1234,"title":"Acme Inc"}}`)
	if b.Len() <= providers.MaxBody || b.Len() > 4<<20 {
		t.Fatalf("fixture is %d bytes", b.Len())
	}
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/files/company_tickers.json" {
			return reply(200, b.String()), nil
		}
		return reply(200, `{}`), nil
	})
	out := enrichWith(t, "sec_edgar", &http.Client{Transport: rt}, map[string]any{"company": "Acme Inc"})
	if v, _ := out.Extra.Get("sec_cik"); v != "0000001234" {
		t.Errorf("tickers over 1 MiB not read: cik %v err %q", v, out.Error)
	}
}

func TestProvidersBodyCapOverride(t *testing.T) {
	big := strings.Repeat("a", providers.MaxBody+10)
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) { return reply(200, big), nil })
	c := &http.Client{Transport: rt}
	if _, err := providers.Do(context.Background(), c, providers.Req{Method: "GET", URL: "https://x.test/"}); err == nil {
		t.Error("default cap not enforced")
	}
	if r, err := providers.Do(context.Background(), c, providers.Req{Method: "GET", URL: "https://x.test/", MaxBody: 4 << 20}); err != nil || len(r.Body) != len(big) {
		t.Errorf("override: %v", err)
	}
	if _, err := providers.Do(context.Background(), c, providers.Req{Method: "GET", URL: "https://x.test/", MaxBody: 100}); err == nil ||
		err.Error() != "response body exceeds 100 bytes" {
		t.Errorf("small override: %v", err)
	}
}

func TestATSRedirects(t *testing.T) {
	srv, hits := loopback(t)
	followed := func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/v1/boards/acme/jobs":
			return redirect(307, "https://boards-api.greenhouse.io/v1/boards/acme/jobs2"), nil
		case "/v1/boards/acme/jobs2":
			return reply(200, `{"jobs":[{"title":"Backend Engineer"}]}`), nil
		}
		return reply(404, ""), nil
	}
	out := enrichWith(t, "ats_hiring", mixedClient(followed, "boards-api.greenhouse.io", "api.lever.co", "api.ashbyhq.com"), map[string]any{"company": "Acme"})
	if out.Error != "" {
		t.Fatalf("redirect not followed: %q", out.Error)
	}
	toLoop := func(r *http.Request) (*http.Response, error) { return redirect(302, srv.URL+"/x"), nil }
	out = enrichWith(t, "ats_hiring", mixedClient(toLoop, "boards-api.greenhouse.io", "api.lever.co", "api.ashbyhq.com"), map[string]any{"company": "Acme"})
	if out.Error != "no_board" || hits.Load() != 0 {
		t.Errorf("redirect to loopback: error %q hits %d", out.Error, hits.Load())
	}
}

// httpx returns a 3xx without Location as is (a non-200 miss), and gives up
// after 20 redirects (TooManyRedirects, swallowed by these providers).
func TestRedirectEdgeCasesMatchHTTPX(t *testing.T) {
	var n atomic.Int32
	loop := func(r *http.Request) (*http.Response, error) {
		n.Add(1)
		return redirect(302, "https://boards-api.greenhouse.io"+r.URL.Path), nil
	}
	out := enrichWith(t, "ats_hiring", mixedClient(loop, "boards-api.greenhouse.io", "api.lever.co", "api.ashbyhq.com"), map[string]any{"company": "Acme"})
	if out.Error != "no_board" {
		t.Errorf("loop: %q", out.Error)
	}
	if got := n.Load(); got != 3*21 { // 21 requests per board: first + 20 redirects
		t.Errorf("made %d requests, want %d", got, 3*21)
	}
	noLoc := func(*http.Request) (*http.Response, error) { return reply(302, ""), nil }
	out = enrichWith(t, "ats_hiring", mixedClient(noLoc, "boards-api.greenhouse.io", "api.lever.co", "api.ashbyhq.com"), map[string]any{"company": "Acme"})
	if out.Error != "no_board" {
		t.Errorf("no Location: %q", out.Error)
	}
}
