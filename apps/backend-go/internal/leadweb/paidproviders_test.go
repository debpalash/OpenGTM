package leadweb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
	cfg.FirecrawlKey = "fc-test-secret"
	return cfg
}

func TestPaidProvidersAreOptInAndKeysStayRedacted(t *testing.T) {
	cfg, _ := config.LeadwebFrom(func(string) string { return "" })
	if len(NewKeyedEngines(cfg, nil, nil).Configured()) != 0 {
		t.Fatal("unconfigured providers must be inert")
	}
	cfg, err := config.LeadwebFrom(func(k string) string {
		return map[string]string{"FIRECRAWL_API_KEY": " fc-test-secret "}[k]
	})
	if err != nil || cfg.FirecrawlKey.Reveal() != "fc-test-secret" {
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
	for _, client := range []any{NewKeyedEngines(cfg, nil, nil)} {
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
