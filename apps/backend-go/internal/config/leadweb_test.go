package config

import (
	"slices"
	"testing"
	"time"
)

func TestLeadwebDefaults(t *testing.T) {
	s, err := LeadwebFrom(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if s.SearchBackends != "duckduckgo, google, bing, brave, mojeek, yahoo" || !slices.Equal(s.SearchAttemptOrder, []string{"direct", "proxy"}) ||
		!slices.Equal(s.FallbackOrder, []string{"serpapi", "bing", "google_cse", "brave", "serpingapi", "serply", "firecrawl"}) ||
		s.DDGSTimeout != 10*time.Second || s.EngineTimeout != 10*time.Second || !s.CacheEnabled || s.CacheTTL != 600*time.Second ||
		s.CacheEmptyTTL != 60*time.Second || s.CacheMaxEntries != 2000 || !s.BackoffEnabled || s.BackoffBase != 15*time.Second ||
		s.BackoffCap != 900*time.Second || s.ProxyManagerURL.Reveal() != "http://localhost:3050" || s.ProxyList != "" ||
		s.SearxPoolURL != "http://localhost:8889" || s.SearxngURL != "http://localhost:8888" || s.SearxEngines != "bing,qwant,yahoo" ||
		s.SearxDisabled || s.SearxTimeout != 12*time.Second || s.GMapsScraperURL != "" {
		t.Fatalf("defaults: %+v", s)
	}
}

func TestLeadwebOverrides(t *testing.T) {
	env := map[string]string{
		"SEARCH_BACKENDS": "auto", "SEARCH_ATTEMPT_ORDER": " proxy , direct,", "SEARCH_FALLBACK_ORDER": "none",
		"SEARCH_CACHE_TTL": "1.5", "SEARCH_CACHE_MAX_ENTRIES": "-3", "SEARCH_BACKOFF_ENABLED": "off",
		"BRAVE_SEARCH_KEY": " k ", "PROXY_LIST": "1.1.1.1:80", "SEARXNG_DISABLE": "YES", "SEARXPOOL_URL": "http://p:1/",
		"GMAPS_SCRAPER_URL": " http://gmaps:8080/ ",
	}
	s, err := LeadwebFrom(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if s.SearchBackends != "" || !slices.Equal(s.SearchAttemptOrder, []string{"proxy", "direct"}) || s.FallbackOrder != nil ||
		s.CacheTTL != 1500*time.Millisecond || s.CacheMaxEntries != 1 || s.BackoffEnabled || s.BraveKey.Reveal() != "k" ||
		s.ProxyList.Reveal() != "1.1.1.1:80" || !s.SearxDisabled || s.SearxPoolURL != "http://p:1" || s.GMapsScraperURL != "http://gmaps:8080" {
		t.Fatalf("overrides: %+v", s)
	}
	for _, bad := range []map[string]string{{"DDGS_TIMEOUT": "x"}, {"SEARCH_CACHE_MAX_ENTRIES": "1.5"}, {"SEARCH_CACHE_ENABLED": "maybe"}, {"SEARCH_ENGINE_TIMEOUT": "-1"}} {
		if _, err := LeadwebFrom(func(k string) string { return bad[k] }); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

func TestSpicrawlTimeoutMustBeFiniteAndBounded(t *testing.T) {
	for _, value := range []string{"NaN", "+Inf", "-Inf", "-1", "86401", "invalid"} {
		if _, err := LeadwebFrom(func(k string) string {
			if k == "SPICRAWL_TIMEOUT" {
				return value
			}
			return ""
		}); err == nil {
			t.Errorf("invalid Spicrawl timeout %q accepted", value)
		}
	}
}
