// Package unisearch is the Go port of the academic/document source registry behind GET /api/v2/search/unified
// (legacy apps/api/sources, plan 15 WP-ROUTES-B): arXiv, Crossref, DOAJ, Project Gutenberg, OpenLibrary, PubMed
// Central, Semantic Scholar, Internet Archive, Google Books and (when keyed) Google Custom Search, searched
// concurrently and merged, deduplicated, filtered and sorted.
//
// Every request goes through an SSRF-guarded client (egress.NewClientForEnv in production). Source failures never
// surface: a source that errors contributes no results, exactly as in Python.
package unisearch

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// File types (apps/api/sources FileType).
const (
	TypePDF   = "pdf"
	TypeDOCX  = "docx"
	TypeEPUB  = "epub"
	TypeMOBI  = "mobi"
	TypeTXT   = "txt"
	TypeHTML  = "html"
	TypeOther = "other"
)

// Result is the standardized search result (SearchResult.model_dump(), field order included).
type Result struct {
	ID          string  `json:"id"`
	Title       string  `json:"title"`
	URL         string  `json:"url"`
	Source      string  `json:"source"`
	DownloadURL *string `json:"download_url"`
	Thumbnail   *string `json:"thumbnail"`
	Author      *string `json:"author"`
	Year        *int    `json:"year"`
	FileType    string  `json:"file_type"`
	FileSize    *string `json:"file_size"`
	Snippet     *string `json:"snippet"`
}

// Source is one document source.
type Source interface {
	Name() string
	DisplayName() string
	Available() bool
	// Search returns at most the source's own notion of limit results; it never fails (errors give nil).
	Search(ctx context.Context, query string, limit int) []Result
}

// Unified is the merged answer.
type Unified struct {
	Query   string
	Total   int
	Results []Result
}

// Registry holds the sources in registration order (the order results are flattened in).
type Registry struct {
	sources []Source
	// Timeout is the global deadline of one unified search (Python: 8 s).
	Timeout time.Duration
}

// NewRegistryOf builds a registry over the given sources (tests, embedders).
func NewRegistryOf(sources ...Source) *Registry {
	return &Registry{sources: sources, Timeout: 8 * time.Second}
}

// Names lists the registered source names in order.
func (r *Registry) Names() []string {
	out := make([]string, len(r.sources))
	for i, s := range r.sources {
		out[i] = s.Name()
	}
	return out
}

// SearchUnified is SourceRegistry.search_unified: search the named sources (nil = all) concurrently, flatten in
// registry order, keep file_type matches, drop duplicates (normalized title plus the first ten normalized author
// characters), then sort ("date": year desc, "title": case-folded title, anything else keeps source order).
//
// Deviations from Python (fixes): file_type filters instead of raising (use_enum_values stores a plain string, so
// Python's result.file_type.value was an AttributeError on any request carrying file_type); sources that answer
// within the global deadline are kept when another is slow (Python dropped everything on a timeout).
func (r *Registry) SearchUnified(ctx context.Context, query string, names []string, limitPerSource int, sortBy, fileType string) Unified {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	var picked []Source
	for _, s := range r.sources {
		if (len(names) == 0 || want[s.Name()]) && s.Available() {
			picked = append(picked, s)
		}
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out := make([][]Result, len(picked))
	done := make([]chan struct{}, len(picked))
	var wg sync.WaitGroup
	for i, s := range picked {
		done[i] = make(chan struct{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(done[i])
			defer func() { _ = recover() }()
			res := s.Search(cctx, query, limitPerSource)
			select {
			case <-cctx.Done():
				// finished after the deadline: not part of the answer
			default:
				out[i] = res
			}
		}()
	}
	allDone := make(chan struct{})
	go func() { wg.Wait(); close(allDone) }()
	select {
	case <-allDone:
	case <-cctx.Done():
		// sources honour the cancelled context; give stragglers a moment, never block the answer on them
		select {
		case <-allDone:
		case <-time.After(50 * time.Millisecond):
		}
	}

	var all []Result
	for i := range picked {
		select {
		case <-done[i]:
		default:
			continue // still running past the deadline: its slot may be written later, never read it
		}
		for _, res := range out[i] {
			if fileType != "" && res.FileType != fileType {
				continue
			}
			all = append(all, res)
		}
	}

	seen := map[string]bool{}
	var uniq []Result
	for _, res := range all {
		key := normalize(res.Title)
		if res.Author != nil && *res.Author != "" {
			key += firstRunes(normalize(*res.Author), 10)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		uniq = append(uniq, res)
	}

	switch sortBy {
	case "date":
		sort.SliceStable(uniq, func(i, j int) bool { return yearOf(uniq[i]) > yearOf(uniq[j]) })
	case "title":
		sort.SliceStable(uniq, func(i, j int) bool { return strings.ToLower(uniq[i].Title) < strings.ToLower(uniq[j].Title) })
	}
	return Unified{Query: query, Total: len(uniq), Results: uniq}
}

func yearOf(r Result) int {
	if r.Year == nil {
		return 0
	}
	return *r.Year
}

// normalize keeps the lower-cased letters and digits (Python: c.lower() for c in text if c.isalnum()).
func normalize(s string) string {
	var b strings.Builder
	for _, c := range s {
		if unicode.IsLetter(c) || unicode.IsNumber(c) {
			b.WriteRune(unicode.ToLower(c))
		}
	}
	return b.String()
}

func firstRunes(s string, n int) string {
	i := 0
	for j := range s {
		if i == n {
			return s[:j]
		}
		i++
	}
	return s
}

// ── shared HTTP ─────────────────────────────────────────────────────────────

const (
	maxBody      = 8 << 20
	maxRedirects = 5
	defaultUA    = "OpenGTM/3.0 (+https://github.com/debpalash/OpenGTM)"
)

// getter fetches public APIs through the guarded client. It follows redirects itself (the egress client never
// does) so every hop is dialled through the guard, as aiohttp followed them in Python.
type getter struct {
	hc      *http.Client
	timeout time.Duration
}

type httpAnswer struct {
	Status  int
	Body    []byte
	Headers http.Header
}

// get returns ok=false on any transport problem (the caller treats that as "no results").
func (g getter) get(ctx context.Context, rawURL string, hdr map[string]string) (httpAnswer, bool) {
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	cur := rawURL
	for hop := 0; hop <= maxRedirects; hop++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, cur, nil)
		if err != nil {
			return httpAnswer{}, false
		}
		req.Header.Set("User-Agent", defaultUA)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := g.hc.Do(req)
		if err != nil {
			return httpAnswer{}, false
		}
		switch resp.StatusCode {
		case 301, 302, 303, 307, 308:
			loc := resp.Header.Get("Location")
			_ = resp.Body.Close()
			base, perr := url.Parse(cur)
			next, nerr := url.Parse(loc)
			if loc == "" || perr != nil || nerr != nil {
				return httpAnswer{}, false
			}
			cur = base.ResolveReference(next).String()
			continue
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		_ = resp.Body.Close()
		if err != nil {
			return httpAnswer{}, false
		}
		return httpAnswer{Status: resp.StatusCode, Body: b, Headers: resp.Header}, true
	}
	return httpAnswer{}, false
}

func sp(s string) *string { return &s }
