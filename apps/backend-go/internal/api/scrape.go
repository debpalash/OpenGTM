package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/browserclient"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/unisearch"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// Plan 15 WP-ROUTES-B: the browser-backed and registry routes, behind GO_SCRAPE_ROUTES_ENABLED (default off; the
// routes then stay on FastAPI and nginx keeps sending them there until WP-RETIRE-INFRA).
//
//	GET  /api/v2/search/unified   routers/leads.py public_unified_search (current_workspace, 60/minute)
//	POST /api/v2/scraper/scrape   routers/leads.py public_scrape          (require_editor, 30/minute)
//	POST /api/v2/scrape           routers/scraper.py scrape_url           (global admin; saves history)
//	GET  /api/v2/scrapes          routers/scraper.py get_scrape_history   (global admin)
//
// Not ported (controller ruling D4): GET /api/sources and GET /api/trending stay unrouted (404). The scrape routes
// fetch user-supplied URLs: the Go strict URL guard (scheme, literal and resolved addresses) runs first, then the
// browser service fetches the page (its own egress proxy re-checks every redirect hop and subrequest). Go never
// fetches the URL itself and never uses the operator egress client for it.

const (
	scrapeRate         = "30/minute"
	unifiedRate        = "60/minute"
	scrapeFastTimeout  = 10 * time.Second
	scrapeSlowTimeout  = 60 * time.Second
	scrapeMaxBytes     = 5 << 20
	scrapePreviewChars = 2000
	scrapeBodyCap      = 1 << 20
	unifiedMaxPerSrc   = 100
	// scrapeUA is the browser user agent the legacy Playwright path used.
	scrapeUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
)

// pageRenderer is the slice of browserclient.Client the scraper uses.
type pageRenderer interface {
	Render(ctx context.Context, req browserclient.RenderRequest) (browserclient.RenderResult, error)
}

// unifiedSearcher is unisearch.Registry.
type unifiedSearcher interface {
	SearchUnified(ctx context.Context, query string, names []string, limitPerSource int, sortBy, fileType string) unisearch.Unified
}

type scrapeRoutes struct {
	d      Deps
	log    *slog.Logger
	render pageRenderer
	guard  urlGuard
	search unifiedSearcher
	store  scrapeStore

	// one limiter pool per route, built on first use (Limiter.Limit makes a fresh budget on every call)
	publicLim, adminLim, unifiedLim limitedOnce
}

// limitedOnce wraps inner with d.Limiter.Limit(spec) exactly once.
type limitedOnce struct {
	once sync.Once
	h    http.Handler
}

func (l *limitedOnce) get(d Deps, spec string, inner http.Handler) http.Handler {
	l.once.Do(func() {
		l.h = inner
		if d.Limiter != nil {
			l.h = d.Limiter.Limit(spec)(inner)
		}
	})
	return l.h
}

type scrapeCtxKey struct{}

// RegisterScrapeRoutes registers the four routes behind GO_SCRAPE_ROUTES_ENABLED.
func RegisterScrapeRoutes(r *httpx.Router, d Deps) {
	if d.Pool == nil || d.Identity == nil {
		return
	}
	on, err := config.ScrapeRoutesFrom(d.getenv())
	if err != nil {
		d.logger().Error("GO_SCRAPE_ROUTES_ENABLED is invalid; scrape routes are not registered")
		return
	}
	if !on {
		return
	}
	newScrapeRoutes(d).register(r)
}

func newScrapeRoutes(d Deps) *scrapeRoutes {
	cfg := config.ScrapeFrom(d.getenv())
	appEnv := d.Config.AppEnv
	if appEnv == "" {
		appEnv = d.getenv()("APP_ENV")
	}
	var bc *browserclient.Client
	if cfg.BrowserURL != "" {
		// internal service on the browser network: a plain client, never the egress client (it would refuse the
		// private address). The target URLs the service opens are guarded by Go first and by the service itself.
		bc = browserclient.New(cfg.BrowserURL, cfg.BrowserToken.Reveal(), nil)
	}
	hc := egress.NewClientForEnv(appEnv, d.EgressAllowHosts)
	h := &scrapeRoutes{d: d, log: d.logger(), render: bc, store: pgScrapeStore{pool: d.Pool},
		guard: urlGuard{appEnv: appEnv, allow: d.EgressAllowHosts}}
	h.search = unisearch.NewRegistry(unisearch.Config{GoogleAPIKey: cfg.GoogleAPIKey.Reveal(), GoogleCSEID: cfg.GoogleCSEID.Reveal(),
		GoogleBooksKey: cfg.GoogleBooksKey.Reveal()}, hc, unisearch.URLs{})
	return h
}

func (h *scrapeRoutes) register(r *httpx.Router) {
	d := h.d
	authn := identity.Authenticate(d.Signer, d.Identity)
	workspace := identity.Workspace(d.Identity)
	editor := identity.RequireRole("tables.write", "editor", "admin")
	admin := identity.RequireGlobalAdmin()
	r.Handle("GET /api/v2/search/unified", http.HandlerFunc(h.unified), authn, workspace)
	r.Handle("POST /api/v2/scraper/scrape", http.HandlerFunc(h.publicScrape), authn, workspace, editor)
	r.Handle("POST /api/v2/scrape", http.HandlerFunc(h.adminScrape), authn, admin)
	r.Handle("GET /api/v2/scrapes", http.HandlerFunc(h.history), authn, admin)
}

// ── URL guard ───────────────────────────────────────────────────────────────

// urlGuard is core/url_guard.check_url(resolve=True): scheme, literal addresses in any encoding, blocked host
// names and every resolved address. The bench/test allow-list applies only in APP_ENV test or bench.
type urlGuard struct {
	appEnv string
	allow  []string
	lookup func(ctx context.Context, host string) ([]netip.Addr, error) // nil: the system resolver
}

var errURLBlocked = errors.New("url not allowed")

type blockedURL struct{ reason string }

func (b blockedURL) Error() string        { return b.reason }
func (b blockedURL) Is(target error) bool { return target == errURLBlocked }

func (g urlGuard) allowlisted(host string) bool {
	if e := strings.ToLower(strings.TrimSpace(g.appEnv)); e != "test" && e != "bench" {
		return false
	}
	for _, a := range g.allow {
		if strings.ToLower(strings.TrimSpace(a)) == host {
			return true
		}
	}
	return false
}

func (g urlGuard) check(ctx context.Context, raw string) error {
	raw = strings.TrimSpace(raw)
	if err := egress.CheckURLFor(g.appEnv, g.allow, raw, true); err != nil {
		var be *egress.BlockedError
		if errors.As(err, &be) {
			return blockedURL{be.Reason}
		}
		return blockedURL{"blocked"}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return blockedURL{"unparseable url"}
	}
	host := strings.ToLower(u.Hostname())
	if g.allowlisted(host) {
		return nil
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return nil // a literal address was vetted by CheckURLFor
	}
	look := g.lookup
	if look == nil {
		look = func(ctx context.Context, h string) ([]netip.Addr, error) {
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			return net.DefaultResolver.LookupNetIP(ctx, "ip", h)
		}
	}
	addrs, err := look(ctx, host)
	if err != nil || len(addrs) == 0 {
		return blockedURL{"cannot resolve host"}
	}
	for _, a := range addrs {
		if egress.IsBlockedIP(a.Unmap()) {
			return blockedURL{"host resolves to a private address"}
		}
	}
	return nil
}

// ── scraping ────────────────────────────────────────────────────────────────

// scrapeResult is UniversalScraper._parse_html's dict.
type scrapeResult struct {
	URL, Title, Description, Method string
	Emails                          []string
	WordCount                       int
	Preview, HTML                   string
}

var scrapeEmailRE = regexp.MustCompile(`[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}`)

// pyFields is len(text.split()) and friends: Python splits on Unicode whitespace and the ASCII separators.
func pyWordCount(s string) int {
	return len(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }))
}

// fromRender is _parse_html over what the browser service already extracted: title and meta description from the
// page, text from the same html_to_text conversion (scripts, styles, embeds dropped, headings and lists kept,
// links and images stripped).
func fromRender(res browserclient.RenderResult, pageURL, method string) scrapeResult {
	title := res.Title
	if title == "" {
		title = pageURL
	}
	var emails []string
	seen := map[string]bool{}
	for _, e := range scrapeEmailRE.FindAllString(res.Text, -1) {
		if !seen[e] {
			seen[e] = true
			emails = append(emails, e)
		}
	}
	return scrapeResult{URL: pageURL, Title: title, Description: res.Description, Method: method, Emails: emails,
		WordCount: pyWordCount(res.Text), Preview: headRunes(res.Text, scrapePreviewChars), HTML: res.HTML}
}

func headRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := 0
	for j := range s {
		if i == n {
			return s[:j]
		}
		i++
	}
	return s
}

// jsWall is the fast-path check: "enable javascript" anywhere, or "please wait..." on a page under 2000 characters
// (Python: `a or b and c`).
func jsWall(html string) bool {
	lower := strings.ToLower(html)
	return strings.Contains(lower, "enable javascript") || (strings.Contains(lower, "please wait...") && utf8.RuneCountInString(html) < 2000)
}

// scrape is UniversalScraper.scrape without the screenshot stream: the URL is guarded, a plain fetch is tried
// first (method "fast"), and a JS wall, a non-200 or a fetch error falls back to the real browser with lazy-load
// scrolling (method "robust"). A blocked destination is a security decision, not a reason to retry elsewhere.
func (h *scrapeRoutes) scrape(ctx context.Context, pageURL string) (scrapeResult, error) {
	if err := h.guard.check(ctx, pageURL); err != nil {
		return scrapeResult{}, err
	}
	if h.render == nil || !browserConfigured(h.render) {
		return scrapeResult{}, browserclient.ErrUnavailable
	}
	fast, err := h.render.Render(ctx, browserclient.RenderRequest{URL: pageURL, Engine: "impersonate", IncludeHTML: true, IncludeText: true,
		MaxBytes: scrapeMaxBytes, Timeout: scrapeFastTimeout})
	switch {
	case err == nil:
		if fast.Status == http.StatusOK && !jsWall(fast.HTML) {
			final := fast.FinalURL
			if final == "" {
				final = pageURL
			}
			return fromRender(fast, final, "fast"), nil
		}
	case errors.Is(err, browserclient.ErrBlocked):
		return scrapeResult{}, blockedURL{"blocked by egress policy"}
	case errors.Is(err, browserclient.ErrUnavailable), errors.Is(err, browserclient.ErrUnauthorized):
		return scrapeResult{}, err
	}
	req := browserclient.RenderRequest{URL: pageURL, Engine: "browser", WaitUntil: "domcontentloaded", WaitMS: 2000, Scroll: true, MaxScrolls: 5,
		IncludeHTML: true, IncludeText: true, MaxBytes: scrapeMaxBytes, UserAgent: scrapeUA, Timeout: scrapeSlowTimeout}
	// Credential rule (review 3b M5): user-facing scrape routes fetch pages logged-out only. The operator's
	// LINKEDIN_LI_AT_COOKIE is never attached, because any workspace editor could reach it. Credentials may
	// transit to the browser service per request only when they are workspace-owned secrets of a workspace
	// feature that needs them (e.g. staffspy), and the service never stores them.
	res, err := h.render.Render(ctx, req)
	if err != nil {
		if errors.Is(err, browserclient.ErrBlocked) {
			return scrapeResult{}, blockedURL{"blocked by egress policy"}
		}
		return scrapeResult{}, err
	}
	return fromRender(res, pageURL, "robust"), nil
}

func browserConfigured(r pageRenderer) bool {
	if c, ok := r.(*browserclient.Client); ok {
		return c.Configured()
	}
	return true
}

// failure maps a scrape error to an HTTP status and a message that carries no upstream text.
func failure(err error) (int, string) {
	switch {
	case errors.Is(err, errURLBlocked):
		return http.StatusBadRequest, err.Error()
	case errors.Is(err, browserclient.ErrUnavailable), errors.Is(err, browserclient.ErrUnauthorized):
		return http.StatusServiceUnavailable, "browser service is not available"
	case errors.Is(err, browserclient.ErrBusy):
		return http.StatusServiceUnavailable, "browser service is busy"
	case errors.Is(err, browserclient.ErrTimeout):
		return http.StatusInternalServerError, "the page took too long to load"
	case errors.Is(err, browserclient.ErrNavigation):
		return http.StatusInternalServerError, "the page could not be loaded"
	}
	return http.StatusInternalServerError, "scrape failed"
}

func blockedDetail(err error) string {
	var b blockedURL
	if errors.As(err, &b) {
		return b.reason
	}
	return "blocked"
}

// ── POST /api/v2/scraper/scrape ─────────────────────────────────────────────

// publicScrapeBody validates `body: dict` and the url (a 422 never counts against the limit; a 400 does).
func (h *scrapeRoutes) publicScrape(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, scrapeBodyCap))
	if err != nil {
		httpx.Detail(w, http.StatusRequestEntityTooLarge, "Request body too large")
		return
	}
	obj, errs := wbcrud.DecodeBodyObject(raw)
	if errs != nil {
		httpx.ValidationError(w, errs...)
		return
	}
	pageURL := ""
	if v, ok := obj.Get("url"); ok {
		if s, isStr := v.(string); isStr {
			pageURL = strings.TrimSpace(s)
		}
	}
	h.publicLim.get(h.d, scrapeRate, http.HandlerFunc(h.publicRun)).ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), scrapeCtxKey{}, pageURL)))
}

func (h *scrapeRoutes) publicRun(w http.ResponseWriter, r *http.Request) {
	pageURL, _ := r.Context().Value(scrapeCtxKey{}).(string)
	if pageURL == "" {
		httpx.Detail(w, http.StatusBadRequest, "url is required")
		return
	}
	res, err := h.scrape(r.Context(), pageURL)
	if err != nil {
		status, msg := failure(err)
		if errors.Is(err, errURLBlocked) {
			httpx.Detail(w, http.StatusBadRequest, "url not allowed: "+blockedDetail(err))
			return
		}
		h.log.Warn("scraper/scrape failed", "status", status)
		// Python answers 200 with an error body for every other failure
		writePy(w, errorScrape{URL: pageURL, Status: "error", Error: msg})
		return
	}
	writePy(w, publicScrapeJSON(res))
}

type errorScrape struct {
	URL    string `json:"url"`
	Status string `json:"status"`
	Error  string `json:"error"`
}

// publicScrapeJSON is _parse_html's key order.
func publicScrapeJSON(s scrapeResult) any {
	return struct {
		Status      string   `json:"status"`
		URL         string   `json:"url"`
		Title       string   `json:"title"`
		Description string   `json:"description"`
		Method      string   `json:"method"`
		Emails      []string `json:"extracted_emails"`
		WordCount   int      `json:"word_count"`
		Preview     string   `json:"preview_text"`
		HTML        string   `json:"html_content"`
	}{"success", s.URL, s.Title, s.Description, s.Method, nonNil(s.Emails), s.WordCount, s.Preview, s.HTML}
}

// scrapeResponseJSON is the ScrapeResponse model's field order (POST /api/v2/scrape).
func scrapeResponseJSON(s scrapeResult) any {
	return struct {
		Status      string   `json:"status"`
		URL         string   `json:"url"`
		Title       string   `json:"title"`
		Description string   `json:"description"`
		Emails      []string `json:"extracted_emails"`
		WordCount   int      `json:"word_count"`
		Preview     string   `json:"preview_text"`
		HTML        string   `json:"html_content"`
		Method      string   `json:"method"`
	}{"success", s.URL, s.Title, s.Description, nonNil(s.Emails), s.WordCount, s.Preview, s.HTML, s.Method}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// ── POST /api/v2/scrape ─────────────────────────────────────────────────────

// adminScrape is routers/scraper.py scrape_url: ScrapeRequest{url: str} (a 422 never counts), then the limited
// scrape and the history/link/email writes.
func (h *scrapeRoutes) adminScrape(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, scrapeBodyCap))
	if err != nil {
		httpx.Detail(w, http.StatusRequestEntityTooLarge, "Request body too large")
		return
	}
	obj, errs := wbcrud.DecodeBodyObject(raw)
	if errs != nil {
		httpx.ValidationError(w, errs...)
		return
	}
	v, ok := obj.Get("url")
	if !ok {
		httpx.ValidationError(w, httpx.FieldError{Type: "missing", Loc: []any{"body", "url"}, Msg: "Field required", Input: obj})
		return
	}
	reqURL, isStr := v.(string)
	if !isStr {
		httpx.ValidationError(w, httpx.FieldError{Type: "string_type", Loc: []any{"body", "url"}, Msg: "Input should be a valid string", Input: v})
		return
	}
	h.adminLim.get(h.d, scrapeRate, http.HandlerFunc(h.adminRun)).ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), scrapeCtxKey{}, reqURL)))
}

func (h *scrapeRoutes) adminRun(w http.ResponseWriter, r *http.Request) {
	reqURL, _ := r.Context().Value(scrapeCtxKey{}).(string)
	res, err := h.scrape(r.Context(), reqURL)
	if err != nil {
		status, msg := failure(err)
		if errors.Is(err, errURLBlocked) {
			msg = "URL not allowed: " + blockedDetail(err)
		} else {
			h.log.Warn("v2/scrape failed", "status", status)
		}
		httpx.Detail(w, status, msg)
		return
	}
	if err := h.store.Save(r.Context(), reqURL, res); err != nil {
		h.log.Error("v2/scrape: saving history failed", "err_type", errTypeName(err))
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	writePy(w, scrapeResponseJSON(res))
}
