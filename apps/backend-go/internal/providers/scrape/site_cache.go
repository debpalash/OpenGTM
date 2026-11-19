package scrape

import (
	"context"
	"sync"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// The site providers (website_scraper, deep_scraper) return every field they find on the lead's website, and the
// caller picks the one it asked for (Output.Value is fields[Input.Field]). A workbook asks for the same lead once
// per column (email, phone, size, description, LinkedIn), so without a memory each column walked the same pages
// again: in the first-run benchmark a site was walked 5 times by deep_scraper and 3 to 8 times by website_scraper,
// each walk 13 to 39 sequential requests or up to four browser renders, which is why they timed out.
//
// siteMemory keeps the finished answer for siteMemoryTTL so the columns of one row, and a quick re-run, share one
// walk. It remembers a walk that found nothing too, but never one that ended because the caller's context ended:
// that says nothing about the site. The TTL is short on purpose: a user who re-runs a cell minutes later gets a
// fresh fetch.
const (
	siteMemoryTTL = 2 * time.Minute
	siteMemoryMax = 512
)

type siteEntry struct {
	out providers.Output
	exp time.Time
}

type siteMemory struct {
	mu sync.Mutex
	m  map[string]siteEntry
}

func (s *siteMemory) reset() {
	s.mu.Lock()
	s.m = nil
	s.mu.Unlock()
}

func (s *siteMemory) get(key string) (providers.Output, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[key]
	if !ok || !time.Now().Before(e.exp) {
		delete(s.m, key)
		return providers.Output{}, false
	}
	return e.out, true
}

func (s *siteMemory) put(key string, out providers.Output) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]siteEntry{}
	}
	if len(s.m) >= siteMemoryMax {
		now := time.Now()
		for k, e := range s.m {
			if !now.Before(e.exp) {
				delete(s.m, k)
			}
		}
		for k := range s.m {
			if len(s.m) < siteMemoryMax {
				break
			}
			delete(s.m, k)
		}
	}
	s.m[key] = siteEntry{out: out, exp: time.Now().Add(siteMemoryTTL)}
}

var (
	deepMemory    siteMemory
	websiteMemory siteMemory
)

func resetSiteMemory() {
	deepMemory.reset()
	websiteMemory.reset()
}

// forField is the remembered answer as a fresh Output for in.Field: its own copy of the fields and the value of the
// field asked for.
func forField(out providers.Output, in providers.Input) providers.Output {
	cp := out
	if out.Extra != nil {
		cp.Extra = providers.NewObj()
		for _, k := range out.Extra.Keys() {
			v, _ := out.Extra.Get(k)
			cp.Extra.Set(k, v)
		}
		cp.Value = nil
		if v, ok := cp.Extra.Get(in.Field); ok {
			cp.Value = v
		}
	}
	return cp
}

// remember runs compute for key unless the memory has the answer. The answer is stored only when the walk finished
// on its own (no error, caller's context alive).
func (s *siteMemory) remember(ctx context.Context, key string, in providers.Input,
	compute func() (providers.Output, error)) (providers.Output, error) {
	if out, ok := s.get(key); ok {
		return forField(out, in), nil
	}
	out, err := compute()
	if err == nil && ctx.Err() == nil {
		s.put(key, forField(out, in))
	}
	return out, err
}
