package leadweb

import (
	"context"
	"errors"
	"net/url"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/browserclient"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
)

// FetchResult is one page fetch (legacy leadgen/http.py FetchResult).
// TierUsed: 2 TLS-impersonating HTTP, 3 headless browser, 4 FlareSolverr.
type FetchResult struct {
	URL        string
	StatusCode int
	Text       string
	TierUsed   int
	ProxyUsed  string // may carry credentials: never log
	Error      string
}

// OK is a 2xx/3xx answer without an error.
func (r FetchResult) OK() bool { return r.StatusCode >= 200 && r.StatusCode < 400 && r.Error == "" }

// Emails is FetchEmails over the body.
func (r FetchResult) Emails() []string { return FetchEmails(r.Text) }

// Phones is FetchPhones over the body.
func (r FetchResult) Phones() []string { return FetchPhones(r.Text) }

// Fetcher is the legacy StealthClient: per-domain rate limiting and circuit
// breaker, PROXY_LIST rotation, then the browser service's impersonate
// engine, escalating to a real browser on a challenge / JS-only page and to
// FlareSolverr when still challenged. Every URL is checked with the Go egress
// guard first; the service re-checks every hop.
type Fetcher struct {
	browser Browser
	limiter *RateLimiter
	pool    *ProxyPool
	appEnv  string
	allow   []string
}

// FetchOptions: Tier <= 2 (default) starts with HTTP and escalates, >= 3
// goes straight to the browser. Timeout zero is 15 s (the browser tier gets
// 10 s more).
type FetchOptions struct {
	Tier    int
	NoProxy bool
	Timeout time.Duration
}

// NewFetcher wires a fetcher. allowHosts are the bench/test egress allow-list
// (honoured only when appEnv is test or bench).
func NewFetcher(b Browser, limiter *RateLimiter, pool *ProxyPool, appEnv string, allowHosts []string) *Fetcher {
	return &Fetcher{browser: b, limiter: limiter, pool: pool, appEnv: appEnv, allow: allowHosts}
}

// Fetch fetches rawURL; it never returns an error, the result carries it.
func (f *Fetcher) Fetch(ctx context.Context, rawURL string, o FetchOptions) FetchResult {
	res := FetchResult{URL: rawURL}
	if err := egress.CheckURLFor(f.appEnv, f.allow, rawURL, true); err != nil {
		res.Error = "blocked url"
		return res
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		res.Error = "blocked url"
		return res
	}
	domain := u.Host
	tier := o.Tier
	if tier == 0 {
		tier = 2
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	if f.limiter.IsBlocked(domain) {
		res.Error = "Circuit breaker tripped for " + domain
		return res
	}
	if err := f.limiter.Acquire(ctx, domain); err != nil {
		res.Error = "cancelled"
		return res
	}
	proxy := ""
	if !o.NoProxy && f.pool != nil && f.pool.Len() > 0 {
		proxy = f.pool.Get(domain, tier)
	}
	if tier <= 2 {
		res = f.render(ctx, rawURL, "impersonate", 2, proxy, timeout)
		if res.OK() && !IsChallenge(res.StatusCode, res.Text) {
			f.reportOK(domain, proxy)
			return res
		}
		if IsChallenge(res.StatusCode, res.Text) {
			bproxy := proxy
			if proxy != "" {
				bproxy = f.pool.Get(domain, 3)
			}
			br := f.render(ctx, rawURL, "browser", 3, bproxy, timeout+10*time.Second)
			if br.OK() || res.Text == "" {
				res = br
			}
			proxy = bproxy
		}
	} else {
		res = f.render(ctx, rawURL, "browser", 3, proxy, timeout+10*time.Second)
	}
	if !res.OK() && IsChallenge(res.StatusCode, res.Text) {
		if fs := f.render(ctx, rawURL, "flaresolverr", 4, proxy, timeout+10*time.Second); fs.Error == "" {
			if fs.StatusCode == 0 {
				fs.StatusCode = 200
			}
			res = fs
		}
	}
	if res.OK() {
		f.reportOK(domain, proxy)
	} else {
		f.limiter.ReportFailure(domain)
		if proxy != "" {
			f.pool.ReportBlocked(proxy, domain)
		}
	}
	return res
}

func (f *Fetcher) reportOK(domain, proxy string) {
	f.limiter.ReportSuccess(domain)
	if proxy != "" {
		f.pool.ReportSuccess(proxy, domain)
	}
}

func (f *Fetcher) render(ctx context.Context, rawURL, engine string, tier int, proxy string, timeout time.Duration) FetchResult {
	res := FetchResult{URL: rawURL, TierUsed: tier, ProxyUsed: proxy}
	out, err := f.browser.Render(ctx, browserclient.RenderRequest{URL: rawURL, Engine: engine, IncludeHTML: true,
		UpstreamProxy: browserclient.ProxyURL(proxy), Timeout: timeout})
	if err != nil {
		var be *browserclient.Error
		if errors.As(err, &be) {
			res.Error = be.Code
		} else {
			res.Error = "unavailable"
		}
		return res
	}
	res.StatusCode, res.Text = out.Status, out.HTML
	if tier == 4 && out.FinalURL != "" {
		res.URL = out.FinalURL // legacy: the FlareSolverr solution URL; other tiers keep the requested URL
	}
	return res
}
