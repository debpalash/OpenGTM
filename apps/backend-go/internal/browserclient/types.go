// Package browserclient is the typed client for the internal Python browser
// service (apps/browser-service): Playwright rendering, screenshots and the
// browser / anti-bot scrapers. The wire format is the service's OpenAPI schema,
// checked in as testdata/openapi.json and contract-tested here.
//
// The service holds no secrets, no database and no business logic: callers pass
// everything a scrape needs (cookies included) and do lead mapping, dedup,
// scoring and caching in Go.
package browserclient

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

func secs(d time.Duration) float64 { return d.Seconds() }

// ProxyURL is an upstream proxy the caller picked for one request
// (http/socks5/socks4 URL, possibly with credentials, from PROXY_LIST or the
// proxy manager). It renders as [redacted] under fmt and slog; only the wire
// JSON carries the value. The service chains its own egress guard in front of
// it and never logs or stores it.
type ProxyURL string

func (ProxyURL) String() string             { return "[redacted]" }
func (ProxyURL) GoString() string           { return `"[redacted]"` }
func (ProxyURL) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte("[redacted]")) }
func (ProxyURL) LogValue() slog.Value       { return slog.StringValue("[redacted]") }

// MarshalJSON writes the real value: it is only used for the request body.
func (p ProxyURL) MarshalJSON() ([]byte, error) { return json.Marshal(string(p)) }

// ── health ──────────────────────────────────────────────────────────

// Health is GET /v1/health.
type Health struct {
	Status   string   `json:"status"`  // "ok" | "degraded"
	Version  string   `json:"version"` //
	Browser  string   `json:"browser"` // "ready" | "down" | "disabled"
	Active   int      `json:"active"`
	Capacity int      `json:"capacity"`
	Engines  []string `json:"engines"`
}

// ── render / screenshot ─────────────────────────────────────────────

// Cookie is set on the browser context before navigation (e.g. a LinkedIn
// li_at the caller resolved from its own secret store). Never logged.
type Cookie struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Domain   string `json:"domain"`
	Path     string `json:"path,omitempty"`
	Secure   bool   `json:"secure"`
	HTTPOnly bool   `json:"http_only"`
}

// RenderRequest is POST /v1/render. IncludeHTML/IncludeText are sent as given
// (a zero request asks for neither). Zero MaxScrolls/MaxBytes/WaitUntil/Engine
// mean the service default; Timeout zero means 30 s.
type RenderRequest struct {
	URL           string        `json:"url"`
	Engine        string        `json:"engine,omitempty"`     // "browser" (default) | "impersonate" | "flaresolverr"
	WaitUntil     string        `json:"wait_until,omitempty"` // "commit" | "domcontentloaded" (default) | "load" | "networkidle"
	WaitMS        int           `json:"wait_ms,omitempty"`
	Scroll        bool          `json:"scroll,omitempty"`
	MaxScrolls    int           `json:"max_scrolls,omitempty"`
	IncludeHTML   bool          `json:"include_html"`
	IncludeText   bool          `json:"include_text"`
	MaxBytes      int           `json:"max_bytes,omitempty"` // cap for html and for text, each
	UserAgent     string        `json:"user_agent,omitempty"`
	Locale        string        `json:"locale,omitempty"`
	Cookies       []Cookie      `json:"cookies,omitempty"`
	UpstreamProxy ProxyURL      `json:"upstream_proxy,omitempty"`
	Timeout       time.Duration `json:"-"`
}

func (r RenderRequest) MarshalJSON() ([]byte, error) {
	type alias RenderRequest
	return json.Marshal(struct {
		alias
		TimeoutS float64 `json:"timeout_s,omitempty"`
	}{alias(r), secs(r.Timeout)})
}

// RenderResult is the /v1/render answer.
type RenderResult struct {
	URL             string `json:"url"`
	FinalURL        string `json:"final_url"`
	Status          int    `json:"status"`
	Engine          string `json:"engine"`
	Title           string `json:"title"`
	Description     string `json:"description"`
	HTML            string `json:"html"`
	Text            string `json:"text"`
	HTMLTruncated   bool   `json:"html_truncated"`
	TextTruncated   bool   `json:"text_truncated"`
	Challenge       bool   `json:"challenge"`        // looks like a bot wall / JS shell
	BlockedRequests int    `json:"blocked_requests"` // subrequests the egress guard refused
	ElapsedMS       int    `json:"elapsed_ms"`
}

// ScreenshotRequest is POST /v1/screenshot. Timeout zero means 30 s.
type ScreenshotRequest struct {
	URL            string        `json:"url"`
	WaitUntil      string        `json:"wait_until,omitempty"`
	WaitMS         int           `json:"wait_ms,omitempty"`
	FullPage       bool          `json:"full_page,omitempty"`
	Format         string        `json:"format,omitempty"` // "jpeg" (default) | "png"
	Quality        int           `json:"quality,omitempty"`
	ViewportWidth  int           `json:"viewport_width,omitempty"`
	ViewportHeight int           `json:"viewport_height,omitempty"`
	Timeout        time.Duration `json:"-"`
}

func (r ScreenshotRequest) MarshalJSON() ([]byte, error) {
	type alias ScreenshotRequest
	return json.Marshal(struct {
		alias
		TimeoutS float64 `json:"timeout_s,omitempty"`
	}{alias(r), secs(r.Timeout)})
}

// ScreenshotResult carries the decoded image bytes.
type ScreenshotResult struct {
	URL         string `json:"url"`
	FinalURL    string `json:"final_url"`
	Status      int    `json:"status"`
	ContentType string `json:"content_type"`
	Image       []byte `json:"image_base64"`
	Bytes       int    `json:"bytes"`
	ElapsedMS   int    `json:"elapsed_ms"`
}

// ── browser scrapers ────────────────────────────────────────────────

// GoogleMapsRequest is POST /v1/scrape/google_maps. Timeout zero means 120 s.
type GoogleMapsRequest struct {
	Query         string        `json:"query"`
	City          string        `json:"city"`
	MaxResults    int           `json:"max_results,omitempty"`
	UpstreamProxy ProxyURL      `json:"upstream_proxy,omitempty"`
	Timeout       time.Duration `json:"-"`
}

func (r GoogleMapsRequest) MarshalJSON() ([]byte, error) {
	type alias GoogleMapsRequest
	return json.Marshal(struct {
		alias
		TimeoutS float64 `json:"timeout_s,omitempty"`
	}{alias(r), secs(r.Timeout)})
}

type MapPlace struct {
	Name     string `json:"name"`
	Address  string `json:"address"`
	Phone    string `json:"phone"`
	Website  string `json:"website"`
	Rating   string `json:"rating"`
	Category string `json:"category"`
}

type GoogleMapsResult struct {
	Source        string     `json:"source"`
	SearchURL     string     `json:"search_url"`
	ListingsFound int        `json:"listings_found"`
	Places        []MapPlace `json:"places"`
	Errors        int        `json:"errors"`
	Partial       bool       `json:"partial"`
	ElapsedMS     int        `json:"elapsed_ms"`
}

// JustDialPageRequest is POST /v1/scrape/justdial_page (justdial.com URLs only).
// Timeout zero means 45 s.
type JustDialPageRequest struct {
	URL           string        `json:"url"`
	UpstreamProxy ProxyURL      `json:"upstream_proxy,omitempty"`
	Timeout       time.Duration `json:"-"`
}

func (r JustDialPageRequest) MarshalJSON() ([]byte, error) {
	type alias JustDialPageRequest
	return json.Marshal(struct {
		alias
		TimeoutS float64 `json:"timeout_s,omitempty"`
	}{alias(r), secs(r.Timeout)})
}

type DirectoryListing struct {
	Name      string `json:"name"`
	PhoneText string `json:"phone_text"` // raw; normalise in Go
	Address   string `json:"address"`
}

type JustDialPageResult struct {
	Source    string             `json:"source"`
	FinalURL  string             `json:"final_url"`
	Status    int                `json:"status"`
	Listings  []DirectoryListing `json:"listings"`
	ElapsedMS int                `json:"elapsed_ms"`
}

// ── search-engine scrapers (ddgs) ───────────────────────────────────

// SearchOptions are shared by the search sources. MaxResults zero and DelayS
// nil select the legacy per-source defaults; Timeout zero means 120 s.
type SearchOptions struct {
	MaxResults    int           `json:"max_results,omitempty"`
	DelayS        *float64      `json:"delay_s,omitempty"`
	UpstreamProxy ProxyURL      `json:"upstream_proxy,omitempty"`
	Timeout       time.Duration `json:"-"`
}

// LinkedInRequest: Mode "companies" (Queries x Cities) or "people"
// (CompanyName x Titles; empty Titles = the legacy CEO/CTO/... list).
type LinkedInRequest struct {
	Mode        string   `json:"mode,omitempty"`
	Queries     []string `json:"queries,omitempty"`
	Cities      []string `json:"cities,omitempty"`
	CompanyName string   `json:"company_name,omitempty"`
	Titles      []string `json:"titles,omitempty"`
	SearchOptions
}

func (r LinkedInRequest) MarshalJSON() ([]byte, error) {
	type alias LinkedInRequest
	return json.Marshal(struct {
		alias
		TimeoutS float64 `json:"timeout_s,omitempty"`
	}{alias(r), secs(r.Timeout)})
}

// JobBoardsRequest: Board "naukri" | "indeed" | "foundit".
type JobBoardsRequest struct {
	Board   string   `json:"board"`
	Queries []string `json:"queries"`
	Cities  []string `json:"cities"`
	SearchOptions
}

func (r JobBoardsRequest) MarshalJSON() ([]byte, error) {
	type alias JobBoardsRequest
	return json.Marshal(struct {
		alias
		TimeoutS float64 `json:"timeout_s,omitempty"`
	}{alias(r), secs(r.Timeout)})
}

type CrunchbaseRequest struct {
	Queries []string `json:"queries"`
	Cities  []string `json:"cities"`
	SearchOptions
}

func (r CrunchbaseRequest) MarshalJSON() ([]byte, error) {
	type alias CrunchbaseRequest
	return json.Marshal(struct {
		alias
		TimeoutS float64 `json:"timeout_s,omitempty"`
	}{alias(r), secs(r.Timeout)})
}

// ReviewDirectoriesRequest: Site "clutch" | "goodfirms" | "g2" | "ambitionbox"
// (Cities are used, and required, for ambitionbox only).
type ReviewDirectoriesRequest struct {
	Site    string   `json:"site"`
	Queries []string `json:"queries"`
	Cities  []string `json:"cities,omitempty"`
	SearchOptions
}

func (r ReviewDirectoriesRequest) MarshalJSON() ([]byte, error) {
	type alias ReviewDirectoriesRequest
	return json.Marshal(struct {
		alias
		TimeoutS float64 `json:"timeout_s,omitempty"`
	}{alias(r), secs(r.Timeout)})
}

// WebDirectoriesRequest: Sites subset of "justdial", "sulekha", "indiamart"
// (empty = all three).
type WebDirectoriesRequest struct {
	Queries []string `json:"queries"`
	Cities  []string `json:"cities"`
	Sites   []string `json:"sites,omitempty"`
	SearchOptions
}

func (r WebDirectoriesRequest) MarshalJSON() ([]byte, error) {
	type alias WebDirectoriesRequest
	return json.Marshal(struct {
		alias
		TimeoutS float64 `json:"timeout_s,omitempty"`
	}{alias(r), secs(r.Timeout)})
}

// GoogleSearchRequest: Mode "web" (Queries x Cities) or "news" (Queries).
type GoogleSearchRequest struct {
	Mode    string   `json:"mode,omitempty"`
	Queries []string `json:"queries"`
	Cities  []string `json:"cities,omitempty"`
	SearchOptions
}

func (r GoogleSearchRequest) MarshalJSON() ([]byte, error) {
	type alias GoogleSearchRequest
	return json.Marshal(struct {
		alias
		TimeoutS float64 `json:"timeout_s,omitempty"`
	}{alias(r), secs(r.Timeout)})
}

type SearchHit struct {
	Title  string `json:"title"`
	URL    string `json:"url"`
	Body   string `json:"body"`
	Date   string `json:"date"`
	Source string `json:"source"`
}

// SearchRun is one search string and its raw hits. Error is "" on success, or
// "no_results" | "rate_limited" | "timeout" | "failed".
type SearchRun struct {
	Variant string      `json:"variant"`
	Query   string      `json:"query"`
	City    string      `json:"city"`
	Term    string      `json:"term"`
	Search  string      `json:"search"`
	Hits    []SearchHit `json:"hits"`
	Error   string      `json:"error"`
}

// SearchResult: Partial means the deadline cut the search list short.
type SearchResult struct {
	Source    string      `json:"source"`
	Runs      []SearchRun `json:"runs"`
	Partial   bool        `json:"partial"`
	ElapsedMS int         `json:"elapsed_ms"`
}

// ── generic search (ddgs) ───────────────────────────────────────────

// SearchRequest is POST /v1/search: one keyless ddgs query. Backends nil uses
// the service default (BROWSER_SEARCH_BACKENDS); an empty non-nil slice uses
// the ddgs default. Zero MaxResults/Region/SafeSearch/Method mean the service
// defaults (10, "us-en", "moderate", "text"); Timeout zero means 30 s.
type SearchRequest struct {
	Query         string        `json:"query"`
	Method        string        `json:"method,omitempty"` // "text" | "news"
	MaxResults    int           `json:"max_results,omitempty"`
	Backends      []string      `json:"backends"`
	Region        string        `json:"region,omitempty"`
	SafeSearch    string        `json:"safesearch,omitempty"` // "on" | "moderate" | "off"
	TimeLimit     string        `json:"timelimit,omitempty"`  // "d" | "w" | "m" | "y"
	UpstreamProxy ProxyURL      `json:"upstream_proxy,omitempty"`
	Timeout       time.Duration `json:"-"`
}

func (r SearchRequest) MarshalJSON() ([]byte, error) {
	type alias SearchRequest
	return json.Marshal(struct {
		alias
		TimeoutS float64 `json:"timeout_s,omitempty"`
	}{alias(r), secs(r.Timeout)})
}

// SearchOut is the /v1/search answer. Error is "" on success, or
// "no_results" | "rate_limited" | "timeout" | "failed" (rate_limited and
// timeout are the legacy block signals that grow the per-host backoff).
type SearchOut struct {
	Query     string      `json:"query"`
	Method    string      `json:"method"`
	Hits      []SearchHit `json:"hits"`
	Error     string      `json:"error"`
	ElapsedMS int         `json:"elapsed_ms"`
}

// ── extract ─────────────────────────────────────────────────────────

// ExtractRequest is POST /v1/extract: HTML given (URL is then only the page
// address for metadata), or URL fetched with Engine ("impersonate" default |
// "browser"). IncludeMarkdown is sent as given. Timeout zero means 30 s.
type ExtractRequest struct {
	HTML            string        `json:"html,omitempty"`
	URL             string        `json:"url,omitempty"`
	Engine          string        `json:"engine,omitempty"`
	IncludeMarkdown bool          `json:"include_markdown"`
	MaxBytes        int           `json:"max_bytes,omitempty"`
	UpstreamProxy   ProxyURL      `json:"upstream_proxy,omitempty"`
	Timeout         time.Duration `json:"-"`
}

func (r ExtractRequest) MarshalJSON() ([]byte, error) {
	type alias ExtractRequest
	return json.Marshal(struct {
		alias
		TimeoutS float64 `json:"timeout_s,omitempty"`
	}{alias(r), secs(r.Timeout)})
}

// ExtractResult: Text and metadata from trafilatura (legacy content_extract),
// Markdown from markdownify. Found is false when neither text nor any metadata
// field was found (legacy returned None or an all-empty dict).
type ExtractResult struct {
	Found             bool   `json:"found"`
	Text              string `json:"text"`
	Markdown          string `json:"markdown"`
	WordCount         int    `json:"word_count"`
	Title             string `json:"title"`
	Author            string `json:"author"`
	Date              string `json:"date"`
	Sitename          string `json:"sitename"`
	Description       string `json:"description"`
	FinalURL          string `json:"final_url"`
	Status            int    `json:"status"`
	TextTruncated     bool   `json:"text_truncated"`
	MarkdownTruncated bool   `json:"markdown_truncated"`
	ElapsedMS         int    `json:"elapsed_ms"`
}

// ── holehe ──────────────────────────────────────────────────────────

// HoleheRequest is POST /v1/holehe. MaxModules zero means 50 (legacy).
// Timeout zero means 60 s.
type HoleheRequest struct {
	Email         string        `json:"email"`
	MaxModules    int           `json:"max_modules,omitempty"`
	UpstreamProxy ProxyURL      `json:"upstream_proxy,omitempty"`
	Timeout       time.Duration `json:"-"`
}

func (r HoleheRequest) MarshalJSON() ([]byte, error) {
	type alias HoleheRequest
	return json.Marshal(struct {
		alias
		TimeoutS float64 `json:"timeout_s,omitempty"`
	}{alias(r), secs(r.Timeout)})
}

type HoleheSite struct {
	Name        string `json:"name"`
	Domain      string `json:"domain"`
	Method      string `json:"method"`
	Exists      bool   `json:"exists"`
	RateLimited bool   `json:"rate_limited"`
}

// HoleheResult: raw per-site results in module order. The verdict mapping
// (legacy is_real = len(ExistsOn) >= 2, email_verify, confidence) is Go's.
type HoleheResult struct {
	Email        string       `json:"email"`
	Sites        []HoleheSite `json:"sites"`
	ExistsOn     []string     `json:"exists_on"`
	TotalChecked int          `json:"total_checked"`
	Errors       int          `json:"errors"`
	Partial      bool         `json:"partial"`
	ElapsedMS    int          `json:"elapsed_ms"`
}

// ── staffspy ────────────────────────────────────────────────────────

// StaffSpyRequest is POST /v1/staffspy (503 engine_unavailable unless the
// service runs with BROWSER_STAFFSPY_ENABLED). Cookies carry the LinkedIn
// session (li_at required, JSESSIONID for the csrf token), resolved by the
// caller from its own secret store. MaxResults zero means 25; Timeout zero
// means 180 s.
type StaffSpyRequest struct {
	CompanyName   string        `json:"company_name"`
	SearchTerm    string        `json:"search_term,omitempty"`
	Location      string        `json:"location,omitempty"`
	MaxResults    int           `json:"max_results,omitempty"`
	Cookies       []Cookie      `json:"cookies"`
	UpstreamProxy ProxyURL      `json:"upstream_proxy,omitempty"`
	Timeout       time.Duration `json:"-"`
}

func (r StaffSpyRequest) MarshalJSON() ([]byte, error) {
	type alias StaffSpyRequest
	return json.Marshal(struct {
		alias
		TimeoutS float64 `json:"timeout_s,omitempty"`
	}{alias(r), secs(r.Timeout)})
}

type StaffRow struct {
	Name     string `json:"name"`
	Title    string `json:"title"`
	LinkedIn string `json:"linkedin"`
	Location string `json:"location"`
}

// StaffSpyResult: Error is "" or "login_failed" | "rate_limited" |
// "no_staff_found" | "failed". Seniority ranking is Go's.
type StaffSpyResult struct {
	CompanyName string     `json:"company_name"`
	Staff       []StaffRow `json:"staff"`
	Error       string     `json:"error"`
	ElapsedMS   int        `json:"elapsed_ms"`
}

// ── stats ───────────────────────────────────────────────────────────

type OpStats struct {
	Calls  int            `json:"calls"`
	OK     int            `json:"ok"`
	Errors map[string]int `json:"errors"`
}

type EgressStats struct {
	Sessions       int `json:"sessions"`
	Blocked        int `json:"blocked"`
	UpstreamFailed int `json:"upstream_failed"`
}

// Stats is GET /v1/stats: this service process's counters since start (per
// replica; not aggregated).
type Stats struct {
	Version  string             `json:"version"`
	UptimeS  int                `json:"uptime_s"`
	Active   int                `json:"active"`
	Capacity int                `json:"capacity"`
	Ops      map[string]OpStats `json:"ops"`
	Searches map[string]int     `json:"searches"`
	Egress   EgressStats        `json:"egress"`
}
