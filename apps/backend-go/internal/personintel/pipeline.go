package personintel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/browserclient"
)

// Renderer renders a JavaScript page: the browser service's /v1/render (*browserclient.Client).
type Renderer interface {
	Render(ctx context.Context, req browserclient.RenderRequest) (browserclient.RenderResult, error)
}

// Deps are the collaborators of Enrich.
type Deps struct {
	// HTTP is the caller's egress-guarded client: every search and GitHub
	// request goes through it. Required.
	HTTP *http.Client
	// Browser renders the result pages a plain fetch cannot read (a JavaScript
	// wall). nil, a nil or an unconfigured *browserclient.Client skips those pages.
	Browser Renderer
	// Fetch loads one result page for the deep scrape. nil means the
	// egress-guarded HTTP client above (2 MiB cap, same-host redirects only).
	// Tests inject their own.
	Fetch PageFetcher
}

const (
	// DeepMaxPages bounds the pages handed to the deep scrape.
	DeepMaxPages = 10
	// deepPageTimeout bounds one page call.
	deepPageTimeout = 20 * time.Second

	maxScrapedText  = 1000
	maxScrapedMails = 50
	maxRawSources   = 50
)

// deepBudget (whole deep phase) and deepDelay (between pages) are vars for tests.
var (
	deepBudget = 60 * time.Second
	deepDelay  = 500 * time.Millisecond
)

var deepSkipHosts = []string{"linkedin.com", "google.com", "duckduckgo.com"}

var fullEmailRe = regexp.MustCompile(`^` + EmailPattern + `$`)

// stepKeys is the payload key order of each step (Python dict insertion order).
var stepKeys = map[string][]string{
	"parsed":            {"username"},
	"searching":         {"message"},
	"search_progress":   {"category", "count"},
	"search_done":       {"results_count", "categories"},
	"extracting":        {"message"},
	"profile_extracted": {"name", "headline"},
	"socials_found":     {"social_links"},
	"github_api":        {"message"},
	"articles_found":    {"articles_count", "mentions_count"},
	"deep_scraping":     {"message", "total"},
	"deep_unavailable":  {"message"},
	"scrape_progress":   {"url", "progress"},
	"emails_found":      {"email_count"},
	"completed":         {"profile"},
	"saved":             {"id", "profile"},
	"error":             {"message"},
}

// EventJSON renders a progress message the way Python's json.dumps({"step":
// step, **data}) orders it (step first, then the step's keys in Python order,
// any other keys sorted), without HTML escaping. The WebSocket route sends
// exactly these bytes.
func EventJSON(step string, data map[string]any) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(`{"step":`)
	sb, err := encodeNoEscape(step)
	if err != nil {
		return nil, err
	}
	b.Write(sb)
	written := map[string]bool{}
	put := func(k string) error {
		v, ok := data[k]
		if !ok || written[k] {
			return nil
		}
		written[k] = true
		kb, _ := encodeNoEscape(k)
		vb, err := encodeNoEscape(v)
		if err != nil {
			return err
		}
		b.WriteByte(',')
		b.Write(kb)
		b.WriteByte(':')
		b.Write(vb)
		return nil
	}
	for _, k := range stepKeys[step] {
		if err := put(k); err != nil {
			return nil, err
		}
	}
	rest := make([]string, 0, len(data))
	for k := range data {
		if !written[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		if err := put(k); err != nil {
			return nil, err
		}
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// noBrowser reports a renderer that cannot render: nil, a nil pointer, or a browser client without a URL/token.
func noBrowser(b Renderer) bool {
	if b == nil {
		return true
	}
	if v := reflect.ValueOf(b); v.Kind() == reflect.Pointer && v.IsNil() {
		return true
	}
	if c, ok := b.(interface{ Configured() bool }); ok && !c.Configured() {
		return true
	}
	return false
}

// Enrich runs the person enrichment pipeline (PersonIntelService.enrich).
// notify (optional) receives the progress steps in Python's order with its
// payload keys; it is serialised, so it may write to a socket directly. Like
// Python a pipeline failure does not return an error: the profile comes back
// with status "failed" and an "error" step is sent. Only a cancelled ctx is
// returned as an error (no "error"/"completed" step is sent then).
//
// Enrich has no overall deadline of its own: searches (15 s each), GitHub and
// the deep phase (60 s) are individually bounded, but the caller must bound the
// total time with ctx (the WebSocket route's 120 s deadline).
func Enrich(ctx context.Context, d Deps, linkedinURL string, deep bool, notify func(step string, data map[string]any)) (Profile, error) {
	var mu sync.Mutex
	emit := func(step string, data map[string]any) {
		if notify == nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		notify(step, data)
	}
	profile := NewProfile(linkedinURL)
	profile.Status = "running"
	fail := func(err error) (Profile, error) {
		if ctx.Err() != nil {
			profile.Status = "failed"
			return *profile, ctx.Err()
		}
		profile.Status = "failed"
		emit("error", map[string]any{"message": err.Error()})
		return *profile, nil
	}

	username, err := ParseUsername(linkedinURL)
	if err != nil {
		return fail(err)
	}
	profile.Username = username
	emit("parsed", map[string]any{"username": username})
	emit("searching", map[string]any{"message": "Running web searches..."})

	byCat := map[string][]Result{}
	var cats []string
	var all []Result
	for _, cr := range RunSearches(ctx, d.HTTP, BuildQueries(username), 20, func(category string, count int) {
		emit("search_progress", map[string]any{"category": category, "count": count})
	}) {
		byCat[cr.Category] = cr.Results
		cats = append(cats, cr.Category)
		for _, r := range cr.Results {
			r.Category = cr.Category
			all = append(all, r)
		}
	}
	if ctx.Err() != nil {
		return fail(ctx.Err())
	}
	emit("search_done", map[string]any{"results_count": len(all), "categories": cats})

	emit("extracting", map[string]any{"message": "Extracting profile data..."})
	ExtractLinkedIn(profile, byCat["linkedin"])
	emit("profile_extracted", map[string]any{"name": profile.Name, "headline": profile.Headline})

	ExtractSocial(profile, map[string][]Result{
		"twitter": byCat["twitter"], "github": byCat["github"], "blog": byCat["blog"],
	})
	emit("socials_found", map[string]any{"social_links": profile.social()})

	if profile.social().Has("github") {
		emit("github_api", map[string]any{"message": "Fetching GitHub API data..."})
		if err := EnrichFromGitHub(ctx, d.HTTP, profile); err != nil {
			return fail(err)
		}
	}
	if ctx.Err() != nil {
		return fail(ctx.Err())
	}

	ExtractArticlesAndMentions(profile, byCat["articles"], byCat["general"])
	emit("articles_found", map[string]any{"articles_count": len(profile.Articles), "mentions_count": len(profile.Mentions)})

	if deep {
		if err := deepScrape(ctx, d, profile, all, emit); err != nil {
			return fail(err)
		}
	}

	ExtractEmails(profile, all)
	emit("emails_found", map[string]any{"email_count": len(profile.Emails)})

	profile.RawSources = all[:min(len(all), maxRawSources)]
	profile.Status = "completed"
	emit("completed", map[string]any{"profile": *profile})
	return *profile, nil
}

type deepOutput struct {
	Success         bool     `json:"success"`
	PreviewText     string   `json:"preview_text"`
	ExtractedEmails []string `json:"extracted_emails"`
	WordCount       int      `json:"word_count"`
}

// deepScrape (_deep_scrape_results) fetches up to DeepMaxPages result pages in
// Go (egress-guarded, capped); only JavaScript-wall pages are rendered by the browser service, one render per
// page, inside deepBudget. Page failures are skipped; a browser service that is down or misconfigured ends the
// phase with step deep_unavailable. Only the caller's ctx ending is returned as an error.
func deepScrape(ctx context.Context, d Deps, p *Profile, all []Result, emit func(string, map[string]any)) error {
	unavailable := func() {
		emit("deep_unavailable", map[string]any{"message": "Deep scraping unavailable: browser service not reachable"})
	}
	fetch := d.Fetch
	if fetch == nil {
		fetch = fetchPage(d.HTTP)
	}
	var idx []int
	seen := map[string]struct{}{}
pick:
	for i, r := range all {
		if _, dup := seen[r.URL]; dup || !strings.HasPrefix(r.URL, "http") {
			continue
		}
		for _, h := range deepSkipHosts {
			if strings.Contains(r.URL, h) {
				continue pick
			}
		}
		seen[r.URL] = struct{}{}
		idx = append(idx, i)
		if len(idx) >= DeepMaxPages {
			break
		}
	}
	emit("deep_scraping", map[string]any{"message": "Deep scraping result pages...", "total": min(len(all), 15)})

	dctx, cancel := context.WithTimeout(ctx, deepBudget)
	defer cancel()
	total := len(idx)
	for n, i := range idx {
		if dctx.Err() != nil {
			break
		}
		u := all[i].URL
		emit("scrape_progress", map[string]any{"url": u, "progress": strconv.Itoa(n+1) + "/" + strconv.Itoa(total)})
		skip := func() {
			emit("scrape_progress", map[string]any{"url": u, "progress": strconv.Itoa(n+1) + "/" + strconv.Itoa(total), "skipped": true})
		}
		// A URL the egress guard refuses is never fetched and never sent to the worker.
		if vetDeepURL(u) != nil {
			skip()
			continue
		}
		// Fast path: fetch in Go through the egress-pinned client, capped.
		body, ferr := fetch(dctx, u)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if dctx.Err() != nil {
			return nil // phase budget spent
		}
		if ferr != nil {
			skip()
			continue
		}
		if !needsBrowser(body) {
			if out := extractPage(body); out != nil {
				mergeScraped(p, &all[i], out)
			}
			if n < total-1 && deepDelay > 0 {
				t := time.NewTimer(deepDelay)
				select {
				case <-t.C:
				case <-dctx.Done():
					t.Stop()
				}
			}
			continue
		}
		// JavaScript wall: the browser service renders it (it re-checks the URL and every redirect hop).
		if noBrowser(d.Browser) {
			skip()
			continue
		}
		res, err := d.Browser.Render(dctx, browserclient.RenderRequest{URL: u, IncludeHTML: true, MaxBytes: maxPageBytes,
			Timeout: deepPageTimeout})
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err == nil:
			if out := extractPage([]byte(res.HTML)); out != nil {
				mergeScraped(p, &all[i], out) // keep a page that answered as the budget expired
			}
			if dctx.Err() != nil {
				return nil
			}
		case dctx.Err() != nil:
			return nil // phase budget spent
		case errors.Is(err, browserclient.ErrUnavailable), errors.Is(err, browserclient.ErrUnauthorized),
			errors.Is(err, browserclient.ErrBrowserUnavailable), errors.Is(err, browserclient.ErrEngineUnavailable):
			unavailable()
			return nil
		default:
			skip()
		}
		if n < total-1 && deepDelay > 0 {
			t := time.NewTimer(deepDelay)
			select {
			case <-t.C:
			case <-dctx.Done():
				t.Stop()
			}
		}
	}
	return ctx.Err()
}

// mergeScraped records one page's extraction (untrusted page content: caps and
// validates it) on the result and merges its emails into the profile.
func mergeScraped(p *Profile, r *Result, out json.RawMessage) {
	var o deepOutput
	if json.Unmarshal(out, &o) != nil || !o.Success {
		return
	}
	if rs := []rune(o.PreviewText); len(rs) > maxScrapedText {
		o.PreviewText = string(rs[:maxScrapedText])
	}
	r.ScrapedText = o.PreviewText
	if o.WordCount > 0 {
		r.ScrapedWordCount = o.WordCount
	}
	for _, e := range o.ExtractedEmails {
		if len(r.ScrapedEmails) >= maxScrapedMails {
			break
		}
		if len(e) > 254 || !fullEmailRe.MatchString(e) {
			continue
		}
		r.ScrapedEmails = append(r.ScrapedEmails, e)
		if !contains(p.Emails, e) {
			p.Emails = append(p.Emails, e)
		}
	}
}
