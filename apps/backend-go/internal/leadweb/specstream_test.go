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

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/browserclient"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
)

const specstreamGood = `{"request_id":"req_1","sources":[{"title":"Acme","url":"https://acme.example/about","snippet":"Plain snippet"}]}`

func specstreamConfig() config.LeadwebSettings {
	cfg, _ := config.LeadwebFrom(func(string) string { return "" })
	cfg.SpecStreamKey = "ss-test-secret"
	return cfg
}

func TestSpecStreamIsOptInAndKeyStaysRedacted(t *testing.T) {
	cfg, err := config.LeadwebFrom(func(k string) string {
		return map[string]string{"SPECSTREAM_API_KEY": " ss-test-secret "}[k]
	})
	if err != nil || cfg.SpecStreamKey.Reveal() != "ss-test-secret" {
		t.Fatal("secret loading")
	}
	if got := NewKeyedEngines(cfg, nil, nil).Configured(); !slices.Equal(got, []string{"specstream"}) {
		t.Fatal(got)
	}
	cfg.FirecrawlKey = "fc-test-secret"
	if got := NewKeyedEngines(cfg, nil, nil).Configured(); !slices.Equal(got, []string{"firecrawl", "specstream"}) {
		t.Fatal("default order", got)
	}
	raw, _ := json.Marshal(cfg)
	e := NewKeyedEngines(cfg, nil, nil)
	for _, text := range []string{string(raw), fmt.Sprintf("%+v %#v", cfg, cfg), fmt.Sprintf("%v %+v %#v", e, e, e)} {
		if strings.Contains(text, "ss-test-secret") {
			t.Fatal("key disclosed by formatting")
		}
	}
}

func TestSpecStreamGETContractLimitsAndURLValidation(t *testing.T) {
	var calls, requested int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, _ := io.ReadAll(r.Body)
		if r.Method != "GET" || len(body) != 0 || r.Header.Get("Authorization") != "Bearer ss-test-secret" ||
			r.Header.Get("Content-Type") != "" || r.Header.Get("Accept") != "application/json" {
			t.Error("incorrect request metadata")
		}
		q := r.URL.Query()
		if q.Get("q") != "site:acme.example pricing" || q.Get("speed") != "fast" || len(q) != 3 {
			t.Error("unexpected query parameters", q)
		}
		if strings.Contains(r.URL.RawQuery, "ss-test-secret") {
			t.Error("key in URL")
		}
		fmt.Sscan(q.Get("num"), &requested)
		items := []any{nil, "invalid", map[string]any{"url": 123}}
		for _, u := range []string{"javascript:alert(1)", "ftp://acme.example", "//acme.example", "https://",
			"https://user:password@acme.example", "http://127.0.0.1", "http://169.254.169.254/latest", "http://[::1]",
			"https://acme.example\\@evil.example", "https://acme.example/\nheader"} {
			items = append(items, map[string]any{"url": u})
		}
		for range 110 {
			items = append(items, map[string]any{"title": "Acme", "url": "https://acme.example/about", "snippet": "Plain snippet"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"request_id": "req_1", "sources": items})
	}))
	defer srv.Close()
	e := NewKeyedEngines(specstreamConfig(), srv.Client(), nil)
	e.Endpoints = map[string]string{"specstream": srv.URL}
	for _, n := range []int{1, 100, 500} {
		hits := e.Search(context.Background(), "site:acme.example pricing", n)
		if len(hits) != min(n, 100) || requested != min(n, 100) || hits[0].Body != "Plain snippet" {
			t.Fatal("limit/shape mismatch")
		}
	}
	for _, query := range []string{"", "  ", strings.Repeat("x", 2001), string([]byte{0xff})} {
		if len(e.Search(context.Background(), query, 5)) != 0 {
			t.Fatal("invalid query accepted")
		}
	}
	if len(e.Search(context.Background(), "q", 0)) != 0 || len(e.Search(context.Background(), "q", -1)) != 0 || calls != 3 {
		t.Fatal("unexpected billed request")
	}
}

// Golden fixture: a response in the documented /v1/web-search shape and the
// hits it must map to.
func TestSpecStreamGoldenFixture(t *testing.T) {
	var g struct {
		Query      string          `json:"query"`
		MaxResults int             `json:"max_results"`
		Response   json.RawMessage `json:"response"`
		Want       []Hit           `json:"want"`
	}
	loadJSON(t, "specstream_search.json", &g)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") != g.Query {
			t.Error("query not passed through")
		}
		_, _ = w.Write(g.Response)
	}))
	defer srv.Close()
	e := NewKeyedEngines(specstreamConfig(), srv.Client(), nil)
	e.Endpoints = map[string]string{"specstream": srv.URL}
	if got := e.Search(context.Background(), g.Query, g.MaxResults); !reflect.DeepEqual(got, g.Want) {
		t.Fatalf("got %+v\nwant %+v", got, g.Want)
	}
}

func TestSpecStreamRejectsFailedMalformedAndOversizedResponses(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
	}{
		{200, `{"sources":[{"url":"https://a.example"}]}`}, {200, `null`}, {200, `[]`},
		{200, `{"request_id":"r","sources":"wrong"}`}, {200, specstreamGood + `{}`},
		{200, specstreamGood + strings.Repeat(" ", specstreamMaxBody)}, {200, "not JSON"},
		{401, specstreamGood}, {402, specstreamGood}, {429, specstreamGood}, {500, specstreamGood},
	} {
		t.Run(fmt.Sprint(c.status, "-", len(c.body), "-", c.body[:min(len(c.body), 30)]), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(c.status)
				_, _ = io.WriteString(w, c.body)
			}))
			defer srv.Close()
			e := NewKeyedEngines(specstreamConfig(), srv.Client(), nil)
			e.Endpoints = map[string]string{"specstream": srv.URL}
			if hits := e.Search(context.Background(), "q", 5); len(hits) != 0 {
				t.Fatal("untrusted result accepted")
			}
		})
	}
}

func TestSpecStreamRedirectsNeverReplayCredentials(t *testing.T) {
	var leaked atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1); _, _ = io.WriteString(w, specstreamGood) }))
	defer target.Close()
	for _, status := range []int{301, 302, 303, 307, 308} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, status) }))
		e := NewKeyedEngines(specstreamConfig(), srv.Client(), nil)
		e.Endpoints = map[string]string{"specstream": srv.URL}
		if len(e.Search(context.Background(), "secret query", 5)) != 0 {
			t.Fatal("redirect followed")
		}
		srv.Close()
	}
	if leaked.Load() != 0 {
		t.Fatal("request replayed to redirect target")
	}
}

func TestSpecStreamCancellationAndSanitizedErrors(t *testing.T) {
	var calls atomic.Int32
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, fmt.Errorf("secret upstream error: %s %s", r.Header.Get("Authorization"), r.URL)
	})}
	e := NewKeyedEngines(specstreamConfig(), hc, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if len(e.Search(ctx, "secret query", 5)) != 0 || calls.Load() != 0 {
		t.Fatal("cancelled request sent")
	}
	if len(e.Search(context.Background(), "secret query", 5)) != 0 {
		t.Fatal("transport failure accepted")
	}
	_, _, err := postProvider(context.Background(), hc, providerRequest{endpoint: specstreamEndpoint, key: "secret", query: "q=private", maxBytes: 128})
	if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "private") {
		t.Fatal("upstream error disclosed")
	}
}

func TestSpecStreamFallbackCacheAndWorkspaceIsolation(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); _, _ = io.WriteString(w, specstreamGood) }))
	defer srv.Close()
	cfg := specstreamConfig()
	browser := &fakeBrowser{}
	s := newTestSearcher(t, browser, cfg, nil, nil)
	s.keyed.hc = srv.Client()
	s.keyed.Endpoints = map[string]string{"specstream": srv.URL}
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
	s.keyed.Endpoints = map[string]string{"specstream": srv.URL}
	if len(s.Text(context.Background(), "q", SearchOptions{NoProxy: true})) != 1 || calls.Load() != 2 {
		t.Fatal("paid request before keyless results")
	}
}
