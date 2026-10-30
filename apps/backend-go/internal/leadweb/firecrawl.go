package leadweb

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

const firecrawlEndpoint = "https://api.firecrawl.dev/v2/search"
const firecrawlMaxBody = 2 << 20

func firecrawlSearch(ctx context.Context, e *KeyedEngines, query string, n int) []Hit {
	// The v2 API permits 1..100 results and at most 500 query characters.
	if n <= 0 || strings.TrimSpace(query) == "" || !utf8.ValidString(query) || utf8.RuneCountInString(query) > 500 {
		return nil
	}
	n = min(n, 100)
	endpoint := firecrawlEndpoint
	if override := e.Endpoints["firecrawl"]; override != "" {
		endpoint = override
	}
	_, raw, err := postProvider(ctx, e.hc, providerRequest{
		endpoint: endpoint, key: e.cfg.FirecrawlKey, timeout: e.cfg.EngineTimeout,
		accept: "application/json", maxBytes: firecrawlMaxBody,
		body: map[string]any{"query": query, "limit": n, "highlights": false,
			"origin": "opengtm", "sources": []map[string]string{{"type": "web"}}},
	})
	if err != nil {
		return nil
	}
	var data map[string]any
	if json.Unmarshal(raw, &data) != nil || data["success"] != true {
		return nil
	}
	var hits []Hit
	for _, item := range jlist(obj(data, "data"), "web") {
		row, ok := item.(map[string]any)
		if !ok {
			continue
		}
		href := str(row, "url")
		if !publicPageURL(href) {
			continue
		}
		hits = append(hits, Hit{Title: str(row, "title"), Href: href, Body: str(row, "description")})
		if len(hits) == n {
			break
		}
	}
	return hits
}
