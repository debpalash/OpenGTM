package leadweb

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"
)

const specstreamEndpoint = "https://api.specstream.ai/v1/web-search"
const specstreamMaxBody = 2 << 20

func specstreamSearch(ctx context.Context, e *KeyedEngines, query string, n int) []Hit {
	// The API permits 1..100 results and at most 2000 query characters.
	if n <= 0 || strings.TrimSpace(query) == "" || !utf8.ValidString(query) || utf8.RuneCountInString(query) > 2000 {
		return nil
	}
	n = min(n, 100)
	endpoint := specstreamEndpoint
	if override := e.Endpoints["specstream"]; override != "" {
		endpoint = override
	}
	// The fallback reads titles, URLs and snippets only, so it asks for the fast tier.
	_, raw, err := postProvider(ctx, e.hc, providerRequest{
		endpoint: endpoint, key: e.cfg.SpecStreamKey, timeout: e.cfg.EngineTimeout,
		accept: "application/json", maxBytes: specstreamMaxBody,
		query: encodeParams([]param{{"q", query}, {"num", strconv.Itoa(n)}, {"speed", "fast"}}),
	})
	if err != nil {
		return nil
	}
	var data map[string]any
	if json.Unmarshal(raw, &data) != nil || str(data, "request_id") == "" {
		return nil
	}
	var hits []Hit
	for _, item := range jlist(data, "sources") {
		row, ok := item.(map[string]any)
		if !ok {
			continue
		}
		href := str(row, "url")
		if !publicPageURL(href) {
			continue
		}
		hits = append(hits, Hit{Title: str(row, "title"), Href: href, Body: str(row, "snippet"), Date: str(row, "published")})
		if len(hits) == n {
			break
		}
	}
	return hits
}
