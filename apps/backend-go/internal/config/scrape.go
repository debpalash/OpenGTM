package config

import "strings"

// ScrapeRoutesFrom reads GO_SCRAPE_ROUTES_ENABLED (plan 15 WP-ROUTES-B): serve the browser-backed and registry
// routes from Go (GET /api/v2/search/unified, POST /api/v2/scraper/scrape, POST /api/v2/scrape, GET /api/v2/scrapes)
// and answer GET /api/system-stats from this process plus the browser service instead of the Python worker RPC.
// Off by default (the Python path); WP-CUTOVER flips it. A value that is not a boolean is an error.
func ScrapeRoutesFrom(get func(string) string) (bool, error) {
	return boolVar(get, "GO_SCRAPE_ROUTES_ENABLED", false)
}

// ScrapeSettings configure the scrape and unified-search routes.
type ScrapeSettings struct {
	// browser service (internal network only; the bearer token is shared with the worker)
	BrowserURL   string
	BrowserToken Secret

	// keyed document sources (legacy sources/google_search.py, sources/legacy.py); all optional
	GoogleAPIKey   Secret // GOOGLE_API_KEY
	GoogleCSEID    Secret // GOOGLE_CSE_ID
	GoogleBooksKey Secret // GOOGLE_BOOKS_API_KEY
}

// ScrapeFrom reads the scrape settings.
func ScrapeFrom(get func(string) string) ScrapeSettings {
	t := func(k string) string { return strings.TrimSpace(get(k)) }
	return ScrapeSettings{
		BrowserURL:     t("BROWSER_SERVICE_URL"),
		BrowserToken:   Secret(t("BROWSER_SERVICE_TOKEN")),
		GoogleAPIKey:   Secret(t("GOOGLE_API_KEY")),
		GoogleCSEID:    Secret(t("GOOGLE_CSE_ID")),
		GoogleBooksKey: Secret(t("GOOGLE_BOOKS_API_KEY")),
	}
}
