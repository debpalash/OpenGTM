// Package leadweb is the Go web layer over the browser service (plan 15
// WP-WEB): keyless search through the service with the legacy cache,
// per-host backoff, connection fallback and keyed engines; page fetching
// with rate limiting, proxy rotation and tier escalation; the gosom Maps
// sidecar client; and the HTML helpers (emails, phones, links, meta) the
// lead pipeline used from Python. Page work runs in the browser service; all
// parsing, keys and proxy choice stay in Go.
//
// State (cache, backoff, rate limiter, proxy rotation) is per process, as it
// was per Python process: each api / worker replica has its own (R6).
package leadweb

import (
	"context"
	"net/http"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/browserclient"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
)

// Web bundles the process-wide pieces.
type Web struct {
	Search  *Searcher
	Fetch   *Fetcher
	GMaps   *GMaps
	Limiter *RateLimiter
	Pool    *ProxyPool
	Proxies *ProxyManager
	browser *browserclient.Client
}

// New wires leadweb from settings. bc is the browser-service client; appEnv
// and allowHosts are the egress bench/test allow-list (config.AppEnv,
// EGRESS_ALLOW_HOSTS) for public-host clients and the URL pre-check.
func New(cfg config.LeadwebSettings, bc *browserclient.Client, appEnv string, allowHosts []string) *Web {
	return newWeb(cfg, bc, bc, egress.NewClientForEnv(appEnv, allowHosts), appEnv, allowHosts)
}

func newWeb(cfg config.LeadwebSettings, b Browser, bc *browserclient.Client, public *http.Client,
	appEnv string, allowHosts []string) *Web {
	backoff := NewBackoff(cfg.BackoffEnabled, cfg.BackoffBase, cfg.BackoffCap)
	limiter := NewRateLimiter()
	pool := NewProxyPool(cfg.ProxyList.Reveal())
	mgr := NewProxyManager(cfg.ProxyManagerURL.Reveal(), nil)
	s := &Searcher{
		cfg:     cfg,
		browser: b,
		cache:   NewCache(cfg.CacheTTL, cfg.CacheEmptyTTL, cfg.CacheMaxEntries),
		backoff: backoff,
		keyed:   NewKeyedEngines(cfg, public, backoff),
		proxies: mgr,
		searx:   NewSearx(cfg, nil),
	}
	return &Web{
		Search: s, Fetch: NewFetcher(b, limiter, pool, appEnv, allowHosts), GMaps: NewGMaps(cfg.GMapsScraperURL, nil),
		Limiter: limiter, Pool: pool, Proxies: mgr, browser: bc,
	}
}

// SystemStats is the legacy /system-stats proxy_pool + rate_limiter shape,
// plus the browser service's own counters when it answers. Deviation (plan 15
// D5): legacy built fresh objects per call (always empty counters); these are
// this process's live counters.
type SystemStats struct {
	ProxyPool   PoolStats            `json:"proxy_pool"`
	RateLimiter LimiterStats         `json:"rate_limiter"`
	Browser     *browserclient.Stats `json:"browser,omitempty"`
}

// Stats collects SystemStats; the browser part is best-effort.
func (w *Web) Stats(ctx context.Context) SystemStats {
	st := SystemStats{ProxyPool: w.Pool.Stats(), RateLimiter: w.Limiter.Stats()}
	if w.browser.Configured() {
		if bs, err := w.browser.Stats(ctx); err == nil {
			st.Browser = &bs
		}
	}
	return st
}
