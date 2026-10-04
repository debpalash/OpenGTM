package declarative

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/debpalash/OpenGTM/apps/server/internal/egress"
	"github.com/debpalash/OpenGTM/apps/server/internal/kernels"
)

// fakeExtractor returns canned records per page. Extraction logic lives in
// the Rust kernel; the engine only needs records and the next link.
type fakeExtractor struct {
	mu    sync.Mutex
	pages map[string]fakePage // keyed by URL path+query
	calls []kernels.ExtractRequest
}

type fakePage struct {
	records []map[string]string
	next    string
}

func (f *fakeExtractor) Extract(_ context.Context, req kernels.ExtractRequest) (kernels.ExtractResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	key := req.BaseURL[strings.Index(req.BaseURL[8:], "/")+8:]
	page := f.pages[key]
	if _, nav := req.Fields["next"]; nav && len(req.Fields) == 1 {
		if page.next == "" {
			return kernels.ExtractResult{}, nil
		}
		return kernels.ExtractResult{Records: []map[string]string{{"next": page.next}}}, nil
	}
	return kernels.ExtractResult{Records: page.records}, nil
}

func scraperServer(t *testing.T, robots string) (*httptest.Server, *egress.Client) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			if robots == "" {
				http.NotFound(w, r)
				return
			}
			io.WriteString(w, robots)
			return
		}
		fmt.Fprintf(w, "<html>%s</html>", r.URL.RequestURI())
	}))
	t.Cleanup(srv.Close)
	client, err := egress.New(egress.Options{AllowPrivateForTesting: true, DefaultRPS: 1000, TLSConfig: srv.Client().Transport.(*http.Transport).TLSClientConfig})
	if err != nil {
		t.Fatal(err)
	}
	return srv, client
}

func scraperManifest(maxPages, limitPages int, extraNetwork string) string {
	return fmt.Sprintf(`manifest_version: "2"
name: team_page
kind: scraper
runtime: declarative
version: 0.1.0
capabilities: {network: ["https://127.0.0.1:*"%s]}
limits: {requests_per_second_per_domain: 100, max_pages: %d}
inputs: {domain: string}
outputs: person
scrape:
  start: "https://{{input.domain}}/team?page=1"
  items: "css:.member"
  fields: {full_name: "css:.name::text", title: "css:.role::text"}
  paginate: {next: "css:a.next::attr(href)", max_pages: %d}
`, extraNetwork, limitPages, maxPages)
}

func host(srv *httptest.Server) string { return strings.TrimPrefix(srv.URL, "https://") }

func TestScraperPaginatesWithEvidenceAndProgress(t *testing.T) {
	srv, client := scraperServer(t, "")
	ex := &fakeExtractor{pages: map[string]fakePage{
		"/team?page=1": {records: []map[string]string{{"full_name": "Ada", "title": "CEO"}, {"full_name": "Alan"}}, next: "/team?page=2"},
		"/team?page=2": {records: []map[string]string{{"full_name": "Grace", "title": "CTO"}}, next: srv.URL + "/team?page=3"},
		"/team?page=3": {records: []map[string]string{{"full_name": "Linus"}}},
	}}
	p := mustLoad(t, scraperManifest(5, 10, ""))
	var progress []Progress
	res, err := RunScraper(context.Background(), p, map[string]any{"domain": host(srv)}, ScraperDeps{
		Client: client, Extractor: ex, OnProgress: func(pr Progress) { progress = append(progress, pr) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Pages != 3 || len(res.Records) != 4 || res.Stopped != "" {
		t.Fatalf("result %+v", res)
	}
	if len(progress) != 3 || progress[2].Records != 4 || progress[2].Pages != 3 {
		t.Fatalf("progress %+v", progress)
	}
	ev := res.Records[2].Evidence
	sum := sha256.Sum256([]byte("<html>/team?page=2</html>"))
	if ev.URL != srv.URL+"/team?page=2" || ev.Page != 2 || ev.Items != "css:.member" || ev.Fields["title"] != "css:.role::text" || ev.BodySHA256 != hex.EncodeToString(sum[:]) || ev.FetchedAt.IsZero() {
		t.Fatalf("evidence %+v", ev)
	}
	first := ex.calls[0]
	if first.Items != "css:.member" || first.Format != "html" || first.BaseURL != srv.URL+"/team?page=1" || !strings.Contains(first.Document, "/team?page=1") {
		t.Fatalf("extract request %+v", first)
	}
}

func TestScraperLimitsLoopsAndCapability(t *testing.T) {
	srv, client := scraperServer(t, "")
	pages := map[string]fakePage{
		"/team?page=1": {records: []map[string]string{{"full_name": "A"}}, next: "/team?page=2"},
		"/team?page=2": {records: []map[string]string{{"full_name": "B"}}, next: "/team?page=1#again"},
	}
	run := func(manifestText string, pages map[string]fakePage) ScrapeResult {
		t.Helper()
		res, err := RunScraper(context.Background(), mustLoad(t, manifestText), map[string]any{"domain": host(srv)}, ScraperDeps{Client: client, Extractor: &fakeExtractor{pages: pages}})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := run(scraperManifest(5, 10, ""), pages); res.Pages != 2 || !strings.HasPrefix(res.Stopped, "pagination loop") {
		t.Fatalf("loop: %+v", res)
	}
	if res := run(scraperManifest(1, 10, ""), pages); res.Pages != 1 || len(res.Records) != 1 {
		t.Fatalf("paginate.max_pages: %+v", res)
	}
	if res := run(scraperManifest(5, 1, ""), pages); res.Pages != 1 {
		t.Fatalf("limits.max_pages: %+v", res)
	}
	away := map[string]fakePage{"/team?page=1": {records: []map[string]string{{"full_name": "A"}}, next: "https://elsewhere.example/team"}}
	if res := run(scraperManifest(5, 10, ""), away); res.Pages != 1 || !strings.Contains(res.Stopped, "outside capabilities.network") {
		t.Fatalf("capability: %+v", res)
	}
}

func TestScraperRobotsNoExtractorAndCancellation(t *testing.T) {
	srv, client := scraperServer(t, "User-agent: *\nDisallow: /team\n")
	p := mustLoad(t, scraperManifest(5, 10, ""))
	inputs := map[string]any{"domain": host(srv)}
	if _, err := RunScraper(context.Background(), p, inputs, ScraperDeps{Client: client, Extractor: &fakeExtractor{}}); err == nil || !strings.Contains(err.Error(), "robots.txt disallows") {
		t.Fatalf("robots: %v", err)
	}
	if _, err := RunScraper(context.Background(), p, inputs, ScraperDeps{Client: client}); !errors.Is(err, ErrNoExtractor) {
		t.Fatalf("no extractor: %v", err)
	}
	srv2, client2 := scraperServer(t, "")
	ex := &fakeExtractor{pages: map[string]fakePage{
		"/team?page=1": {records: []map[string]string{{"full_name": "A"}}, next: "/team?page=2"},
		"/team?page=2": {records: []map[string]string{{"full_name": "B"}}},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	res, err := RunScraper(ctx, p, map[string]any{"domain": host(srv2)}, ScraperDeps{Client: client2, Extractor: ex, OnProgress: func(Progress) { cancel() }})
	if !errors.Is(err, context.Canceled) || res.Pages != 1 || len(res.Records) != 1 {
		t.Fatalf("cancellation: %+v %v", res, err)
	}
	if _, err := RunScraper(context.Background(), p, map[string]any{}, ScraperDeps{Client: client2, Extractor: ex}); err == nil {
		t.Fatal("missing required input must fail")
	}
}
