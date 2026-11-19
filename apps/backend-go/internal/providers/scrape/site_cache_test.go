package scrape

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadweb"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// siteRT serves one small contact page for "/" and 404 for every other path, counting requests.
type siteRT struct{ n atomic.Int64 }

func (r *siteRT) RoundTrip(req *http.Request) (*http.Response, error) {
	r.n.Add(1)
	h := http.Header{"Content-Type": {"text/html"}}
	status, body := 404, ""
	if req.URL.Path == "/" {
		status = 200
		body = `<html><head><meta name="description" content="We make things"></head><body>Contact <a href="mailto:hello@acme-site.example">mail</a> <a href="tel:+1 415 555 0123">call</a></body></html>`
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
}

// The workbook asks deep_scraper once per column (email, phone, size, description, ...) for the same lead. The
// provider returns every field it found on each call, so the pages are walked once and the other columns read the
// same result; each call still answers for its own field.
func TestDeepScraperWalksASiteOncePerRunNotOncePerColumn(t *testing.T) {
	Configure(Deps{})
	rt := &siteRT{}
	c := &http.Client{Transport: rt}
	lead := map[string]any{"website": "https://acme-site.example"}

	email, err := deepScraper{}.Enrich(context.Background(), c, nil, providers.Input{Lead: lead, Field: "email"})
	if err != nil || email.Value != "hello@acme-site.example" {
		t.Fatalf("email: %+v %v", email, err)
	}
	after1 := rt.n.Load()
	phone, err := deepScraper{}.Enrich(context.Background(), c, nil, providers.Input{Lead: lead, Field: "phone"})
	if err != nil || phone.Value == nil {
		t.Fatalf("phone: %+v %v", phone, err)
	}
	desc, _ := deepScraper{}.Enrich(context.Background(), c, nil, providers.Input{Lead: lead, Field: "description"})
	if desc.Value != "We make things" {
		t.Fatalf("description: %+v", desc)
	}
	if got := rt.n.Load(); got != after1 {
		t.Fatalf("the second and third column fetched the site again: %d requests after the first call, %d after all three", after1, got)
	}
}

// A site that yielded nothing is remembered too: the other columns do not walk 14 paths again to find nothing.
func TestDeepScraperRemembersAnEmptyWalk(t *testing.T) {
	Configure(Deps{})
	rt := &siteRT{}
	c := &http.Client{Transport: rt}
	lead := map[string]any{"website": "https://nothing-here.example/"}
	rt2 := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		rt.n.Add(1)
		return &http.Response{StatusCode: 404, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})}
	_ = c
	out, _ := deepScraper{}.Enrich(context.Background(), rt2, nil, providers.Input{Lead: lead, Field: "email"})
	if out.Error == "" {
		t.Fatalf("want a miss, got %+v", out)
	}
	n := rt.n.Load()
	out, _ = deepScraper{}.Enrich(context.Background(), rt2, nil, providers.Input{Lead: lead, Field: "phone"})
	if out.Error == "" || rt.n.Load() != n {
		t.Fatalf("second column walked the site again: %d -> %d requests, %+v", n, rt.n.Load(), out)
	}
}

// A walk cut short by the caller's context says nothing about the site and must not be remembered.
func TestDeepScraperDoesNotRememberACancelledWalk(t *testing.T) {
	Configure(Deps{})
	rt := &siteRT{}
	c := &http.Client{Transport: rt}
	lead := map[string]any{"website": "https://cancelled.example"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (deepScraper{}).Enrich(ctx, c, nil, providers.Input{Lead: lead, Field: "email"}); err == nil {
		t.Fatal("want the context error")
	}
	out, err := deepScraper{}.Enrich(context.Background(), c, nil, providers.Input{Lead: lead, Field: "email"})
	if err != nil || out.Value != "hello@acme-site.example" {
		t.Fatalf("a cancelled walk was remembered: %+v %v", out, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type countingFetch struct{ n atomic.Int64 }

func (f *countingFetch) Fetch(_ context.Context, u string, _ leadweb.FetchOptions) leadweb.FetchResult {
	f.n.Add(1)
	if strings.HasSuffix(u, "/contact") {
		return leadweb.FetchResult{Text: "Reach us: sales@acme-site.example or +1 415 555 0123", StatusCode: 200}
	}
	return leadweb.FetchResult{Text: "home", StatusCode: 200}
}

// website_scraper renders up to four pages through the browser service; asking it for email, then phone, must not
// render them twice.
func TestWebsiteScraperRendersASiteOncePerRunNotOncePerColumn(t *testing.T) {
	f := &countingFetch{}
	Configure(Deps{Fetch: f})
	lead := map[string]any{"website": "https://acme-site.example"}
	if _, err := (websiteScraper{}).Enrich(context.Background(), nil, nil, providers.Input{Lead: lead, Field: "email"}); err != nil {
		t.Fatal(err)
	}
	n := f.n.Load()
	if n == 0 {
		t.Fatal("no fetch happened")
	}
	if _, err := (websiteScraper{}).Enrich(context.Background(), nil, nil, providers.Input{Lead: lead, Field: "phone"}); err != nil {
		t.Fatal(err)
	}
	if got := f.n.Load(); got != n {
		t.Fatalf("second column rendered the site again: %d -> %d fetches", n, got)
	}
}
