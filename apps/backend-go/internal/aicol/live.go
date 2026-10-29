package aicol

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/browserclient"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadweb"
)

// WebSearch is the research search over leadweb's resilient keyless search (legacy
// _ddg_search over get_ddgs): cached per workspace, bounded by DDG_SEARCH_TIMEOUT (25 s).
type WebSearch struct {
	Searcher  *leadweb.Searcher
	Workspace string
	Timeout   time.Duration
}

// DDGSearchTimeout reads DDG_SEARCH_TIMEOUT (seconds, default 25).
func DDGSearchTimeout() time.Duration {
	if n, err := strconv.Atoi(os.Getenv("DDG_SEARCH_TIMEOUT")); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return 25 * time.Second
}

// Search implements Searcher.
func (w WebSearch) Search(ctx context.Context, q string, n int) []Hit {
	if w.Searcher == nil {
		return nil
	}
	t := w.Timeout
	if t <= 0 {
		t = DDGSearchTimeout()
	}
	sctx, cancel := context.WithTimeout(ctx, t)
	defer cancel()
	hits := w.Searcher.Text(sctx, q, leadweb.SearchOptions{MaxResults: n, Workspace: w.Workspace})
	out := make([]Hit, len(hits))
	for i, h := range hits {
		out[i] = Hit{Title: h.Title, Href: h.Href, Body: h.Body}
	}
	return out
}

// Extractor is the browser service's extract op.
type Extractor interface {
	Extract(ctx context.Context, req browserclient.ExtractRequest) (browserclient.ExtractResult, error)
}

// WebPages is UniversalScraper.scrape for research: leadweb's fetch facade (egress check,
// per-domain rate limit, TLS-impersonating fetch escalating to a real browser on a
// challenge) and the browser service's markdown of the body (legacy _parse_html: scripts
// and embeds dropped, links and images stripped); the preview is its first 2000 characters.
type WebPages struct {
	Fetcher   *leadweb.Fetcher
	Extractor Extractor
}

const previewChars = 2000

// Fetch implements PageFetcher. Like the legacy scraper, whatever page the fetch returned
// (also an error status page) is read; a transport failure or a blocked URL is an error.
func (w WebPages) Fetch(ctx context.Context, u string) (string, error) {
	if w.Fetcher == nil || w.Extractor == nil {
		return "", errors.New("page fetching is not configured")
	}
	r := w.Fetcher.Fetch(ctx, u, leadweb.FetchOptions{})
	if r.Error != "" {
		return "", errors.New(r.Error)
	}
	if r.Text == "" {
		return "", fmt.Errorf("empty response (status %d)", r.StatusCode)
	}
	x, err := w.Extractor.Extract(ctx, browserclient.ExtractRequest{HTML: r.Text, URL: u, IncludeMarkdown: true,
		MaxBytes: 4 * previewChars, Timeout: 30 * time.Second})
	if err != nil {
		return "", err
	}
	return truncRunes(x.Markdown, previewChars), nil
}
