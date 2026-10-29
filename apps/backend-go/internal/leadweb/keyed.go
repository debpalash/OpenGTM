package leadweb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
)

// Keyed search APIs (leadgen/search_engines.py), called from Go directly: the
// API keys never reach the browser service. Every adapter returns [] on any
// failure (missing key, HTTP error, bad JSON), as legacy.

type param struct{ k, v string }

// encodeParams is httpx's query encoding: insertion order, quote_plus.
func encodeParams(ps []param) string {
	parts := make([]string, len(ps))
	for i, p := range ps {
		parts[i] = url.QueryEscape(p.k) + "=" + url.QueryEscape(p.v)
	}
	return strings.Join(parts, "&")
}

type keyedEngine struct {
	name       string
	configured func(*KeyedEngines) bool
	request    func(e *KeyedEngines, q string, n int) (endpoint string, ps []param, hdr map[string]string)
	parse      func(data map[string]any, n int) []Hit
	// search handles APIs with a different wire contract from the legacy GET adapters.
	search func(context.Context, *KeyedEngines, string, int) []Hit
}

// KeyedEngines is the fallback chain behind keyless search.
type KeyedEngines struct {
	cfg     config.LeadwebSettings
	hc      *http.Client
	backoff *Backoff
	// Endpoints overrides the API URLs (tests); keys are engine names.
	Endpoints map[string]string
}

func (KeyedEngines) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("KeyedEngines{credentials:[redacted]}"))
}

var keyedRegistry = []keyedEngine{
	{
		name:       "serpapi",
		configured: func(e *KeyedEngines) bool { return e.cfg.SerpAPIKey != "" },
		request: func(e *KeyedEngines, q string, n int) (string, []param, map[string]string) {
			return "https://serpapi.com/search.json", []param{{"engine", "google"}, {"q", q}, {"num", strconv.Itoa(n)},
				{"api_key", e.cfg.SerpAPIKey.Reveal()}}, nil
		},
		parse: func(d map[string]any, n int) []Hit {
			return pick(jlist(d, "organic_results"), "link", "title", "snippet", n, false)
		},
	},
	{
		name:       "bing",
		configured: func(e *KeyedEngines) bool { return e.cfg.BingKey != "" },
		request: func(e *KeyedEngines, q string, n int) (string, []param, map[string]string) {
			return "https://api.bing.microsoft.com/v7.0/search",
				[]param{{"q", q}, {"count", strconv.Itoa(n)}, {"responseFilter", "Webpages"}},
				map[string]string{"Ocp-Apim-Subscription-Key": e.cfg.BingKey.Reveal()}
		},
		parse: func(d map[string]any, n int) []Hit {
			return pick(jlist(obj(d, "webPages"), "value"), "url", "name", "snippet", n, false)
		},
	},
	{
		name:       "google_cse",
		configured: func(e *KeyedEngines) bool { return e.cfg.GoogleCSEID != "" && e.cfg.GoogleCSEKey != "" },
		request: func(e *KeyedEngines, q string, n int) (string, []param, map[string]string) {
			return "https://www.googleapis.com/customsearch/v1", []param{{"q", q}, {"cx", e.cfg.GoogleCSEID.Reveal()},
				{"key", e.cfg.GoogleCSEKey.Reveal()}, {"num", strconv.Itoa(min(n, 10))}}, nil
		},
		parse: func(d map[string]any, n int) []Hit {
			return pick(jlist(d, "items"), "link", "title", "snippet", n, false)
		},
	},
	{
		name:       "brave",
		configured: func(e *KeyedEngines) bool { return e.cfg.BraveKey != "" },
		request: func(e *KeyedEngines, q string, n int) (string, []param, map[string]string) {
			return "https://api.search.brave.com/res/v1/web/search", []param{{"q", q}, {"count", strconv.Itoa(min(n, 20))}},
				map[string]string{"X-Subscription-Token": e.cfg.BraveKey.Reveal(), "Accept": "application/json"}
		},
		parse: func(d map[string]any, n int) []Hit {
			return pick(jlist(obj(d, "web"), "results"), "url", "title", "description", n, false)
		},
	},
	{
		name:       "serpingapi",
		configured: func(e *KeyedEngines) bool { return e.cfg.SerpingAPIKey != "" },
		request: func(e *KeyedEngines, q string, n int) (string, []param, map[string]string) {
			return "https://api.serpingapi.com/v1/search", []param{{"q", q}, {"num", strconv.Itoa(min(n, 100))}},
				map[string]string{"X-API-Key": e.cfg.SerpingAPIKey.Reveal()}
		},
		parse: func(d map[string]any, n int) []Hit {
			return pick(jlist(d, "organic"), "link", "title", "snippet", n, false)
		},
	},
	{
		name:       "serply",
		configured: func(e *KeyedEngines) bool { return e.cfg.SerplyKey != "" },
		request: func(e *KeyedEngines, q string, n int) (string, []param, map[string]string) {
			return "https://api.serply.io/v1/search", []param{{"q", q}, {"num", strconv.Itoa(min(n, 10))}},
				map[string]string{"X-Api-Key": e.cfg.SerplyKey.Reveal(), "User-Agent": "OpenGTM"}
		},
		parse: func(d map[string]any, n int) []Hit {
			return pick(jlist(d, "results"), "link", "title", "description", n, true)
		},
	},
	{
		name:       "firecrawl",
		configured: func(e *KeyedEngines) bool { return e.cfg.FirecrawlKey != "" },
		search:     firecrawlSearch,
	},
}

func obj(d map[string]any, k string) map[string]any {
	m, _ := d[k].(map[string]any)
	return m
}

func jlist(d map[string]any, k string) []any {
	l, _ := d[k].([]any)
	return l
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

// pick maps result objects to hits: no href = skipped, at most n.
func pick(items []any, hrefKey, titleKey, bodyKey string, n int, httpOnly bool) []Hit {
	var out []Hit
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		href := str(m, hrefKey)
		if href == "" || httpOnly && !strings.HasPrefix(href, "http://") && !strings.HasPrefix(href, "https://") {
			continue
		}
		out = append(out, Hit{Title: str(m, titleKey), Href: href, Body: str(m, bodyKey)})
		if len(out) >= n {
			break
		}
	}
	return out
}

// NewKeyedEngines builds the chain. hc should be the SSRF-guarded egress
// client (the APIs are public hosts).
func NewKeyedEngines(cfg config.LeadwebSettings, hc *http.Client, backoff *Backoff) *KeyedEngines {
	return &KeyedEngines{cfg: cfg, hc: hc, backoff: backoff}
}

// Configured lists the engines with keys, in SEARCH_FALLBACK_ORDER (unknown
// names dropped).
func (e *KeyedEngines) Configured() []string {
	var out []string
	for _, name := range e.cfg.FallbackOrder {
		for _, k := range keyedRegistry {
			if k.name == name && k.configured(e) {
				out = append(out, name)
			}
		}
	}
	return out
}

// Search tries each configured engine (skipping one in backoff) and returns
// the first non-empty result set, or nil.
func (e *KeyedEngines) Search(ctx context.Context, query string, maxResults int) []Hit {
	for _, name := range e.Configured() {
		if ctx.Err() != nil {
			return nil
		}
		if e.backoff != nil && e.backoff.Blocked(name) {
			continue
		}
		if hits := e.run(ctx, name, query, maxResults); len(hits) > 0 {
			if e.backoff != nil {
				e.backoff.RecordSuccess(name)
			}
			return hits
		}
	}
	return nil
}

func (e *KeyedEngines) engine(name string) *keyedEngine {
	for i := range keyedRegistry {
		if keyedRegistry[i].name == name {
			return &keyedRegistry[i]
		}
	}
	return nil
}

// run is one adapter: [] when unconfigured or on any failure.
func (e *KeyedEngines) run(ctx context.Context, name, query string, n int) []Hit {
	k := e.engine(name)
	if k == nil || !k.configured(e) {
		return nil
	}
	if k.search != nil {
		return k.search(ctx, e, query, n)
	}
	endpoint, ps, hdr := k.request(e, query, n)
	if o := e.Endpoints[name]; o != "" {
		endpoint = o
	}
	data := e.getJSON(ctx, endpoint+"?"+encodeParams(ps), hdr)
	if data == nil {
		return nil
	}
	return k.parse(data, n)
}

func (e *KeyedEngines) getJSON(ctx context.Context, u string, hdr map[string]string) map[string]any {
	timeout := e.cfg.EngineTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Accept", "*/*")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := e.hc.Do(req)
	if err != nil {
		return nil // the error text holds the URL (and the key): never logged
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var data map[string]any
	if json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&data) != nil {
		return nil
	}
	return data
}
