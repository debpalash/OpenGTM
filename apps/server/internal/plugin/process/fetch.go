//go:build unix

package process

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/server/internal/egress"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/manifest"
)

func fetchErr(id int64, code, msg string) FetchResult {
	return FetchResult{Type: TypeFetchResult, ID: id, Error: &FetchError{Code: code, Message: msg}}
}

// doFetch performs one plugin fetch through the shared egress client, with the
// same enforcement as the WebAssembly host function: the manifest's network
// capability on the request and every redirect hop, the destination guard and
// DNS pinning, robots.txt for scrapers, per-domain rate limits, size caps and
// the fetch budget (limits.max_pages). Secret values never appear in what is
// returned to the plugin or recorded as evidence.
func (r *runner) doFetch(ctx context.Context, f Fetch) FetchResult {
	client := r.s.opts.Client
	if client == nil {
		return fetchErr(f.ID, "network_unavailable", "this host has no egress client")
	}
	p := r.p
	r.fmu.Lock()
	over := len(r.fetches) >= p.Limits.MaxPages
	r.fmu.Unlock()
	if over {
		return fetchErr(f.ID, "too_many_fetches", "fetch budget for this run is exhausted (limits.max_pages)")
	}
	if err := p.Network().Allows(f.URL); err != nil {
		return fetchErr(f.ID, "capability_denied", r.redact(err.Error()))
	}
	method := f.Method
	if method == "" {
		method = "GET"
	}
	body := []byte(f.Body)
	if f.BodyBase64 != "" {
		b, err := base64.StdEncoding.DecodeString(f.BodyBase64)
		if err != nil {
			return fetchErr(f.ID, "bad_request", "body_base64 is not valid base64")
		}
		body = b
	}
	if len(body) == 0 {
		body = nil
	}
	hdr := http.Header{}
	sensitive := make([]string, 0, len(f.Headers))
	for k, v := range f.Headers {
		hdr.Set(k, v)
		sensitive = append(sensitive, k) // plugin headers never follow a cross-origin redirect
	}
	// A response must fit one frame after JSON (and possibly base64) encoding.
	maxBody := p.Limits.MaxResponseBytes
	if frameCap := int64(r.maxFrm-(64<<10)) / 4 * 3; maxBody > frameCap {
		maxBody = frameCap
	}
	resp, err := client.Do(ctx, egress.Request{
		Method:           method,
		URL:              f.URL,
		Header:           hdr,
		Body:             body,
		Robots:           p.Kind == "scraper" || p.Kind == "signal",
		RPS:              p.Limits.RequestsPerSecondPerDomain,
		MaxBodyBytes:     maxBody,
		Allow:            p.Network().AllowsURL,
		SensitiveHeaders: sensitive,
	})
	if err != nil {
		return fetchErr(f.ID, classifyFetch(err), r.redact(err.Error()))
	}
	ev := FetchEvidence{
		URL: r.redact(resp.Evidence.URL), Status: resp.Status,
		FetchedAt:   resp.Evidence.FetchedAt.UTC().Format(time.RFC3339Nano),
		ContentType: resp.Evidence.ContentType, Bytes: resp.Evidence.Bytes, SHA256: resp.Evidence.SHA256,
	}
	r.fmu.Lock()
	r.fetches = append(r.fetches, ev)
	r.fmu.Unlock()
	out := FetchResult{Type: TypeFetchResult, ID: f.ID, Status: resp.Status, Headers: map[string]string{}, Evidence: &ev}
	for k := range resp.Header {
		out.Headers[strings.ToLower(k)] = resp.Header.Get(k)
	}
	if utf8.Valid(resp.Body) {
		out.Body = string(resp.Body)
	} else {
		out.BodyBase64 = base64.StdEncoding.EncodeToString(resp.Body)
	}
	return out
}

func classifyFetch(err error) string {
	var be *egress.BlockedError
	var nd *manifest.ErrNetworkDenied
	var rl *egress.RateLimitError
	switch {
	case errors.As(err, &nd):
		return "capability_denied"
	case errors.As(err, &be):
		return "blocked_url"
	case errors.Is(err, egress.ErrRobotsDisallowed), errors.Is(err, egress.ErrRobotsUnavailable):
		return "robots_disallowed"
	case errors.Is(err, egress.ErrBodyTooLarge):
		return "body_too_large"
	case errors.As(err, &rl):
		return "rate_limited"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	return "fetch_failed"
}
