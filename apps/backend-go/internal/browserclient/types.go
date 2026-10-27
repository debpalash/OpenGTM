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
	"time"
)

func secs(d time.Duration) float64 { return d.Seconds() }

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
	URL         string        `json:"url"`
	Engine      string        `json:"engine,omitempty"`     // "browser" (default) | "impersonate" | "flaresolverr"
	WaitUntil   string        `json:"wait_until,omitempty"` // "commit" | "domcontentloaded" (default) | "load" | "networkidle"
	WaitMS      int           `json:"wait_ms,omitempty"`
	Scroll      bool          `json:"scroll,omitempty"`
	MaxScrolls  int           `json:"max_scrolls,omitempty"`
	IncludeHTML bool          `json:"include_html"`
	IncludeText bool          `json:"include_text"`
	MaxBytes    int           `json:"max_bytes,omitempty"` // cap for html and for text, each
	UserAgent   string        `json:"user_agent,omitempty"`
	Locale      string        `json:"locale,omitempty"`
	Cookies     []Cookie      `json:"cookies,omitempty"`
	Timeout     time.Duration `json:"-"`
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
	Query      string        `json:"query"`
	City       string        `json:"city"`
	MaxResults int           `json:"max_results,omitempty"`
	Timeout    time.Duration `json:"-"`
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
	URL     string        `json:"url"`
	Timeout time.Duration `json:"-"`
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
	MaxResults int           `json:"max_results,omitempty"`
	DelayS     *float64      `json:"delay_s,omitempty"`
	Timeout    time.Duration `json:"-"`
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
