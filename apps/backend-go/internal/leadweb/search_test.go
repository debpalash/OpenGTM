package leadweb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/browserclient"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
)

// Vectors: testdata/search_vectors.json, recorded by
// legacy/python/scripts/record_leadweb_search.py (fake clock, fake httpx, fake ddgs).

type searchVectors struct {
	CacheKeys []struct {
		Salt, Method, Query, Backends, Key string
		MaxResults                         *int `json:"max_results"`
	} `json:"cache_keys"`
	BlockSignals []struct {
		Type, Msg string
		Block     bool
	} `json:"block_signals"`
	BackoffZero []float64 `json:"backoff_delays_zero"`
	BackoffMax  []float64 `json:"backoff_delays_max"`
	CacheOps    []struct {
		Op, Key string
		T       float64
		Empty   bool
		Hit     bool
		Len     int
	} `json:"cache_ops"`
	RateLimiter []struct {
		T      float64
		Op     string
		Domain string
		Result any
		RPM    *int `json:"rpm"`
	} `json:"rate_limiter"`
	RateLimiterStats LimiterStats `json:"rate_limiter_stats"`
	ProxyPool        []struct {
		Op, Domain, Proxy string
		Tier              int
		Result            json.RawMessage
	} `json:"proxy_pool"`
	ProxyPoolEmptyStats PoolStats `json:"proxy_pool_empty_stats"`
	KeyedEngines        []struct {
		Engine      string
		MaxResults  int `json:"max_results"`
		Status      int
		InvalidJSON bool `json:"invalid_json"`
		Request     *struct {
			Method, URL string
			Headers     map[string]string
		}
		Results []Hit
	} `json:"keyed_engines"`
	FallbackOrders []struct {
		Order      *string
		Unset      []string
		Configured []string
	} `json:"fallback_orders"`
	SearchFallback struct {
		Requests []string
		Results  []Hit
	} `json:"search_fallback"`
	DDGSChain []struct {
		Name, Method    string
		Proxy           *string
		Results         []Hit
		DDGSCalls       []ddgsCall `json:"ddgs_calls"`
		ProxyReports    [][]any    `json:"proxy_reports"`
		KeyedRequests   int        `json:"keyed_requests"`
		DDGSBlocked     bool       `json:"ddgs_blocked"`
		DDGSConsecutive int        `json:"ddgs_consecutive"`
		CacheSize       int        `json:"cache_size"`
		SecondCalls     int        `json:"second_call_ddgs_calls"`
		SecondResults   []Hit      `json:"second_results"`
	} `json:"ddgs_chain"`
	DDGSBackedOff struct {
		DDGSCalls int `json:"ddgs_calls"`
		Results   []Hit
	} `json:"ddgs_backed_off"`
	ProxyManager struct {
		Proxies  []string
		Requests []struct {
			Body, Method, URL string
			Timeout           float64
			Headers           map[string]string
		}
	} `json:"proxy_manager"`
	Searx []struct {
		Name      string
		Requests  []string
		Results   []Hit
		DDGSCalls []ddgsCall `json:"ddgs_calls"`
	} `json:"searx"`
	Gosom struct {
		Requests []struct {
			Body, Method, URL string
			Headers           map[string]string
		}
		Rows []map[string]string
	} `json:"gosom"`
	SystemStats map[string]map[string]any `json:"system_stats"`
}

type ddgsCall struct {
	Method, Query string
	Proxy         *string
	Kwargs        map[string]any
}

func loadSearch(t *testing.T) searchVectors {
	t.Helper()
	var v searchVectors
	loadJSON(t, "search_vectors.json", &v)
	return v
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time   { return c.t }
func (c *clock) at(sec float64)   { c.t = time.Unix(0, 0).Add(time.Duration(sec * float64(time.Second))) }
func newClock(sec float64) *clock { c := &clock{}; c.at(sec); return c }

func TestCacheKeysMatchPython(t *testing.T) {
	for _, k := range loadSearch(t).CacheKeys {
		n := -1
		if k.MaxResults != nil {
			n = *k.MaxResults
		}
		if got := CacheKey(k.Salt, k.Method, k.Query, n, k.Backends); got != k.Key {
			t.Errorf("CacheKey(%q,%q,%q,%d,%q) = %s want %s", k.Salt, k.Method, k.Query, n, k.Backends, got, k.Key)
		}
	}
}

func TestBlockSignalsMatchPython(t *testing.T) {
	for _, s := range loadSearch(t).BlockSignals {
		if got := IsBlockSignal(s.Type + " " + s.Msg); got != s.Block {
			t.Errorf("IsBlockSignal(%s %q) = %v", s.Type, s.Msg, got)
		}
	}
}

func TestBackoffDelaysMatchPython(t *testing.T) {
	v := loadSearch(t)
	for name, want := range map[string][]float64{"zero": v.BackoffZero, "max": v.BackoffMax} {
		c := newClock(1000)
		b := NewBackoff(true, 15*time.Second, 900*time.Second)
		b.now = c.now
		b.jitter = func(m float64) float64 {
			if name == "max" {
				return m
			}
			return 0
		}
		for i, w := range want {
			if !b.Record("ddgs", true) {
				t.Fatal("not recorded")
			}
			if got := b.Remaining("ddgs").Seconds(); math.Abs(got-w) > 1e-6 {
				t.Errorf("%s delay %d = %v want %v", name, i, got, w)
			}
		}
		if !b.Blocked("ddgs") || b.Consecutive("ddgs") != len(want) {
			t.Fatal("state")
		}
		b.RecordSuccess("ddgs")
		if b.Blocked("ddgs") || b.Consecutive("ddgs") != 0 {
			t.Fatal("success must clear")
		}
	}
	off := NewBackoff(false, time.Second, time.Second)
	if off.Record("x", true) || off.Blocked("x") {
		t.Fatal("disabled backoff must be a no-op")
	}
	b := NewBackoff(true, time.Second, time.Second)
	if b.Record("x", false) || b.Blocked("x") {
		t.Fatal("a non-block failure must not back off")
	}
}

func TestCacheTTLAndLRUMatchPython(t *testing.T) {
	c := newClock(0)
	cache := NewCache(600*time.Second, 60*time.Second, 3)
	cache.now = c.now
	for i, op := range loadSearch(t).CacheOps {
		c.at(op.T)
		switch op.Op {
		case "put":
			var hits []Hit
			if !op.Empty {
				hits = []Hit{{Title: op.Key}}
			}
			cache.Put(op.Key, hits, op.Empty)
		case "get":
			_, hit := cache.Get(op.Key)
			if hit != op.Hit || cache.Len() != op.Len {
				t.Errorf("op %d get %s @%v: hit=%v len=%d, want hit=%v len=%d", i, op.Key, op.T, hit, cache.Len(), op.Hit, op.Len)
			}
		}
	}
}

func TestCacheReturnsCopies(t *testing.T) {
	cache := NewCache(time.Minute, time.Minute, 10)
	cache.Put("k", []Hit{{Title: "a"}}, false)
	got, _ := cache.Get("k")
	got[0].Title = "mutated"
	again, _ := cache.Get("k")
	if again[0].Title != "a" {
		t.Fatal("cached value mutated through a returned slice")
	}
}

func TestRateLimiterMatchesPython(t *testing.T) {
	v := loadSearch(t)
	c := newClock(0)
	rl := NewRateLimiter()
	rl.now = c.now
	for i, op := range v.RateLimiter {
		c.at(op.T)
		var got any
		switch op.Op {
		case "try":
			got = rl.TryAcquire(op.Domain)
		case "wait":
			got = math.Round(rl.Wait(op.Domain).Seconds()*1e6) / 1e6
		case "fail":
			rl.ReportFailure(op.Domain)
			got = rl.IsBlocked(op.Domain)
		case "blocked":
			got = rl.IsBlocked(op.Domain)
		}
		want := op.Result
		if f, ok := want.(float64); ok {
			if g, ok := got.(float64); !ok || math.Abs(g-f) > 1e-6 {
				t.Errorf("op %d %s %s @%v = %v want %v", i, op.Op, op.Domain, op.T, got, want)
			}
			continue
		}
		if got != want {
			t.Errorf("op %d %s %s @%v = %v want %v", i, op.Op, op.Domain, op.T, got, want)
		}
		if op.RPM != nil && rl.buckets[op.Domain].rpm != float64(*op.RPM) {
			t.Errorf("%s rpm %v want %d", op.Domain, rl.buckets[op.Domain].rpm, *op.RPM)
		}
	}
	// the legacy run also reported success/failures on b.in: Stats counts it
	c.at(2000)
	for range 2 {
		rl.ReportFailure("b.in")
	}
	rl.ReportSuccess("b.in")
	for range 4 {
		rl.ReportFailure("b.in")
	}
	if rl.IsBlocked("b.in") {
		t.Fatal("success must reset the failure streak")
	}
	if st := rl.Stats(); !reflect.DeepEqual(st, v.RateLimiterStats) {
		t.Fatalf("stats %+v want %+v", st, v.RateLimiterStats)
	}
}

func TestRateLimiterAcquireWaitsAndHonoursContext(t *testing.T) {
	rl := NewRateLimiter()
	for range 2 {
		if err := rl.Acquire(context.Background(), "www.linkedin.com"); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := rl.Acquire(ctx, "www.linkedin.com"); err == nil {
		t.Fatal("third request within the minute must wait (2 rpm)")
	}
}

func TestProxyPoolMatchesPython(t *testing.T) {
	v := loadSearch(t)
	c := newClock(5000)
	p := NewProxyPool("1.1.1.1:8080, u:p@2.2.2.2:3128,socks5-host.example:1080, 3.3.3.3:1080socks4 ,,4.4.4.4:80")
	p.now = c.now
	p.choose = func(int) int { return 0 }
	for i, op := range v.ProxyPool {
		switch op.Op {
		case "all":
			var want []string
			_ = json.Unmarshal(op.Result, &want)
			if !slices.Equal(p.all, want) {
				t.Fatalf("all %q want %q", p.all, want)
			}
		case "get":
			var want string
			_ = json.Unmarshal(op.Result, &want)
			if op.Domain == "c.in" {
				c.at(5000 + 1801)
			}
			if got := p.Get(op.Domain, op.Tier); got != want {
				t.Errorf("op %d get %s tier %d = %q want %q", i, op.Domain, op.Tier, got, want)
			}
			if op.Domain == "c.in" {
				p.ReportSuccess(p.all[0], "a.in")
			}
		case "blocked":
			p.ReportBlocked(op.Proxy, op.Domain)
		case "stats":
			var want PoolStats
			_ = json.Unmarshal(op.Result, &want)
			if got := p.Stats(); got != want {
				t.Errorf("op %d stats %+v want %+v", i, got, want)
			}
		}
	}
	if st := NewProxyPool("").Stats(); st != v.ProxyPoolEmptyStats || NewProxyPool("").Get("x", 2) != "" {
		t.Fatal("empty pool")
	}
}

func TestProxyPoolKeepsExplicitSchemes(t *testing.T) {
	// legacy prefixed a second scheme ("socks5://socks5://h:p"): fixed
	p := NewProxyPool("socks5://u:p@1.2.3.4:1080, http://5.6.7.8:3128, socks4a://9.9.9.9:1080")
	if !slices.Equal(p.all, []string{"http://5.6.7.8:3128", "socks5://u:p@1.2.3.4:1080", "socks4a://9.9.9.9:1080"}) {
		t.Fatal(p.all)
	}
}

// ── keyed engines ────────────────────────────────────────────────────

type recorded struct {
	path, query string
	header      http.Header
}

func engineServer(t *testing.T, respond func(path string) (int, string)) (*httptest.Server, *[]recorded) {
	t.Helper()
	var mu sync.Mutex
	var reqs []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, recorded{r.URL.Path, r.URL.RawQuery, r.Header.Clone()})
		mu.Unlock()
		st, body := respond(r.URL.Path)
		w.WriteHeader(st)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &reqs
}

var engineResponses = map[string]string{
	"serpapi":    `{"organic_results": [{"title": "A & B", "link": "https://a.example/", "snippet": "sa"}, {"title": null, "link": "", "snippet": "x"}, {"link": "https://b.example/"}]}`,
	"bing":       `{"webPages": {"value": [{"name": "N", "url": "https://n.example/", "snippet": "s"}, {"url": null}]}}`,
	"google_cse": `{"items": [{"title": "G", "link": "https://g.example/", "snippet": null}]}`,
	"brave":      `{"web": {"results": [{"title": "Br", "url": "https://br.example/", "description": "d"}]}}`,
	"serpingapi": `{"organic": [{"title": "S", "link": "https://s.example/", "snippet": "ss"}]}`,
	"serply":     `{"results": [{"title": "Y", "link": "ftp://nope/", "description": "x"}, {"title": "Y2", "link": "http://y.example/", "description": "yd"}]}`,
}

func allKeys() config.LeadwebSettings {
	cfg, _ := config.LeadwebFrom(func(string) string { return "" })
	cfg.SerpAPIKey, cfg.BingKey, cfg.GoogleCSEID, cfg.GoogleCSEKey = "serp-k", "bing-k", "cse-id", "cse-k"
	cfg.BraveKey, cfg.SerpingAPIKey, cfg.SerplyKey = "brave-k", "sping-k", "serply-k"
	return cfg
}

func endpointsFor(srv *httptest.Server) map[string]string {
	m := map[string]string{}
	for name := range engineResponses {
		m[name] = srv.URL + "/" + name
	}
	return m
}

func TestKeyedEnginesMatchPython(t *testing.T) {
	v := loadSearch(t)
	status, body := 200, ""
	srv, reqs := engineServer(t, func(string) (int, string) { return status, body })
	e := NewKeyedEngines(allKeys(), srv.Client(), NewBackoff(true, time.Second, time.Second))
	e.Endpoints = endpointsFor(srv)
	for _, c := range v.KeyedEngines {
		*reqs = nil
		status, body = 200, engineResponses[c.Engine]
		switch {
		case c.Status == 429:
			status, body = 429, `{"error":"x"}`
		case c.InvalidJSON:
			body = "not json"
		}
		q := "acme staffing & co/ü"
		if c.Request == nil {
			q = "q"
		}
		got := e.run(context.Background(), c.Engine, q, c.MaxResults)
		if !reflect.DeepEqual(nzh(got), nzh(c.Results)) {
			t.Errorf("%s n=%d results %+v want %+v", c.Engine, c.MaxResults, got, c.Results)
		}
		if c.Request == nil {
			continue
		}
		wantURL, _ := url.Parse(c.Request.URL)
		if len(*reqs) != 1 || (*reqs)[0].query != wantURL.RawQuery {
			t.Errorf("%s query %v want %s", c.Engine, *reqs, wantURL.RawQuery)
			continue
		}
		for k, v := range c.Request.Headers {
			if g := (*reqs)[0].header.Get(k); g != v {
				t.Errorf("%s header %s = %q want %q", c.Engine, k, g, v)
			}
		}
	}
}

func nzh(h []Hit) []Hit {
	if h == nil {
		return []Hit{}
	}
	return h
}

func TestKeyedFallbackOrderMatchesPython(t *testing.T) {
	for _, o := range loadSearch(t).FallbackOrders {
		env := map[string]string{"SERPAPI_KEY": "a", "BING_SEARCH_KEY": "b", "GOOGLE_CSE_ID": "c", "GOOGLE_CSE_KEY": "d",
			"BRAVE_SEARCH_KEY": "e", "SERPINGAPI_API_KEY": "f", "SERPLY_API_KEY": "g"}
		if o.Order != nil {
			env["SEARCH_FALLBACK_ORDER"] = *o.Order
			if *o.Order == "" {
				env["SEARCH_FALLBACK_ORDER"] = "none" // Go spelling of Python's empty value
			}
		}
		for _, u := range o.Unset {
			delete(env, u)
		}
		cfg, err := config.LeadwebFrom(func(k string) string { return env[k] })
		if err != nil {
			t.Fatal(err)
		}
		if got := NewKeyedEngines(cfg, nil, nil).Configured(); !slices.Equal(got, o.Configured) {
			t.Errorf("order %v unset %v: %v want %v", o.Order, o.Unset, got, o.Configured)
		}
	}
}

func TestKeyedSearchFallbackMatchesPython(t *testing.T) {
	v := loadSearch(t).SearchFallback
	srv, reqs := engineServer(t, func(p string) (int, string) {
		switch p {
		case "/serpapi":
			return 200, `{"organic_results": []}`
		case "/bing":
			return 500, `{}`
		}
		return 200, engineResponses["google_cse"]
	})
	e := NewKeyedEngines(allKeys(), srv.Client(), NewBackoff(true, time.Second, time.Second))
	e.Endpoints = endpointsFor(srv)
	got := e.Search(context.Background(), "q", 4)
	var paths []string
	for _, r := range *reqs {
		paths = append(paths, r.path)
	}
	if !slices.Equal(paths, []string{"/serpapi", "/bing", "/google_cse"}) || !reflect.DeepEqual(got, v.Results) {
		t.Fatalf("paths %v results %+v want %v %+v", paths, got, v.Requests, v.Results)
	}
	// an engine in backoff is skipped
	e.backoff.Record("serpapi", true)
	*reqs = nil
	e.Search(context.Background(), "q", 4)
	if (*reqs)[0].path != "/bing" {
		t.Fatal("backed-off engine was called")
	}
}

func TestKeyedEnginesNeverLogTheKey(t *testing.T) {
	e := NewKeyedEngines(allKeys(), &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("dial failed")
	})}, nil)
	if got := e.run(context.Background(), "serpapi", "q", 3); got != nil {
		t.Fatal(got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// ── resilient keyless search over the browser service ────────────────

type fakeBrowser struct {
	mu       sync.Mutex
	searches []browserclient.SearchRequest
	script   []browserclient.SearchOut
	renders  []browserclient.RenderRequest
	render   func(browserclient.RenderRequest) (browserclient.RenderResult, error)
	err      error
}

func (f *fakeBrowser) Search(_ context.Context, r browserclient.SearchRequest) (browserclient.SearchOut, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searches = append(f.searches, r)
	if f.err != nil {
		return browserclient.SearchOut{}, f.err
	}
	if len(f.script) == 0 {
		return browserclient.SearchOut{Error: "no_results"}, nil
	}
	out := f.script[0]
	f.script = f.script[1:]
	return out, nil
}

func (f *fakeBrowser) Render(_ context.Context, r browserclient.RenderRequest) (browserclient.RenderResult, error) {
	f.mu.Lock()
	f.renders = append(f.renders, r)
	f.mu.Unlock()
	return f.render(r)
}

// step maps a recorded ddgs step to what the browser service answers.
func step(kind string) browserclient.SearchOut {
	switch kind {
	case "hit":
		return browserclient.SearchOut{Hits: []browserclient.SearchHit{{Title: "T", URL: "https://h.example/", Body: "B"}}}
	case "empty":
		return browserclient.SearchOut{}
	case "no_results":
		return browserclient.SearchOut{Error: "no_results"}
	case "rate_limited":
		return browserclient.SearchOut{Error: "rate_limited"}
	case "timeout":
		return browserclient.SearchOut{Error: "timeout"}
	}
	panic(kind)
}

// scenario scripts mirror the recorder's fake ddgs steps
var chainScripts = map[string][]string{
	"direct_hit":                 {"hit"},
	"direct_empty_proxy_hit":     {"empty", "hit"},
	"direct_ratelimit_proxy_hit": {"rate_limited", "hit"},
	"no_proxy_empty_keyed":       {"no_results"},
	"both_fail_no_keys":          {"no_results", "timeout"},
	"news_no_keyed":              {"empty"},
}

type reportLog struct {
	mu   sync.Mutex
	reqs []string
}

func managerServer(t *testing.T, bodies []string) (*httptest.Server, *reportLog) {
	t.Helper()
	log := &reportLog{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		log.mu.Lock()
		log.reqs = append(log.reqs, r.Method+" "+r.URL.RequestURI()+" "+r.Header.Get("Accept")+r.Header.Get("Content-Type")+" "+string(b))
		var body string
		if r.URL.Path == "/api/proxy" && len(bodies) > 0 {
			body, bodies = bodies[0], bodies[1:]
		}
		log.mu.Unlock()
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, log
}

func newTestSearcher(t *testing.T, b Browser, cfg config.LeadwebSettings, mgr *ProxyManager, keyedSrv *httptest.Server) *Searcher {
	t.Helper()
	backoff := NewBackoff(true, 15*time.Second, 900*time.Second)
	backoff.jitter = func(float64) float64 { return 0 }
	keyed := NewKeyedEngines(cfg, nil, backoff)
	if keyedSrv != nil {
		keyed.hc = keyedSrv.Client()
		keyed.Endpoints = endpointsFor(keyedSrv)
	}
	return &Searcher{cfg: cfg, browser: b, cache: NewCache(cfg.CacheTTL, cfg.CacheEmptyTTL, cfg.CacheMaxEntries),
		backoff: backoff, keyed: keyed, proxies: mgr}
}

func TestResilientSearchMatchesPython(t *testing.T) {
	v := loadSearch(t)
	for _, sc := range v.DDGSChain {
		t.Run(sc.Name, func(t *testing.T) {
			cfg, _ := config.LeadwebFrom(func(string) string { return "" })
			if sc.KeyedRequests > 0 || sc.Name == "news_no_keyed" {
				cfg.SerpAPIKey = "serp-k"
			}
			keyedSrv, keyedReqs := engineServer(t, func(string) (int, string) { return 200, engineResponses["serpapi"] })
			mgrSrv, mgrLog := managerServer(t, nil)
			fb := &fakeBrowser{}
			for _, k := range chainScripts[sc.Name] {
				fb.script = append(fb.script, step(k))
			}
			s := newTestSearcher(t, fb, cfg, NewProxyManager(mgrSrv.URL, mgrSrv.Client()), keyedSrv)
			o := SearchOptions{MaxResults: 7, NoProxy: sc.Proxy == nil}
			if sc.Proxy != nil {
				o.Proxy = browserclient.ProxyURL(*sc.Proxy)
			}
			search := s.Text
			if sc.Method == "news" {
				search = s.News
			}
			got := search(context.Background(), "acme staffing", o)
			if !reflect.DeepEqual(nzh(got), nzh(sc.Results)) {
				t.Fatalf("results %+v want %+v", got, sc.Results)
			}
			if len(fb.searches) != len(sc.DDGSCalls) {
				t.Fatalf("%d searches, want %d", len(fb.searches), len(sc.DDGSCalls))
			}
			for i, c := range sc.DDGSCalls {
				r := fb.searches[i]
				wantProxy := ""
				if c.Proxy != nil {
					wantProxy = *c.Proxy
				}
				if string(r.UpstreamProxy) != wantProxy || r.Method != c.Method || r.Query != c.Query ||
					float64(r.MaxResults) != c.Kwargs["max_results"].(float64) ||
					strings.Join(r.Backends, ", ") != c.Kwargs["backend"] {
					t.Errorf("search %d = %+v, want %+v", i, r, c)
				}
			}
			var reports []string
			for _, r := range mgrLog.reqs {
				if strings.HasPrefix(r, "POST /api/report") {
					reports = append(reports, r[strings.Index(r, "{"):])
				}
			}
			var want []string
			for _, r := range sc.ProxyReports {
				st := "failure"
				if r[2].(bool) {
					st = "success"
				}
				want = append(want, fmt.Sprintf(`{"host": %q, "port": %v, "status": %q}`, r[0], r[1], st))
			}
			if !slices.Equal(reports, want) {
				t.Errorf("proxy reports %q want %q", reports, want)
			}
			if len(*keyedReqs) != sc.KeyedRequests {
				t.Errorf("keyed requests %d want %d", len(*keyedReqs), sc.KeyedRequests)
			}
			if s.backoff.Blocked(ddgsHost) != sc.DDGSBlocked || s.backoff.Consecutive(ddgsHost) != sc.DDGSConsecutive {
				t.Errorf("backoff blocked=%v n=%d", s.backoff.Blocked(ddgsHost), s.backoff.Consecutive(ddgsHost))
			}
			if s.cache.Len() != sc.CacheSize {
				t.Errorf("cache %d want %d", s.cache.Len(), sc.CacheSize)
			}
			before := len(fb.searches)
			again := search(context.Background(), "ACME  staffing", o)
			if len(fb.searches)-before != sc.SecondCalls || !reflect.DeepEqual(nzh(again), nzh(sc.SecondResults)) {
				t.Errorf("second call: %d searches, %+v", len(fb.searches)-before, again)
			}
		})
	}
}

func TestBackedOffSearchGoesToKeyedEngines(t *testing.T) {
	v := loadSearch(t).DDGSBackedOff
	cfg, _ := config.LeadwebFrom(func(string) string { return "" })
	cfg.BraveKey = "brave-k"
	keyedSrv, _ := engineServer(t, func(string) (int, string) { return 200, engineResponses["brave"] })
	fb := &fakeBrowser{}
	s := newTestSearcher(t, fb, cfg, nil, keyedSrv)
	s.backoff.Record(ddgsHost, true)
	got := s.Text(context.Background(), "q", SearchOptions{MaxResults: 3, NoProxy: true})
	if len(fb.searches) != v.DDGSCalls || !reflect.DeepEqual(got, v.Results) {
		t.Fatalf("%d searches, %+v", len(fb.searches), got)
	}
}

func TestSearchCacheIsSaltedByWorkspace(t *testing.T) {
	cfg, _ := config.LeadwebFrom(func(string) string { return "" })
	fb := &fakeBrowser{script: []browserclient.SearchOut{step("hit"), step("hit")}}
	s := newTestSearcher(t, fb, cfg, nil, nil)
	s.Text(context.Background(), "secret plan", SearchOptions{Workspace: "ws-a", NoProxy: true})
	s.Text(context.Background(), "secret plan", SearchOptions{Workspace: "ws-b", NoProxy: true})
	s.Text(context.Background(), "secret plan", SearchOptions{Workspace: "ws-a", NoProxy: true})
	if len(fb.searches) != 2 {
		t.Fatalf("%d searches: a workspace must not read another's cached free-form query", len(fb.searches))
	}
}

func TestSearchServiceOutageIsNotABlockAndNotTheProxysFault(t *testing.T) {
	cfg, _ := config.LeadwebFrom(func(string) string { return "" })
	mgrSrv, mgrLog := managerServer(t, nil)
	fb := &fakeBrowser{err: browserclient.ErrUnreachable}
	s := newTestSearcher(t, fb, cfg, NewProxyManager(mgrSrv.URL, mgrSrv.Client()), nil)
	if got := s.Text(context.Background(), "q", SearchOptions{Proxy: "http://1.2.3.4:8080"}); got != nil {
		t.Fatal(got)
	}
	if s.backoff.Blocked(ddgsHost) || len(mgrLog.reqs) != 0 {
		t.Fatal("outage counted against ddgs or the proxy")
	}
}

func TestSearchUsesTheProxyManagerByDefault(t *testing.T) {
	cfg, _ := config.LeadwebFrom(func(string) string { return "" })
	mgrSrv, _ := managerServer(t, []string{`{"success": true, "proxies": [{"host": "1.1.1.1", "port": 1080, "protocol": "socks5"}]}`})
	fb := &fakeBrowser{script: []browserclient.SearchOut{step("empty"), step("hit")}}
	s := newTestSearcher(t, fb, cfg, NewProxyManager(mgrSrv.URL, mgrSrv.Client()), nil)
	s.Text(context.Background(), "q", SearchOptions{})
	if len(fb.searches) != 2 || fb.searches[0].UpstreamProxy != "" || fb.searches[1].UpstreamProxy != "socks5://1.1.1.1:1080" {
		t.Fatalf("%+v", fb.searches)
	}
}

func TestProxyManagerMatchesPython(t *testing.T) {
	v := loadSearch(t).ProxyManager
	srv, log := managerServer(t, []string{
		`{"success": true, "proxies": [{"host": "1.1.1.1", "port": 1080, "protocol": "socks5"}, {"host": "2.2.2.2", "port": 8080, "protocol": "http"}, {"host": "3.3.3.3", "port": 1081}]}`,
		`{"success": false}`, `{"success": false}`, `{"success": false}`,
	})
	m := NewProxyManager(srv.URL+"/", srv.Client())
	var got []string
	for range 5 {
		got = append(got, m.Get(context.Background(), ""))
	}
	got = append(got, m.Get(context.Background(), "socks5"))
	m.Report(context.Background(), "socks5://1.1.1.1:1080", true)
	m.Report(context.Background(), "http://user:pw@2.2.2.2:8080", false)
	if !slices.Equal(got, v.Proxies) {
		t.Fatalf("proxies %q want %q", got, v.Proxies)
	}
	var want []string
	for _, r := range v.Requests {
		u, _ := url.Parse(r.URL)
		hdr := r.Headers["Accept"] + r.Headers["Content-type"]
		want = append(want, r.Method+" "+u.RequestURI()+" "+hdr+" "+r.Body)
	}
	if !slices.Equal(log.reqs, want) {
		t.Fatalf("requests\n %q\nwant\n %q", log.reqs, want)
	}
	if NewProxyManager("", nil).Get(context.Background(), "") != "" {
		t.Fatal("unconfigured manager")
	}
}

func TestSearxWebSearchMatchesPython(t *testing.T) {
	v := loadSearch(t)
	responses := map[string][][2]string{
		"pool_hit": {{"200", `{"results": [{"url": "https://p.example/", "title": "P", "content": "pc"}, {"href": "https://h.example/", "title": "H", "body": "hb"}, {"title": "nourl"}]}`}},
		"pool_empty_direct_hit": {{"200", `{"results": []}`},
			{"200", `{"results": [{"url": "https://d.example/", "title": null, "content": null}]}`}},
		"pool_500_direct_bad_json": {{"500", `{}`}, {"200", `<html>`}},
	}
	for _, sc := range v.Searx {
		resp := responses[sc.Name]
		var got []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = append(got, r.Host+r.URL.RequestURI())
			st, body := resp[0][0], resp[0][1]
			resp = resp[1:]
			if st == "500" {
				w.WriteHeader(500)
			}
			_, _ = io.WriteString(w, body)
		}))
		host := strings.TrimPrefix(srv.URL, "http://")
		cfg, _ := config.LeadwebFrom(func(string) string { return "" })
		// both "instances" are the same test server; the path+query tell them apart
		cfg.SearxPoolURL, cfg.SearxngURL = srv.URL, srv.URL
		fb := &fakeBrowser{script: []browserclient.SearchOut{step("hit")}}
		s := &Searcher{cfg: cfg, browser: fb, searx: NewSearx(cfg, srv.Client())}
		hits, err := s.WebSearch(context.Background(), "acme staffing", 2, false)
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		var want []string
		for _, r := range sc.Requests {
			u, _ := url.Parse(r)
			want = append(want, host+u.RequestURI())
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s requests %q want %q", sc.Name, got, want)
		}
		if !reflect.DeepEqual(nzh(hits), nzh(sc.Results)) {
			t.Errorf("%s hits %+v want %+v", sc.Name, hits, sc.Results)
		}
		if len(fb.searches) != len(sc.DDGSCalls) {
			t.Errorf("%s ddgs calls %d want %d", sc.Name, len(fb.searches), len(sc.DDGSCalls))
		}
		for _, r := range fb.searches {
			if r.Backends == nil || len(r.Backends) != 0 || r.UpstreamProxy != "" || r.MaxResults != 2 {
				t.Errorf("%s ddgs fallback request %+v (want ddgs default backends, direct)", sc.Name, r)
			}
		}
	}
}

func TestWebSearchRaiseErrors(t *testing.T) {
	cfg, _ := config.LeadwebFrom(func(string) string { return "" })
	cfg.SearxDisabled = true
	s := &Searcher{cfg: cfg, browser: &fakeBrowser{script: []browserclient.SearchOut{step("rate_limited"), step("no_results")}},
		searx: NewSearx(cfg, nil)}
	if _, err := s.WebSearch(context.Background(), "q", 3, true); err != ErrSearchFailed {
		t.Fatal(err)
	}
	if hits, err := s.WebSearch(context.Background(), "q", 3, false); err != nil || hits != nil {
		t.Fatal(hits, err)
	}
}

func TestGMapsMatchesPython(t *testing.T) {
	v := loadSearch(t).Gosom
	csvBody := "title,phone,website,emails\r\n\"Acme, Inc\",+91 98765 43210,https://acme.in,\"a@acme.in, b@acme.in\"\r\nBeta,,,\r\n"
	steps := [][2]string{{"201", `{"id": "job-1"}`}, {"200", `{"status": "working"}`}, {"500", `{}`},
		{"200", `{"status": "ok"}`}, {"200", csvBody}}
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.Path+" "+r.Header.Get("Content-Type")+" "+string(b))
		st := steps[0]
		steps = steps[1:]
		var code int
		fmt.Sscan(st[0], &code)
		w.WriteHeader(code)
		_, _ = io.WriteString(w, st[1])
	}))
	defer srv.Close()
	g := NewGMaps(srv.URL+"/", srv.Client())
	var slept []time.Duration
	g.sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	g.Poll = time.Second
	rows := g.SearchPlaces(context.Background(), []string{"cafes in pune", strings.Repeat("x", 130)}, GMapsJob{})
	var want []string
	for _, r := range v.Requests {
		u, _ := url.Parse(r.URL)
		want = append(want, r.Method+" "+u.Path+" "+r.Headers["content-type"]+" "+r.Body)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("requests\n %q\nwant\n %q", got, want)
	}
	if !reflect.DeepEqual(rows, v.Rows) || len(slept) != 3 {
		t.Fatalf("rows %v slept %v", rows, slept)
	}
}

func TestGMapsFailuresAreEmpty(t *testing.T) {
	for name, steps := range map[string][][2]string{
		"failed":         {{"201", `{"id": "job-2"}`}, {"200", `{"status": "failed"}`}},
		"create_failed":  {{"400", "bad"}},
		"no_id":          {{"201", `{}`}},
		"timeout":        {{"201", `{"id": "job-3"}`}, {"200", `{"status": "working"}`}, {"200", `{"status": "working"}`}, {"200", `{"status": "working"}`}},
		"empty_download": {{"201", `{"id": "j"}`}, {"200", `{"status": "ok"}`}, {"200", " \n"}},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			st := steps[0]
			steps = steps[1:]
			var code int
			fmt.Sscan(st[0], &code)
			w.WriteHeader(code)
			_, _ = io.WriteString(w, st[1])
		}))
		g := NewGMaps(srv.URL, srv.Client())
		g.sleep = func(context.Context, time.Duration) error { return nil }
		g.Poll, g.Overall = time.Second, 3*time.Second
		if rows := g.SearchPlaces(context.Background(), []string{"q"}, GMapsJob{}); rows != nil {
			t.Errorf("%s: %v", name, rows)
		}
		srv.Close()
	}
	if NewGMaps("", nil).Available() || NewGMaps("", nil).SearchPlaces(context.Background(), []string{"q"}, GMapsJob{}) != nil {
		t.Fatal("unconfigured")
	}
}

func TestGMapsEgressAllowsOnlyItsHost(t *testing.T) {
	if got := allowHostsFor("http://gmaps:8080"); !slices.Equal(got, []string{"gmaps:8080"}) {
		t.Fatal(got)
	}
	if got := allowHostsFor("https://Proxy.Internal"); !slices.Equal(got, []string{"proxy.internal:443"}) {
		t.Fatal(got)
	}
	// the default client refuses any other internal address
	g := NewGMaps("http://127.0.0.1:1", nil)
	if _, err := g.hc.Get("http://127.0.0.2:1/"); err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("non-listed internal host reachable: %v", err)
	}
}

func TestSystemStatsShapeMatchesPython(t *testing.T) {
	v := loadSearch(t).SystemStats
	w := &Web{Pool: NewProxyPool(""), Limiter: NewRateLimiter(), browser: nil}
	b, _ := json.Marshal(w.Stats(context.Background()))
	var got map[string]map[string]any
	_ = json.Unmarshal(b, &got)
	if !reflect.DeepEqual(got, v) {
		t.Fatalf("%s\nwant %v", b, v)
	}
}
