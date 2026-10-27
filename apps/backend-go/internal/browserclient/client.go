package browserclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
)

const (
	// MaxTimeout mirrors the service schema bound (timeout_s <= 600); the
	// service further clamps to its BROWSER_MAX_TIMEOUT_S.
	MaxTimeout = 600 * time.Second
	// clientSlack covers the service's queue wait (BROWSER_QUEUE_WAIT_S, 5 s)
	// and its post-deadline grace (2 s) so the server answers before we give up.
	clientSlack = 10 * time.Second
	// DefaultMaxBodyBytes caps a success body (service default response cap is
	// 5 MiB; raise MaxBodyBytes with BROWSER_MAX_RESPONSE_BYTES).
	DefaultMaxBodyBytes = 16 << 20
	maxErrorBody        = 64 << 10
	healthTimeout       = 5 * time.Second
)

// Sentinel errors; every *Error matches exactly one via errors.Is.
var (
	ErrUnavailable        = errors.New("browserclient: browser service not configured")
	ErrUnauthorized       = errors.New("browserclient: token rejected")
	ErrBadRequest         = errors.New("browserclient: request rejected")
	ErrBlocked            = errors.New("browserclient: destination blocked by egress policy")
	ErrBusy               = errors.New("browserclient: browser service at capacity")
	ErrTimeout            = errors.New("browserclient: deadline exceeded")
	ErrNavigation         = errors.New("browserclient: page could not be loaded")
	ErrUpstream           = errors.New("browserclient: upstream service failed")
	ErrEngineUnavailable  = errors.New("browserclient: engine not available")
	ErrBrowserUnavailable = errors.New("browserclient: browser not available")
	ErrTooLarge           = errors.New("browserclient: size cap exceeded")
	ErrUnreachable        = errors.New("browserclient: browser service unreachable")
	ErrInvalidResponse    = errors.New("browserclient: invalid response")
	ErrServer             = errors.New("browserclient: browser service error")
)

var sentinels = map[string]error{
	"unauthorized":        ErrUnauthorized,
	"bad_request":         ErrBadRequest,
	"not_found":           ErrBadRequest,
	"method_not_allowed":  ErrBadRequest,
	"egress_blocked":      ErrBlocked,
	"busy":                ErrBusy,
	"timeout":             ErrTimeout,
	"navigation_failed":   ErrNavigation,
	"upstream_error":      ErrUpstream,
	"engine_unavailable":  ErrEngineUnavailable,
	"browser_unavailable": ErrBrowserUnavailable,
	"too_large":           ErrTooLarge,
	"unreachable":         ErrUnreachable,
	"invalid_response":    ErrInvalidResponse,
	"internal":            ErrServer,
}

// Error is a failed call. Code is one of the fixed service codes (or
// "unreachable" / "invalid_response" from the client side); it never carries
// upstream text, the token or page content. Fields lists rejected request
// fields for bad_request (our own schema names only).
type Error struct {
	Code       string
	HTTPStatus int
	Fields     []string
}

func (e *Error) Error() string {
	s := "browserclient: " + e.Code
	if e.HTTPStatus != 0 {
		s += " (http " + strconv.Itoa(e.HTTPStatus) + ")"
	}
	if len(e.Fields) > 0 {
		s += " fields=" + strings.Join(e.Fields, ",")
	}
	return s
}

func (e *Error) Is(target error) bool { return sentinels[e.Code] == target }

// Retryable reports whether the same call may succeed later (capacity,
// deadline, transport, browser restart). Egress blocks and bad requests never do.
func Retryable(err error) bool {
	return errors.Is(err, ErrBusy) || errors.Is(err, ErrTimeout) || errors.Is(err, ErrUnreachable) ||
		errors.Is(err, ErrBrowserUnavailable) || errors.Is(err, ErrServer)
}

// Client calls the browser service. The token is never logged, formatted or
// returned in errors. Calls are not retried: browser work is expensive, so the
// caller decides (see Retryable).
type Client struct {
	baseURL string
	token   config.Secret
	hc      *http.Client
	// MaxBodyBytes caps success bodies (default DefaultMaxBodyBytes).
	MaxBodyBytes int64
	// AllowedPorts are the destination ports URLs may name (the service's
	// BROWSER_EGRESS_PORTS); empty allows any. New sets 80 and 443.
	AllowedPorts []int
	slack        time.Duration
}

// New builds a client for BROWSER_SERVICE_URL / BROWSER_SERVICE_TOKEN. hc may be
// nil; per-call deadlines come from the request Timeout, not hc.Timeout.
// Redirects are never followed.
func New(baseURL, token string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{}
	} else {
		cp := *hc
		cp.Timeout = 0
		hc = &cp
	}
	// Never honour HTTP(S)_PROXY: the bearer token and target URLs must not go
	// through an environment proxy (review L8).
	switch tr := hc.Transport.(type) {
	case nil:
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.Proxy = nil
		hc.Transport = t
	case *http.Transport:
		t := tr.Clone()
		t.Proxy = nil
		hc.Transport = t
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"), token: config.Secret(token), hc: hc,
		MaxBodyBytes: DefaultMaxBodyBytes, slack: clientSlack, AllowedPorts: []int{80, 443}}
}

// checkTarget pre-checks a URL the service will open with the Go egress guard
// (scheme, literal private/metadata IPs, blocked hostnames), refuses userinfo
// and ports outside AllowedPorts. The service re-checks every hop itself.
func (c *Client) checkTarget(raw string) error {
	if !c.Configured() {
		return ErrUnavailable
	}
	blocked := &Error{Code: "egress_blocked"}
	if err := egress.CheckURL(raw, true); err != nil {
		return blocked
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.User != nil || u.Hostname() == "" {
		return blocked
	}
	port := 443
	if strings.EqualFold(u.Scheme, "http") {
		port = 80
	}
	if p := u.Port(); p != "" {
		if port, err = strconv.Atoi(p); err != nil {
			return blocked
		}
	}
	if len(c.AllowedPorts) > 0 && !slices.Contains(c.AllowedPorts, port) {
		return blocked
	}
	return nil
}

// Format redacts the token for every fmt verb.
func (c Client) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(c.String())) }

func (c Client) String() string {
	return "browserclient.Client{url:" + c.baseURL + ", token:[redacted]}"
}

// Configured reports whether URL and token are set.
func (c *Client) Configured() bool { return c != nil && c.baseURL != "" && c.token != "" }

func clamp(d, def time.Duration) time.Duration {
	if d <= 0 {
		d = def
	}
	if d > MaxTimeout {
		d = MaxTimeout
	}
	return d
}

// Health calls GET /v1/health (5 s deadline).
func (c *Client) Health(ctx context.Context) (Health, error) {
	var out Health
	err := c.do(ctx, http.MethodGet, "/v1/health", nil, healthTimeout, &out)
	return out, err
}

// Render loads one page (POST /v1/render).
func (c *Client) Render(ctx context.Context, req RenderRequest) (RenderResult, error) {
	if err := c.checkTarget(req.URL); err != nil {
		return RenderResult{}, err
	}
	req.Timeout = clamp(req.Timeout, 30*time.Second)
	var out RenderResult
	return out, c.post(ctx, "/v1/render", req, req.Timeout, &out)
}

// Screenshot captures one page (POST /v1/screenshot).
func (c *Client) Screenshot(ctx context.Context, req ScreenshotRequest) (ScreenshotResult, error) {
	if err := c.checkTarget(req.URL); err != nil {
		return ScreenshotResult{}, err
	}
	req.Timeout = clamp(req.Timeout, 30*time.Second)
	var out ScreenshotResult
	return out, c.post(ctx, "/v1/screenshot", req, req.Timeout, &out)
}

// GoogleMaps runs the Maps place scraper (POST /v1/scrape/google_maps).
func (c *Client) GoogleMaps(ctx context.Context, req GoogleMapsRequest) (GoogleMapsResult, error) {
	req.Timeout = clamp(req.Timeout, 120*time.Second)
	var out GoogleMapsResult
	return out, c.post(ctx, "/v1/scrape/google_maps", req, req.Timeout, &out)
}

// JustDialPage scrapes one JustDial listing page (POST /v1/scrape/justdial_page).
func (c *Client) JustDialPage(ctx context.Context, req JustDialPageRequest) (JustDialPageResult, error) {
	if err := c.checkTarget(req.URL); err != nil {
		return JustDialPageResult{}, err
	}
	req.Timeout = clamp(req.Timeout, 45*time.Second)
	var out JustDialPageResult
	return out, c.post(ctx, "/v1/scrape/justdial_page", req, req.Timeout, &out)
}

func (c *Client) search(ctx context.Context, source string, body any, timeout time.Duration) (SearchResult, error) {
	var out SearchResult
	return out, c.post(ctx, "/v1/scrape/"+source, body, timeout, &out)
}

// LinkedIn runs POST /v1/scrape/linkedin.
func (c *Client) LinkedIn(ctx context.Context, req LinkedInRequest) (SearchResult, error) {
	req.Timeout = clamp(req.Timeout, 120*time.Second)
	return c.search(ctx, "linkedin", req, req.Timeout)
}

// JobBoards runs POST /v1/scrape/job_boards.
func (c *Client) JobBoards(ctx context.Context, req JobBoardsRequest) (SearchResult, error) {
	req.Timeout = clamp(req.Timeout, 120*time.Second)
	return c.search(ctx, "job_boards", req, req.Timeout)
}

// Crunchbase runs POST /v1/scrape/crunchbase.
func (c *Client) Crunchbase(ctx context.Context, req CrunchbaseRequest) (SearchResult, error) {
	req.Timeout = clamp(req.Timeout, 120*time.Second)
	return c.search(ctx, "crunchbase", req, req.Timeout)
}

// ReviewDirectories runs POST /v1/scrape/review_directories.
func (c *Client) ReviewDirectories(ctx context.Context, req ReviewDirectoriesRequest) (SearchResult, error) {
	req.Timeout = clamp(req.Timeout, 120*time.Second)
	return c.search(ctx, "review_directories", req, req.Timeout)
}

// WebDirectories runs POST /v1/scrape/web_directories.
func (c *Client) WebDirectories(ctx context.Context, req WebDirectoriesRequest) (SearchResult, error) {
	req.Timeout = clamp(req.Timeout, 120*time.Second)
	return c.search(ctx, "web_directories", req, req.Timeout)
}

// GoogleSearch runs POST /v1/scrape/google_search.
func (c *Client) GoogleSearch(ctx context.Context, req GoogleSearchRequest) (SearchResult, error) {
	req.Timeout = clamp(req.Timeout, 120*time.Second)
	return c.search(ctx, "google_search", req, req.Timeout)
}

// Search runs one keyless ddgs query (POST /v1/search).
func (c *Client) Search(ctx context.Context, req SearchRequest) (SearchOut, error) {
	req.Timeout = clamp(req.Timeout, 30*time.Second)
	var out SearchOut
	return out, c.post(ctx, "/v1/search", req, req.Timeout, &out)
}

// Extract returns main text, metadata and markdown for HTML or a URL.
func (c *Client) Extract(ctx context.Context, req ExtractRequest) (ExtractResult, error) {
	if req.HTML == "" {
		if err := c.checkTarget(req.URL); err != nil {
			return ExtractResult{}, err
		}
	}
	req.Timeout = clamp(req.Timeout, 30*time.Second)
	var out ExtractResult
	return out, c.post(ctx, "/v1/extract", req, req.Timeout, &out)
}

// Holehe checks which sites an email is registered on.
func (c *Client) Holehe(ctx context.Context, req HoleheRequest) (HoleheResult, error) {
	req.Timeout = clamp(req.Timeout, 60*time.Second)
	var out HoleheResult
	return out, c.post(ctx, "/v1/holehe", req, req.Timeout, &out)
}

// StaffSpy scrapes a company's LinkedIn roster.
func (c *Client) StaffSpy(ctx context.Context, req StaffSpyRequest) (StaffSpyResult, error) {
	req.Timeout = clamp(req.Timeout, 180*time.Second)
	var out StaffSpyResult
	return out, c.post(ctx, "/v1/staffspy", req, req.Timeout, &out)
}

// Stats returns the service process counters (GET /v1/stats).
func (c *Client) Stats(ctx context.Context) (Stats, error) {
	var out Stats
	err := c.do(ctx, http.MethodGet, "/v1/stats", nil, healthTimeout, &out)
	return out, err
}

func (c *Client) post(ctx context.Context, path string, body any, timeout time.Duration, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return &Error{Code: "bad_request"}
	}
	return c.do(ctx, http.MethodPost, path, data, timeout, out)
}

type wireError struct {
	Error struct {
		Code   string   `json:"code"`
		Fields []string `json:"fields"`
	} `json:"error"`
}

var fieldRe = regexp.MustCompile(`^[a-z0-9_.]{1,64}$`)

func (c *Client) do(ctx context.Context, method, path string, body []byte, timeout time.Duration, out any) error {
	if !c.Configured() {
		return ErrUnavailable
	}
	rctx, cancel := context.WithTimeout(ctx, timeout+c.slack)
	defer cancel()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(rctx, method, c.baseURL+path, rdr)
	if err != nil {
		return &Error{Code: "bad_request"}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token.Reveal())
	resp, err := c.hc.Do(req)
	if err != nil {
		return c.transportErr(ctx, rctx) // url.Error text is dropped on purpose
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return decodeError(resp)
	}
	limit := c.MaxBodyBytes
	if limit <= 0 {
		limit = DefaultMaxBodyBytes
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return c.transportErr(ctx, rctx)
	}
	if int64(len(data)) > limit {
		return &Error{Code: "too_large", HTTPStatus: resp.StatusCode}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return &Error{Code: "invalid_response", HTTPStatus: resp.StatusCode}
	}
	return nil
}

func (c *Client) transportErr(ctx, rctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(rctx.Err(), context.DeadlineExceeded) {
		return &Error{Code: "timeout"}
	}
	return &Error{Code: "unreachable"}
}

func decodeError(resp *http.Response) error {
	st := resp.StatusCode
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	var w wireError
	_ = json.Unmarshal(data, &w)
	code := w.Error.Code
	if _, known := sentinels[code]; !known || code == "unreachable" || code == "invalid_response" {
		code = codeForStatus(st)
	}
	e := &Error{Code: code, HTTPStatus: st}
	if code == "bad_request" {
		for _, f := range w.Error.Fields {
			if len(e.Fields) < 10 && fieldRe.MatchString(f) {
				e.Fields = append(e.Fields, f)
			}
		}
	}
	return e
}

func codeForStatus(st int) string {
	switch {
	case st == http.StatusUnauthorized:
		return "unauthorized"
	case st == http.StatusForbidden:
		return "egress_blocked"
	case st == http.StatusRequestEntityTooLarge:
		return "too_large"
	case st == http.StatusTooManyRequests:
		return "busy"
	case st == http.StatusGatewayTimeout:
		return "timeout"
	case st == http.StatusServiceUnavailable:
		return "browser_unavailable"
	case st >= 500:
		return "internal"
	default:
		return "bad_request"
	}
}
