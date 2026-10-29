package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// LeadwebSettings configure internal/leadweb (plan 15 WP-WEB): the Go side of
// web search and page fetching over the browser service. Names and defaults
// are the legacy Python ones (leadgen/proxy_client.py, search_engines.py,
// search_cache.py, enrichment/web_search.py, scrapers/gmaps_service_client.py).
// leadweb is a library; it has no enabling flag (its consumers are gated).
type LeadwebSettings struct {
	// keyless search through the browser service (ddgs)
	SearchBackends     string        // SEARCH_BACKENDS (default "duckduckgo, google, bing, brave, mojeek, yahoo"; "auto" = ddgs default)
	SearchAttemptOrder []string      // SEARCH_ATTEMPT_ORDER (default direct,proxy)
	DDGSTimeout        time.Duration // DDGS_TIMEOUT seconds (default 10): the service-side search deadline

	// keyed fallback engines, called from Go directly (keys never reach the browser service)
	FallbackOrder []string      // SEARCH_FALLBACK_ORDER (default serpapi,bing,google_cse,brave,serpingapi,serply,firecrawl; "none" = no keyed engines)
	EngineTimeout time.Duration // SEARCH_ENGINE_TIMEOUT seconds (default 10)
	SerpAPIKey    Secret        // SERPAPI_KEY
	BingKey       Secret        // BING_SEARCH_KEY
	GoogleCSEID   Secret        // GOOGLE_CSE_ID
	GoogleCSEKey  Secret        // GOOGLE_CSE_KEY
	BraveKey      Secret        // BRAVE_SEARCH_KEY
	SerpingAPIKey Secret        // SERPINGAPI_API_KEY
	SerplyKey     Secret        // SERPLY_API_KEY
	FirecrawlKey  Secret        // FIRECRAWL_API_KEY

	// result cache and per-host backoff (process-local, as legacy)
	CacheEnabled    bool          // SEARCH_CACHE_ENABLED (default true)
	CacheTTL        time.Duration // SEARCH_CACHE_TTL seconds (default 600)
	CacheEmptyTTL   time.Duration // SEARCH_CACHE_EMPTY_TTL seconds (default 60)
	CacheMaxEntries int           // SEARCH_CACHE_MAX_ENTRIES (default 2000, min 1)
	BackoffEnabled  bool          // SEARCH_BACKOFF_ENABLED (default true)
	BackoffBase     time.Duration // SEARCH_BACKOFF_BASE seconds (default 15)
	BackoffCap      time.Duration // SEARCH_BACKOFF_CAP seconds (default 900)

	// upstream proxies: chosen in Go, passed per request to the browser service
	ProxyManagerURL Secret // PROXY_MANAGER_URL (default http://localhost:3050); may carry credentials
	ProxyList       Secret // PROXY_LIST: comma-separated host:port / user:pass@host:port / scheme://...

	// self-hosted SearXNG (enrichment web_search)
	SearxPoolURL  string        // SEARXPOOL_URL (default http://localhost:8889)
	SearxngURL    string        // SEARXNG_URL (default http://localhost:8888)
	SearxEngines  string        // SEARXPOOL_DEFAULT_ENGINES (default bing,qwant,yahoo)
	SearxDisabled bool          // SEARXNG_DISABLE (1/true/yes)
	SearxTimeout  time.Duration // SEARXNG_HTTP_TIMEOUT seconds (default 12)

	// gosom/google-maps-scraper REST sidecar
	GMapsScraperURL string // GMAPS_SCRAPER_URL (empty = off)
}

const (
	defaultSearchBackends = "duckduckgo, google, bing, brave, mojeek, yahoo"
	defaultFallbackOrder  = "serpapi,bing,google_cse,brave,serpingapi,serply,firecrawl"
)

// LeadwebFrom reads the leadweb settings. Unlike the Python readers (which
// fell back to the default on a malformed number), a malformed value is an
// error, so a typo is noticed at start.
func LeadwebFrom(get func(string) string) (LeadwebSettings, error) {
	s := LeadwebSettings{
		SearchBackends:  defaultSearchBackends,
		DDGSTimeout:     10 * time.Second,
		EngineTimeout:   10 * time.Second,
		CacheEnabled:    true,
		CacheTTL:        600 * time.Second,
		CacheEmptyTTL:   60 * time.Second,
		CacheMaxEntries: 2000,
		BackoffEnabled:  true,
		BackoffBase:     15 * time.Second,
		BackoffCap:      900 * time.Second,
		ProxyManagerURL: "http://localhost:3050",
		SearxPoolURL:    "http://localhost:8889",
		SearxngURL:      "http://localhost:8888",
		SearxEngines:    "bing,qwant,yahoo",
		SearxTimeout:    12 * time.Second,
	}
	raw := func(k string) (string, bool) {
		v := get(k)
		return v, v != ""
	}
	if v, ok := raw("SEARCH_BACKENDS"); ok {
		s.SearchBackends = v
	}
	s.SearchAttemptOrder = splitList(get("SEARCH_ATTEMPT_ORDER"), "direct,proxy")
	// Python told an empty value from an unset one ("" = ddgs default / no
	// keyed engines); get() cannot, so those are spelled "auto" and "none".
	if strings.EqualFold(strings.TrimSpace(s.SearchBackends), "auto") {
		s.SearchBackends = ""
	}
	s.FallbackOrder = splitList(get("SEARCH_FALLBACK_ORDER"), defaultFallbackOrder)
	if len(s.FallbackOrder) == 1 && strings.EqualFold(s.FallbackOrder[0], "none") {
		s.FallbackOrder = nil
	}
	var err error
	for _, d := range []struct {
		key string
		dst *time.Duration
		min float64
	}{
		{"DDGS_TIMEOUT", &s.DDGSTimeout, 1},
		{"SEARCH_ENGINE_TIMEOUT", &s.EngineTimeout, 0.1},
		{"SEARCH_CACHE_TTL", &s.CacheTTL, 0},
		{"SEARCH_CACHE_EMPTY_TTL", &s.CacheEmptyTTL, 0},
		{"SEARCH_BACKOFF_BASE", &s.BackoffBase, 0},
		{"SEARCH_BACKOFF_CAP", &s.BackoffCap, 0},
		{"SEARXNG_HTTP_TIMEOUT", &s.SearxTimeout, 0.1},
	} {
		if err = secondsVar(get, d.key, d.min, d.dst); err != nil {
			return s, err
		}
	}
	if v, ok := raw("SEARCH_CACHE_MAX_ENTRIES"); ok {
		n, perr := strconv.Atoi(strings.TrimSpace(v))
		if perr != nil {
			return s, fmt.Errorf("config: SEARCH_CACHE_MAX_ENTRIES=%q: not an integer", v)
		}
		s.CacheMaxEntries = max(1, n)
	}
	if s.CacheEnabled, err = boolVar(get, "SEARCH_CACHE_ENABLED", true); err != nil {
		return s, err
	}
	if s.BackoffEnabled, err = boolVar(get, "SEARCH_BACKOFF_ENABLED", true); err != nil {
		return s, err
	}
	for _, k := range []struct {
		key string
		dst *Secret
	}{
		{"SERPAPI_KEY", &s.SerpAPIKey}, {"BING_SEARCH_KEY", &s.BingKey}, {"GOOGLE_CSE_ID", &s.GoogleCSEID},
		{"GOOGLE_CSE_KEY", &s.GoogleCSEKey}, {"BRAVE_SEARCH_KEY", &s.BraveKey},
		{"SERPINGAPI_API_KEY", &s.SerpingAPIKey}, {"SERPLY_API_KEY", &s.SerplyKey},
		{"FIRECRAWL_API_KEY", &s.FirecrawlKey},
	} {
		*k.dst = Secret(strings.TrimSpace(get(k.key)))
	}
	if v, ok := raw("PROXY_MANAGER_URL"); ok {
		s.ProxyManagerURL = Secret(strings.TrimSpace(v))
	}
	s.ProxyList = Secret(get("PROXY_LIST"))
	if v, ok := raw("SEARXPOOL_URL"); ok {
		s.SearxPoolURL = v
	}
	if v, ok := raw("SEARXNG_URL"); ok {
		s.SearxngURL = v
	}
	s.SearxPoolURL = strings.TrimRight(s.SearxPoolURL, "/")
	s.SearxngURL = strings.TrimRight(s.SearxngURL, "/")
	if v, ok := raw("SEARXPOOL_DEFAULT_ENGINES"); ok {
		s.SearxEngines = v
	}
	switch strings.ToLower(get("SEARXNG_DISABLE")) {
	case "1", "true", "yes":
		s.SearxDisabled = true
	}
	s.GMapsScraperURL = strings.TrimRight(strings.TrimSpace(get("GMAPS_SCRAPER_URL")), "/")
	return s, nil
}

// splitList is the legacy `[s.strip() for s in raw.split(",") if s.strip()]`
// with def used when the variable is unset (an empty value gives an empty list).
func splitList(raw, def string) []string {
	if raw == "" {
		raw = def
	}
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func secondsVar(get func(string) string, key string, minV float64, dst *time.Duration) error {
	v := strings.TrimSpace(get(key))
	if v == "" {
		return nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < minV || f > 86400 {
		return fmt.Errorf("config: %s=%q: want seconds in [%g, 86400]", key, v, minV)
	}
	*dst = time.Duration(f * float64(time.Second))
	return nil
}
