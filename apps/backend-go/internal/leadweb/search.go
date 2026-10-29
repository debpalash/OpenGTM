package leadweb

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/browserclient"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
)

// Browser is the part of *browserclient.Client leadweb uses (fakes in tests).
type Browser interface {
	Render(ctx context.Context, req browserclient.RenderRequest) (browserclient.RenderResult, error)
	Search(ctx context.Context, req browserclient.SearchRequest) (browserclient.SearchOut, error)
}

// ddgsHost is the backoff key of the keyless (ddgs) search, as legacy.
const ddgsHost = "ddgs"

// Searcher is the legacy resilient search (leadgen/proxy_client.py
// _ResilientDDGS): cached, keyless multi-engine ddgs through the browser
// service over the SEARCH_ATTEMPT_ORDER connections (direct, then a proxy
// from the proxy manager), per-host backoff, then the keyed engines.
type Searcher struct {
	cfg     config.LeadwebSettings
	browser Browser
	cache   *Cache
	backoff *Backoff
	keyed   *KeyedEngines
	proxies *ProxyManager
	searx   *Searx
}

// SearchOptions are per call. Workspace salts the cache key (legacy tenant
// salt: a free-form query never leaks across workspaces). Proxy overrides the
// proxy-manager pick; NoProxy skips the proxy attempt.
type SearchOptions struct {
	MaxResults int // 0 = 10 (ddgs default)
	Workspace  string
	Proxy      browserclient.ProxyURL
	NoProxy    bool
}

// KeyedConfigured reports whether an opt-in API fallback can search without
// the browser sidecar. It does not expose keys or configuration details.
func (s *Searcher) KeyedConfigured() bool {
	return s != nil && s.keyed != nil && len(s.keyed.Configured()) > 0
}

// Text is a web search; news is News. Failures degrade to an empty result.
func (s *Searcher) Text(ctx context.Context, query string, o SearchOptions) []Hit {
	return s.run(ctx, "text", query, o)
}

// News is a news search (no keyed fallback, as legacy).
func (s *Searcher) News(ctx context.Context, query string, o SearchOptions) []Hit {
	return s.run(ctx, "news", query, o)
}

func backendList(b string) []string {
	out := []string{}
	for _, p := range strings.Split(b, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (s *Searcher) attempts(proxy browserclient.ProxyURL) []browserclient.ProxyURL {
	var seq []browserclient.ProxyURL
	for _, mode := range s.cfg.SearchAttemptOrder {
		switch mode {
		case "direct":
			seq = append(seq, "")
		case "proxy":
			if proxy != "" {
				seq = append(seq, proxy)
			}
		}
	}
	if len(seq) == 0 {
		seq = []browserclient.ProxyURL{""}
	}
	return seq
}

func (s *Searcher) run(ctx context.Context, method, query string, o SearchOptions) []Hit {
	n := o.MaxResults
	if n <= 0 {
		n = 10
	}
	key := CacheKey(o.Workspace, method, query, n, s.cfg.SearchBackends)
	if s.cfg.CacheEnabled {
		if hits, ok := s.cache.Get(key); ok {
			return hits
		}
	}
	proxy := o.Proxy
	if proxy == "" && !o.NoProxy {
		proxy = browserclient.ProxyURL(s.proxies.Get(ctx, ""))
	}
	if !s.backoff.Blocked(ddgsHost) {
		for _, attempt := range s.attempts(proxy) {
			hits, failed, block := s.ddgs(ctx, method, query, n, attempt)
			if len(hits) > 0 {
				if attempt != "" {
					s.proxies.Report(ctx, string(attempt), true)
				}
				s.backoff.RecordSuccess(ddgsHost)
				if s.cfg.CacheEnabled {
					s.cache.Put(key, hits, false)
				}
				return hits
			}
			if failed {
				s.backoff.Record(ddgsHost, block)
				if attempt != "" {
					s.proxies.Report(ctx, string(attempt), false)
				}
			}
		}
	}
	if method == "text" && s.keyed != nil {
		if hits := s.keyed.Search(ctx, query, n); len(hits) > 0 {
			if s.cfg.CacheEnabled {
				s.cache.Put(key, hits, false)
			}
			return hits
		}
	}
	if s.cfg.CacheEnabled {
		s.cache.Put(key, nil, true)
	}
	return nil
}

// ddgs is one keyless search over one connection. failed means ddgs raised
// (legacy: the proxy is reported bad and block signals grow the backoff);
// an empty answer is no failure. A browser-service outage is not the
// proxy's fault and not a block.
func (s *Searcher) ddgs(ctx context.Context, method, query string, n int,
	proxy browserclient.ProxyURL) (hits []Hit, failed, block bool) {
	out, err := s.browser.Search(ctx, browserclient.SearchRequest{
		Query: query, Method: method, MaxResults: n, Backends: backendList(s.cfg.SearchBackends),
		UpstreamProxy: proxy, Timeout: 3 * max(s.cfg.DDGSTimeout, time.Second),
	})
	if err != nil {
		return nil, false, false
	}
	switch out.Error {
	case "":
		return toHits(out.Hits), false, false
	case "rate_limited", "timeout":
		return nil, true, true
	default: // no_results (ddgs raises on empty), failed
		return nil, true, false
	}
}

func toHits(hs []browserclient.SearchHit) []Hit {
	out := make([]Hit, 0, len(hs))
	for _, h := range hs {
		out = append(out, Hit{Title: h.Title, Href: h.URL, Body: h.Body, Date: h.Date, Source: h.Source})
	}
	return out
}

// ErrSearchFailed is returned by WebSearch with raiseErrors when every
// backend failed (legacy raise_errors=True re-raised the ddgs exception).
var ErrSearchFailed = errors.New("leadweb: search failed")

// WebSearch is the legacy enrichment web_search stand-in used by providers:
// SearXNG pool, then direct SearXNG, then plain ddgs (its default backends,
// direct, no cache). With raiseErrors a ddgs failure is an error instead of
// an empty result.
func (s *Searcher) WebSearch(ctx context.Context, query string, maxResults int, raiseErrors bool) ([]Hit, error) {
	if maxResults <= 0 {
		maxResults = 10
	}
	if hits := s.searx.Search(ctx, query, maxResults); len(hits) > 0 {
		return hits, nil
	}
	out, err := s.browser.Search(ctx, browserclient.SearchRequest{Query: query, MaxResults: maxResults,
		Backends: []string{}, Timeout: 30 * time.Second})
	if err == nil && out.Error == "" {
		return toHits(out.Hits), nil
	}
	if raiseErrors {
		return nil, ErrSearchFailed
	}
	return nil, nil
}
