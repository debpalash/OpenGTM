package main

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Result is one normalized search hit.
type Result struct {
	Title   string   `json:"title"`
	URL     string   `json:"url"`
	Content string   `json:"content"`
	Engines []string `json:"engines,omitempty"`
}

// searxResponse mirrors the bits of SearXNG's JSON we care about.
type searxResponse struct {
	Results []struct {
		URL     string   `json:"url"`
		Title   string   `json:"title"`
		Content string   `json:"content"`
		Engine  string   `json:"engine"`
		Engines []string `json:"engines"`
	} `json:"results"`
}

func (sr searxResponse) normalize() []Result {
	out := make([]Result, 0, len(sr.Results))
	for _, r := range sr.Results {
		if r.URL == "" {
			continue
		}
		engs := r.Engines
		if len(engs) == 0 && r.Engine != "" {
			engs = []string{r.Engine}
		}
		out = append(out, Result{Title: r.Title, URL: r.URL, Content: r.Content, Engines: engs})
	}
	return out
}

// SearchOutcome is what the HTTP layer returns.
type SearchOutcome struct {
	Query         string   `json:"query"`
	Results       []Result `json:"results"`
	InstancesUsed []string `json:"instances_used"`
	ElapsedMs     int64    `json:"elapsed_ms"`
}

// Search fans out the query to several healthy instances concurrently, merges
// and dedups their results, and returns as soon as MinResponses have answered
// (or the budget runs out). This is the core concurrency win: each instance is
// a different IP, so a CAPTCHA on one doesn't sink the query.
func (p *Pool) Search(ctx context.Context, query, engines string, fanout int) SearchOutcome {
	start := time.Now()
	if engines == "" {
		engines = p.cfg.DefaultEngines
	}
	if fanout <= 0 {
		fanout = p.cfg.Fanout
	}
	insts := p.Pick(fanout)

	sctx, cancel := context.WithTimeout(ctx, p.cfg.SearchTimeout)
	defer cancel()

	type res struct {
		inst    string
		results []Result
		err     error
	}
	ch := make(chan res, len(insts))
	var wg sync.WaitGroup
	for _, in := range insts {
		wg.Add(1)
		go func(in *Instance) {
			defer wg.Done()
			tctx, c := context.WithTimeout(sctx, p.cfg.PerTryTimeout)
			defer c()
			r, err := p.querySearx(tctx, in, query, engines)
			if err != nil {
				in.mu.Lock()
				in.fails++
				if in.fails >= 3 {
					in.healthy = false // demote; health loop will re-promote if it recovers
				}
				in.mu.Unlock()
			}
			ch <- res{in.URL, r, err}
		}(in)
	}
	go func() { wg.Wait(); close(ch) }()

	merged := map[string]*Result{}
	var order []string
	var used []string
	ok := 0
	for r := range ch {
		if r.err != nil {
			continue
		}
		used = append(used, r.inst)
		for i := range r.results {
			k := canonURL(r.results[i].URL)
			if k == "" {
				continue
			}
			if ex, seen := merged[k]; seen {
				ex.Engines = mergeStrings(ex.Engines, r.results[i].Engines)
				continue
			}
			rc := r.results[i]
			merged[k] = &rc
			order = append(order, k)
		}
		if ok++; ok >= p.cfg.MinResponses {
			break // enough coverage; let the rest finish in the background (drained by GC)
		}
	}

	out := SearchOutcome{Query: query, InstancesUsed: used, ElapsedMs: time.Since(start).Milliseconds()}
	for _, k := range order {
		if keepResult(*merged[k]) {
			out.Results = append(out.Results, *merged[k])
		}
	}
	return out
}

// keepResult drops the SEO-spam some engines (notably qwant) inject: gibberish
// titles on random short http:// domains with odd TLDs. The reliable signal —
// real company hits are https and/or corroborated by more than one engine,
// spam is single-engine + http + a junk domain. Keep when EITHER holds.
func keepResult(r Result) bool {
	if len(r.Engines) > 1 {
		return true // corroborated by multiple engines → trustworthy
	}
	u, err := url.Parse(r.URL)
	if err != nil || u.Host == "" {
		return false
	}
	if u.Scheme != "https" {
		return false // observed spam was all http://
	}
	if suspiciousHost(u.Host) {
		return false
	}
	return true
}

// suspiciousHost flags short, vowel-starved, odd-TLD registrable names typical of
// throwaway spam domains (e.g. "kupijur.sn", "tedborre.af", "oc.bf").
func suspiciousHost(host string) bool {
	host = strings.ToLower(host)
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	parts := strings.Split(host, ".")
	if len(parts) < 2 {
		return true
	}
	tld := parts[len(parts)-1]
	label := parts[len(parts)-2] // registrable name
	common := map[string]bool{"com": true, "org": true, "net": true, "io": true,
		"co": true, "in": true, "ai": true, "gov": true, "edu": true, "info": true,
		"biz": true, "us": true, "uk": true}
	if common[tld] {
		return false // mainstream TLD → trust it
	}
	// uncommon TLD + a short/vowel-starved label = throwaway spam domain
	vowels := strings.Count(label, "a") + strings.Count(label, "e") +
		strings.Count(label, "i") + strings.Count(label, "o") + strings.Count(label, "u")
	if len(label) <= 8 || (len(label) > 0 && float64(vowels)/float64(len(label)) < 0.25) {
		return true
	}
	return false
}

// canonURL normalizes for dedup: lowercase host, strip trailing slash + fragment.
func canonURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return strings.TrimSpace(raw)
	}
	u.Host = strings.ToLower(u.Host)
	u.Fragment = ""
	p := strings.TrimRight(u.Path, "/")
	if p == "" {
		p = "/"
	}
	u.Path = p
	return u.String()
}

func mergeStrings(a, b []string) []string {
	seen := map[string]bool{}
	for _, s := range a {
		seen[s] = true
	}
	for _, s := range b {
		if !seen[s] {
			a = append(a, s)
			seen[s] = true
		}
	}
	return a
}
