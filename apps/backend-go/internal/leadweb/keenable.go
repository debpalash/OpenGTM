package leadweb

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// Keenable search (https://docs.keenable.ai/api-reference/search). Opt-in: the
// engine is configured only with KEENABLE_API_KEY (keyed endpoint) or an
// explicit KEENABLE_ENABLED=true (keyless public endpoint). Otherwise it never
// sends a request.
const (
	keenableKeyedEndpoint  = "https://api.keenable.ai/v1/search"
	keenablePublicEndpoint = "https://api.keenable.ai/v1/search/public"
	keenableMaxBody        = 2 << 20
)

func keenableSearch(ctx context.Context, e *KeyedEngines, query string, n int) []Hit {
	// The API permits 1..50 results; the query cap mirrors the other engines.
	if n <= 0 || strings.TrimSpace(query) == "" || !utf8.ValidString(query) || utf8.RuneCountInString(query) > 500 {
		return nil
	}
	if e.cfg.KeenableKey == "" && !e.cfg.KeenableEnabled {
		return nil
	}
	n = min(n, 50)
	req := providerRequest{
		endpoint: keenableKeyedEndpoint, key: e.cfg.KeenableKey, timeout: e.cfg.EngineTimeout,
		accept: "application/json", maxBytes: keenableMaxBody,
		body: map[string]any{"query": query, "max_results": n},
	}
	if e.cfg.KeenableKey == "" { // explicit keyless mode: no credential, required app identifier
		req.endpoint, req.keyless = keenablePublicEndpoint, true
		req.headers = map[string]string{"X-Keenable-Title": "OpenGTM"}
	}
	if override := e.Endpoints["keenable"]; override != "" {
		req.endpoint = override
	}
	_, raw, err := postProvider(ctx, e.hc, req)
	if err != nil {
		return nil
	}
	var data map[string]any
	if json.Unmarshal(raw, &data) != nil {
		return nil
	}
	var hits []Hit
	for _, item := range jlist(data, "results") {
		row, ok := item.(map[string]any)
		if !ok {
			continue
		}
		href := str(row, "url")
		if !publicPageURL(href) {
			continue
		}
		hits = append(hits, Hit{Title: str(row, "title"), Href: href, Body: str(row, "snippet")})
		if len(hits) == n {
			break
		}
	}
	return hits
}
