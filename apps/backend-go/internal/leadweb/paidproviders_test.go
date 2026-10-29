package leadweb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/browserclient"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
)

const firecrawlGood = `{"success":true,"data":{"web":[{"title":"Acme","url":"https://acme.example/about","description":"Plain snippet"}]}}`

func paidConfig() config.LeadwebSettings {
	cfg, _ := config.LeadwebFrom(func(string) string { return "" })
	cfg.FirecrawlKey, cfg.SpicrawlKey = "fc-test-secret", "sp-test-secret"
	return cfg
}

func testSpicrawl(cfg config.LeadwebSettings, server *httptest.Server) *Spicrawl {
	s := NewSpicrawl(cfg, server.Client())
	s.endpoint = server.URL
	s.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}
	return s
}

func TestPaidProvidersAreOptInAndKeysStayRedacted(t *testing.T) {
	cfg, _ := config.LeadwebFrom(func(string) string { return "" })
	if NewSpicrawl(cfg, nil) != nil || len(NewKeyedEngines(cfg, nil, nil).Configured()) != 0 {
		t.Fatal("unconfigured providers must be inert")
	}
	cfg, err := config.LeadwebFrom(func(k string) string {
		return map[string]string{"FIRECRAWL_API_KEY": " fc-test-secret ", "SPICRAWL_API_KEY": " sp-test-secret "}[k]
	})
	if err != nil || cfg.FirecrawlKey.Reveal() != "fc-test-secret" || cfg.SpicrawlKey.Reveal() != "sp-test-secret" {
		t.Fatal("secret loading")
	}
	if got := NewKeyedEngines(cfg, nil, nil).Configured(); !slices.Equal(got, []string{"firecrawl"}) {
		t.Fatal(got)
	}
	raw, _ := json.Marshal(cfg)
	for _, text := range []string{string(raw), fmt.Sprintf("%+v %#v", cfg, cfg)} {
		if strings.Contains(text, "test-secret") {
			t.Fatal("key disclosed by formatting")
		}
	}
	for _, client := range []any{NewSpicrawl(cfg, nil), *NewSpicrawl(cfg, nil), NewKeyedEngines(cfg, nil, nil)} {
		if strings.Contains(fmt.Sprintf("%v %+v %#v %s %d", client, client, client, client, client), "test-secret") {
			t.Fatal("provider client formatting disclosed credentials")
		}
	}
}

func TestFirecrawlPOSTContractLimitsAndURLValidation(t *testing.T) {
	var calls int
	var requested int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "POST" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer fc-test-secret" ||
			r.Header.Get("Content-Type") != "application/json" {
			t.Error("incorrect request metadata")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		requested = int(body["limit"].(float64))
		if body["query"] != "site:acme.example pricing" || body["highlights"] != false || body["scrapeOptions"] != nil ||
			!reflect.DeepEqual(body["sources"], []any{map[string]any{"type": "web"}}) {
			t.Error("unexpected request body")
		}
		payload, _ := json.Marshal(body)
		if strings.Contains(string(payload), "fc-test-secret") {
			t.Error("key in body")
		}
		items := []any{nil, "invalid", map[string]any{"url": 123}}
		for _, u := range []string{"javascript:alert(1)", "ftp://acme.example", "//acme.example", "https://",
			"https://user:password@acme.example", "http://127.0.0.1", "http://169.254.169.254/latest", "http://[::1]",
			"https://acme.example\\@evil.example", "https://acme.example/\nheader"} {
			items = append(items, map[string]any{"url": u})
		}
		for range 110 {
			items = append(items, map[string]any{"title": "Acme", "url": "https://acme.example/about", "description": "Plain snippet"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"web": items}})
	}))
	defer srv.Close()
	e := NewKeyedEngines(paidConfig(), srv.Client(), nil)
	e.Endpoints = map[string]string{"firecrawl": srv.URL}
	for _, n := range []int{1, 100, 500} {
		hits := e.Search(context.Background(), "site:acme.example pricing", n)
		if len(hits) != min(n, 100) || requested != min(n, 100) || hits[0].Body != "Plain snippet" {
			t.Fatal("limit/shape mismatch")
		}
	}
	for _, query := range []string{"", "  ", strings.Repeat("x", 501), string([]byte{0xff})} {
		if len(e.Search(context.Background(), query, 5)) != 0 {
			t.Fatal("invalid query accepted")
		}
	}
	if len(e.Search(context.Background(), "q", 0)) != 0 || len(e.Search(context.Background(), "q", -1)) != 0 || calls != 3 {
		t.Fatal("unexpected billed request")
	}
}

func TestFirecrawlRejectsFailedMalformedAndOversizedResponses(t *testing.T) {
	for _, body := range []string{`{"success":false,"data":{"web":[{"url":"https://a.example"}]}}`,
		`{"data":{"web":[{"url":"https://a.example"}]}}`, `null`, `[]`, `{"success":true,"data":[]}`,
		`{"success":true,"data":{"web":"wrong"}}`, firecrawlGood + `{}`, firecrawlGood + strings.Repeat(" ", firecrawlMaxBody), "not JSON"} {
		t.Run(fmt.Sprint(len(body), "-", body[:min(len(body), 30)]), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
			defer srv.Close()
			e := NewKeyedEngines(paidConfig(), srv.Client(), nil)
			e.Endpoints = map[string]string{"firecrawl": srv.URL}
			if hits := e.Search(context.Background(), "q", 5); len(hits) != 0 {
				t.Fatal("untrusted result accepted")
			}
		})
	}
}

func TestPaidProviderRedirectsNeverReplayCredentialsOrBodies(t *testing.T) {
	var leaked atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1); _, _ = io.WriteString(w, firecrawlGood) }))
	defer target.Close()
	for _, status := range []int{301, 302, 303, 307, 308} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, status) }))
		e := NewKeyedEngines(paidConfig(), srv.Client(), nil)
		e.Endpoints = map[string]string{"firecrawl": srv.URL}
		if len(e.Search(context.Background(), "secret query", 5)) != 0 {
			t.Fatal("redirect followed")
		}
		if _, err := testSpicrawl(paidConfig(), srv).Fetch(context.Background(), "https://acme.example"); err == nil {
			t.Fatal("redirect followed")
		}
		srv.Close()
	}
	if leaked.Load() != 0 {
		t.Fatal("request replayed to redirect target")
	}
}

func TestPaidProviderCancellationAndSanitizedErrors(t *testing.T) {
	var calls atomic.Int32
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, fmt.Errorf("secret upstream error: %s %s", r.Header.Get("Authorization"), r.URL)
	})}
	e := NewKeyedEngines(paidConfig(), hc, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if len(e.Search(ctx, "secret query", 5)) != 0 || calls.Load() != 0 {
		t.Fatal("cancelled request sent")
	}
	if len(e.Search(context.Background(), "secret query", 5)) != 0 {
		t.Fatal("transport failure accepted")
	}
	_, _, err := postProvider(context.Background(), hc, providerRequest{endpoint: firecrawlEndpoint, key: "secret", body: map[string]string{"query": "private"}, maxBytes: 128})
	if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "private") {
		t.Fatal("upstream error disclosed")
	}
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer srv.Close()
	cfg := paidConfig()
	cfg.EngineTimeout = 30 * time.Millisecond
	e = NewKeyedEngines(cfg, srv.Client(), nil)
	e.Endpoints = map[string]string{"firecrawl": srv.URL}
	t0 := time.Now()
	if len(e.Search(context.Background(), "q", 5)) != 0 || time.Since(t0) > 500*time.Millisecond {
		t.Fatal("deadline not enforced")
	}
	<-started
}

func TestFirecrawlFallbackCacheAndWorkspaceIsolation(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); _, _ = io.WriteString(w, firecrawlGood) }))
	defer srv.Close()
	cfg := paidConfig()
	browser := &fakeBrowser{}
	s := newTestSearcher(t, browser, cfg, nil, nil)
	s.keyed.hc = srv.Client()
	s.keyed.Endpoints = map[string]string{"firecrawl": srv.URL}
	for _, ws := range []string{"alpha", "alpha", "beta"} {
		if hits := s.Text(context.Background(), "q", SearchOptions{Workspace: ws, NoProxy: true}); len(hits) != 1 {
			t.Fatal(hits)
		}
	}
	if calls.Load() != 2 || len(browser.searches) != 2 {
		t.Fatal("cache crossed workspace or failed to cache")
	}
	if len(s.News(context.Background(), "q", SearchOptions{Workspace: "alpha", NoProxy: true})) != 0 || calls.Load() != 2 {
		t.Fatal("news invoked paid fallback")
	}
	first := &fakeBrowser{script: []browserclient.SearchOut{step("hit")}}
	s = newTestSearcher(t, first, cfg, nil, nil)
	s.keyed.hc = srv.Client()
	s.keyed.Endpoints = map[string]string{"firecrawl": srv.URL}
	if len(s.Text(context.Background(), "q", SearchOptions{NoProxy: true})) != 1 || calls.Load() != 2 {
		t.Fatal("paid request before keyless results")
	}
}

func TestSpicrawlMarkdownPDFOptionsAndCostCeiling(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer sp-test-secret" {
			t.Error("request metadata")
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("X-Target-Status", "200")
		w.Header().Set("X-Credits-Charged", "1")
		_, _ = io.WriteString(w, "# Article\n\n"+strings.Repeat("é", 2100))
	}))
	defer srv.Close()
	s := testSpicrawl(paidConfig(), srv)
	r, err := s.Scrape(context.Background(), "https://acme.example/report.pdf#private-token", SpicrawlOptions{IncludeTags: []string{"article"}, ExcludeTags: []string{"aside"}})
	if err != nil || r.CreditsCharged != 1 || !strings.HasPrefix(r.Markdown, "# Article") {
		t.Fatal("valid markdown/PDF response rejected", err)
	}
	if body["url"] != "https://acme.example/report.pdf" || body["response_format"] != "markdown" || body["main_content_only"] != true ||
		body["parse_pdf"] != true || body["links"] != false || body["js_render"] != false || body["max_cost"] != float64(1) {
		t.Fatal(body)
	}
	if !reflect.DeepEqual(body["include_tags"], []any{"article"}) || !reflect.DeepEqual(body["exclude_tags"], []any{"aside"}) {
		t.Fatal(body)
	}
	text, err := s.Fetch(context.Background(), "https://acme.example")
	if err != nil || len([]rune(text)) != 2000 {
		t.Fatal("research preview not bounded", err)
	}
	_, err = s.Scrape(context.Background(), "https://acme.example", SpicrawlOptions{JSRender: true})
	if err != nil || body["max_cost"] != float64(3) || body["js_render"] != true {
		t.Fatal("explicit JS request", err)
	}
}

func TestSpicrawlRejectsFailedTargetsAndInvalidContent(t *testing.T) {
	for _, c := range []struct {
		target, credits, kind, body string
		status                      int
	}{
		{"403", "0", "text/markdown", "Forbidden", 200}, {"404", "1", "text/markdown", "Not found", 200},
		{"", "1", "text/markdown", "content", 200}, {"200", "", "text/markdown", "content", 200},
		{"200", "2", "text/markdown", "content", 200}, {"200", "1", "application/json", `{"content":"article"}`, 200},
		{"200", "1", "text/html", "<p>raw HTML</p>", 200}, {"200", "1", "text/markdown", " ", 200},
		{"200", "1", "text/markdown", string([]byte{0xff}), 200}, {"200", "1", "text/markdown", strings.Repeat("x", spicrawlMaxBody+1), 200},
		{"200", "1", "text/markdown", "content", 401},
	} {
		t.Run(fmt.Sprintf("%s-%s-%s-%d", c.target, c.credits, c.kind, len(c.body)), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-Target-Status", c.target)
				w.Header().Set("X-Credits-Charged", c.credits)
				w.Header().Set("Content-Type", c.kind)
				w.WriteHeader(c.status)
				_, _ = io.WriteString(w, c.body)
			}))
			defer srv.Close()
			if _, err := testSpicrawl(paidConfig(), srv).Fetch(context.Background(), "https://acme.example"); err == nil {
				t.Fatal("invalid content accepted")
			}
		})
	}
}

func TestSpicrawlRejectsPrivateTargetsBeforeSendingURL(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer srv.Close()
	s := testSpicrawl(paidConfig(), srv)
	for _, u := range []string{"file:///etc/passwd", "http://localhost", "http://169.254.169.254/latest", "http://2130706433",
		"http://[::ffff:127.0.0.1]", "https://user:pass@acme.example", "https://acme.example\\@127.0.0.1", "https://acme.example/\nheader"} {
		if _, err := s.Fetch(context.Background(), u); !errors.Is(err, errSpicrawlURL) {
			t.Fatal("private URL accepted")
		}
	}
	s.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("10.0.0.1")}, nil
	}
	if _, err := s.Fetch(context.Background(), "https://acme.example"); !errors.Is(err, errSpicrawlURL) {
		t.Fatal("mixed private DNS accepted")
	}
	if requests.Load() != 0 {
		t.Fatal("private target disclosed to provider")
	}
}
