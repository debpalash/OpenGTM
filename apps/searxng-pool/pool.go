package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Instance is one SearXNG endpoint (ours or a public one).
type Instance struct {
	URL   string
	Grade string

	mu          sync.Mutex
	healthy     bool
	jsonOK      bool // returns valid JSON (many public instances disable it)
	latencyMs   int64
	fails       int
	lastChecked time.Time
	lastUsed    time.Time

	client  *http.Client
	inFlight int32
}

func newInstance(rawURL, grade, proxy string) *Instance {
	tr := &http.Transport{
		MaxIdleConns:        20,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
	}
	if proxy != "" {
		if pu, err := url.Parse(proxy); err == nil {
			tr.Proxy = http.ProxyURL(pu)
		}
	}
	return &Instance{
		URL:    strings.TrimRight(rawURL, "/"),
		Grade:  grade,
		client: &http.Client{Transport: tr},
	}
}

// snapshot for read-only reporting / selection.
type instSnap struct {
	URL       string `json:"url"`
	Grade     string `json:"grade"`
	Healthy   bool   `json:"healthy"`
	JSONOK    bool   `json:"json_ok"`
	LatencyMs int64  `json:"latency_ms"`
	Fails     int    `json:"fails"`
}

func (i *Instance) snap() instSnap {
	i.mu.Lock()
	defer i.mu.Unlock()
	return instSnap{i.URL, i.Grade, i.healthy, i.jsonOK, i.latencyMs, i.fails}
}

// Pool owns the instances and the background health loop.
type Pool struct {
	cfg       Config
	mu        sync.RWMutex
	instances []*Instance
	httpc     *http.Client // for the searx.space feed fetch
}

func NewPool(cfg Config) *Pool {
	return &Pool{
		cfg:   cfg,
		httpc: &http.Client{Timeout: 20 * time.Second},
	}
}

// Bootstrap seeds the pool from the configured seeds + the public feed.
func (p *Pool) Bootstrap(ctx context.Context) {
	seen := map[string]bool{}
	var insts []*Instance
	add := func(u, grade string) {
		u = strings.TrimRight(u, "/")
		if u == "" || seen[u] {
			return
		}
		seen[u] = true
		proxy := ""
		if n := len(p.cfg.Proxies); n > 0 {
			proxy = p.cfg.Proxies[len(insts)%n]
		}
		insts = append(insts, newInstance(u, grade, proxy))
	}

	for _, u := range p.cfg.SeedInstances {
		add(u, "seed")
	}
	if p.cfg.UsePublicFeed {
		for _, e := range p.fetchPublicFeed(ctx) {
			if len(insts) >= p.cfg.MaxInstances {
				break
			}
			add(e.url, e.grade)
		}
	}

	p.mu.Lock()
	p.instances = insts
	p.mu.Unlock()
	log.Printf("pool: bootstrapped with %d instances (seeds=%d, feed=%v)",
		len(insts), len(p.cfg.SeedInstances), p.cfg.UsePublicFeed)

	p.CheckAll(ctx) // first pass synchronously so /search works immediately
}

type feedEntry struct {
	url   string
	grade string
}

// fetchPublicFeed pulls searx.space's registry and keeps A/B-grade HTTPS hosts.
func (p *Pool) fetchPublicFeed(ctx context.Context) []feedEntry {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.cfg.FeedURL, nil)
	resp, err := p.httpc.Do(req)
	if err != nil {
		log.Printf("pool: feed fetch failed: %v", err)
		return nil
	}
	defer resp.Body.Close()
	var data struct {
		Instances map[string]struct {
			HTTP struct {
				Grade string `json:"grade"`
			} `json:"http"`
			NetworkType string `json:"network_type"`
		} `json:"instances"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		log.Printf("pool: feed decode failed: %v", err)
		return nil
	}
	var out []feedEntry
	for u, v := range data.Instances {
		if !strings.HasPrefix(u, "https://") || v.NetworkType != "normal" {
			continue // skip tor/i2p and non-clearnet
		}
		if gradeRank(v.HTTP.Grade) < gradeRank(p.cfg.MinGrade) {
			continue
		}
		out = append(out, feedEntry{u, v.HTTP.Grade})
	}
	// best grade first
	sort.Slice(out, func(a, b int) bool { return gradeRank(out[a].grade) > gradeRank(out[b].grade) })
	return out
}

func gradeRank(g string) int {
	switch strings.ToUpper(strings.TrimSpace(g)) {
	case "A+":
		return 6
	case "A":
		return 5
	case "A-":
		return 4
	case "B":
		return 3
	case "C":
		return 2
	case "D":
		return 1
	}
	return 0
}

// HealthLoop re-checks every instance on an interval until ctx is cancelled.
func (p *Pool) HealthLoop(ctx context.Context) {
	t := time.NewTicker(p.cfg.HealthInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.CheckAll(ctx)
		}
	}
}

// CheckAll probes every instance concurrently (bounded).
func (p *Pool) CheckAll(ctx context.Context) {
	p.mu.RLock()
	insts := append([]*Instance(nil), p.instances...)
	p.mu.RUnlock()

	sem := make(chan struct{}, 12)
	var wg sync.WaitGroup
	for _, in := range insts {
		wg.Add(1)
		sem <- struct{}{}
		go func(in *Instance) {
			defer wg.Done()
			defer func() { <-sem }()
			p.check(ctx, in)
		}(in)
	}
	wg.Wait()

	h, j := 0, 0
	for _, in := range insts {
		s := in.snap()
		if s.Healthy {
			h++
		}
		if s.Healthy && s.JSONOK {
			j++
		}
	}
	log.Printf("pool: health check done — %d/%d healthy, %d json-capable", h, len(insts), j)
}

// check probes one instance with a cheap JSON query and records the outcome.
func (p *Pool) check(ctx context.Context, in *Instance) {
	cctx, cancel := context.WithTimeout(ctx, p.cfg.HealthTimeout)
	defer cancel()
	start := time.Now()
	results, err := p.querySearx(cctx, in, "openai", p.cfg.DefaultEngines)
	lat := time.Since(start).Milliseconds()

	in.mu.Lock()
	in.lastChecked = time.Now()
	if err != nil {
		in.healthy = false
		in.jsonOK = false
		in.fails++
	} else {
		in.healthy = true
		in.jsonOK = len(results) >= 0 // valid JSON decoded (0 results still = JSON works)
		in.latencyMs = lat
		if in.fails > 0 {
			in.fails--
		}
	}
	in.mu.Unlock()
}

// Pick returns up to n healthy, JSON-capable instances ordered by a freshness/
// latency score, honoring a politeness gap so we never hammer one instance.
func (p *Pool) Pick(n int) []*Instance {
	p.mu.RLock()
	insts := append([]*Instance(nil), p.instances...)
	p.mu.RUnlock()

	now := time.Now()
	type cand struct {
		in    *Instance
		score float64
	}
	var cands []cand
	for _, in := range insts {
		in.mu.Lock()
		ok := in.healthy && in.jsonOK && now.Sub(in.lastUsed) >= p.cfg.PerInstanceGap
		lat := in.latencyMs
		fails := in.fails
		inflight := atomic.LoadInt32(&in.inFlight)
		in.mu.Unlock()
		if !ok {
			continue
		}
		// lower is better: latency + fail penalty + in-flight penalty
		score := float64(lat) + float64(fails)*500 + float64(inflight)*2000
		cands = append(cands, cand{in, score})
	}
	sort.Slice(cands, func(a, b int) bool { return cands[a].score < cands[b].score })

	out := make([]*Instance, 0, n)
	for _, c := range cands {
		if len(out) >= n {
			break
		}
		c.in.mu.Lock()
		c.in.lastUsed = now
		c.in.mu.Unlock()
		out = append(out, c.in)
	}
	return out
}

// querySearx hits one instance's JSON API and returns normalized results.
func (p *Pool) querySearx(ctx context.Context, in *Instance, query, engines string) ([]Result, error) {
	atomic.AddInt32(&in.inFlight, 1)
	defer atomic.AddInt32(&in.inFlight, -1)

	q := url.Values{}
	q.Set("q", query)
	q.Set("format", "json")
	if engines != "" {
		q.Set("engines", engines)
	}
	u := in.URL + "/search?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; yupcha-searxpool/1.0)")
	req.Header.Set("Accept", "application/json")

	resp, err := in.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, &httpError{resp.StatusCode}
	}
	var sr searxResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&sr); err != nil {
		return nil, err // non-JSON (instance has JSON disabled) → treated as failure
	}
	return sr.normalize(), nil
}

type httpError struct{ code int }

func (e *httpError) Error() string { return "http " + http.StatusText(e.code) }

// Snapshot returns the current pool state for the /pool endpoint.
func (p *Pool) Snapshot() []instSnap {
	p.mu.RLock()
	insts := append([]*Instance(nil), p.instances...)
	p.mu.RUnlock()
	out := make([]instSnap, 0, len(insts))
	for _, in := range insts {
		out = append(out, in.snap())
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Healthy != out[b].Healthy {
			return out[a].Healthy
		}
		return out[a].LatencyMs < out[b].LatencyMs
	})
	return out
}
