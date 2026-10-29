package leadweb

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
)

// Searx is the self-hosted SearXNG path of the legacy enrichment web_search
// stand-in: the pool gateway (SEARXPOOL_URL), then a direct SearXNG
// (SEARXNG_URL). Both are operator-configured internal hosts, allow-listed
// explicitly on the egress client.
type Searx struct {
	pool, direct string
	engines      string
	disabled     bool
	timeout      time.Duration
	hc           *http.Client
}

// NewSearx builds the client; hc nil builds the allow-listed egress client.
func NewSearx(cfg config.LeadwebSettings, hc *http.Client) *Searx {
	if hc == nil {
		hc = egress.NewOperatorClient(append(allowHostsFor(cfg.SearxPoolURL), allowHostsFor(cfg.SearxngURL)...))
	}
	return &Searx{pool: cfg.SearxPoolURL, direct: cfg.SearxngURL, engines: cfg.SearxEngines, disabled: cfg.SearxDisabled,
		timeout: cfg.SearxTimeout, hc: hc}
}

// Search returns the first non-empty hit list from the pool, then the direct
// instance; nil when neither has results (or SEARXNG_DISABLE is set).
func (s *Searx) Search(ctx context.Context, query string, maxResults int) []Hit {
	if s == nil || s.disabled {
		return nil
	}
	for _, a := range []struct {
		base string
		ps   []param
	}{
		{s.pool, []param{{"q", query}, {"n", "4"}}},
		{s.direct, []param{{"q", query}, {"format", "json"}, {"engines", s.engines}}},
	} {
		if a.base == "" {
			continue
		}
		if hits := s.get(ctx, a.base+"/search?"+encodeParams(a.ps), maxResults); len(hits) > 0 {
			return hits
		}
	}
	return nil
}

func (s *Searx) get(ctx context.Context, u string, n int) []Hit {
	timeout := s.timeout
	if timeout <= 0 {
		timeout = 12 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var data map[string]any
	if json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&data) != nil {
		return nil
	}
	var out []Hit
	for _, it := range jlist(data, "results") {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		href := str(m, "url")
		if href == "" {
			href = str(m, "href")
		}
		if href == "" {
			continue
		}
		body := str(m, "content")
		if body == "" {
			body = str(m, "body")
		}
		out = append(out, Hit{Title: str(m, "title"), Href: href, Body: body})
		if len(out) >= n {
			break
		}
	}
	return out
}
