package leadweb

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
)

const spicrawlEndpoint = "https://api.spicrawl.com/v1/scrape"
const spicrawlMaxBody = 4 << 20

var errSpicrawlContent = errors.New("spicrawl: valid successful target markdown was not returned")
var errSpicrawlURL = errors.New("spicrawl: target must be a public HTTP(S) URL without credentials")

// Spicrawl is an opt-in markdown/PDF client. Endpoint and resolver are internal;
// neither API target nor authentication can be controlled by a workbook cell.
type Spicrawl struct {
	key      config.Secret
	hc       *http.Client
	timeout  time.Duration
	endpoint string
	lookup   func(context.Context, string, string) ([]netip.Addr, error)
}

type SpicrawlOptions struct {
	IncludeTags []string
	ExcludeTags []string
	JSRender    bool // Explicit only: no automatic paid retries/escalation.
}

type SpicrawlResult struct {
	Markdown       string
	TargetStatus   int
	CreditsCharged int
}

// Formatting an unexported Secret field via reflection can bypass its own
// formatter. Redact the whole client for every fmt verb.
func (Spicrawl) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("Spicrawl{credentials:[redacted]}"))
}

func NewSpicrawl(cfg config.LeadwebSettings, hc *http.Client) *Spicrawl {
	if cfg.SpicrawlKey == "" {
		return nil
	}
	timeout := cfg.SpicrawlTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &Spicrawl{key: cfg.SpicrawlKey, hc: hc, timeout: timeout,
		endpoint: spicrawlEndpoint, lookup: net.DefaultResolver.LookupNetIP}
}

// Fetch implements the research PageFetcher contract (at most 2000 characters).
// Scrape exposes the bounded full document and billing headers to other callers.
func (s *Spicrawl) Fetch(ctx context.Context, u string) (string, error) {
	r, err := s.Scrape(ctx, u, SpicrawlOptions{})
	if err != nil {
		return "", err
	}
	text := []rune(r.Markdown)
	return string(text[:min(len(text), 2000)]), nil
}

func (s *Spicrawl) Scrape(ctx context.Context, rawURL string, o SpicrawlOptions) (SpicrawlResult, error) {
	var result SpicrawlResult
	if s == nil || s.key == "" {
		return result, errProviderResponse
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if !publicPageURL(rawURL) {
		return result, errSpicrawlURL
	}
	u, _ := url.Parse(rawURL)
	// Do not send fragment-only tokens to a third party.
	u.Fragment, u.RawFragment = "", ""
	ips, err := s.lookup(ctx, "ip", u.Hostname())
	if err != nil || len(ips) == 0 {
		return result, errSpicrawlURL
	}
	for _, ip := range ips {
		if egress.IsBlockedIP(ip) {
			return result, errSpicrawlURL
		}
	}
	for _, selectors := range [][]string{o.IncludeTags, o.ExcludeTags} {
		if len(selectors) > 32 {
			return result, errors.New("spicrawl: too many selectors")
		}
		for _, selector := range selectors {
			if len(selector) == 0 || len(selector) > 256 || !utf8.ValidString(selector) {
				return result, errors.New("spicrawl: invalid selector")
			}
		}
	}
	maxCost := 1
	if o.JSRender {
		maxCost = 3
	}
	body := map[string]any{"url": u.String(), "response_format": "markdown", "main_content_only": true,
		"parse_pdf": true, "links": false, "js_render": o.JSRender, "cache": true, "max_cost": maxCost}
	if len(o.IncludeTags) > 0 {
		body["include_tags"] = o.IncludeTags
	}
	if len(o.ExcludeTags) > 0 {
		body["exclude_tags"] = o.ExcludeTags
	}
	headers, raw, err := postProvider(ctx, s.hc, providerRequest{endpoint: s.endpoint, key: s.key,
		body: body, timeout: s.timeout, accept: "text/markdown, text/plain", maxBytes: spicrawlMaxBody})
	if err != nil {
		return result, err
	}
	target, err := strconv.Atoi(headers.Get("X-Target-Status"))
	if err != nil || target < 200 || target >= 300 {
		return result, errSpicrawlContent
	}
	credits, err := strconv.Atoi(headers.Get("X-Credits-Charged"))
	if err != nil || credits < 0 || credits > maxCost {
		return result, errSpicrawlContent
	}
	contentType, _, err := mime.ParseMediaType(headers.Get("Content-Type"))
	if err != nil || (contentType != "text/markdown" && contentType != "text/plain") ||
		!utf8.Valid(raw) || strings.TrimSpace(string(raw)) == "" {
		return result, errSpicrawlContent
	}
	return SpicrawlResult{Markdown: string(raw), TargetStatus: target, CreditsCharged: credits}, nil
}
