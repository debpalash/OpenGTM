package leadweb

import (
	"context"
	"strings"
	"sync"
	"time"
)

// domainLimits are the legacy per-domain request rates (requests per minute),
// matched in this order on the domain or a subdomain of it.
var domainLimits = []struct {
	domain string
	rpm    int
}{
	{"google.com", 3}, {"google.co.in", 3}, {"maps.google.com", 3}, {"linkedin.com", 2}, {"yelp.com", 5},
	{"glassdoor.com", 3}, {"naukri.com", 5}, {"indeed.com", 5}, {"clutch.co", 5}, {"g2.com", 5},
}

const (
	defaultRPM       = 10
	breakerThreshold = 5                // consecutive failures that trip the breaker
	breakerRecovery  = 15 * time.Minute // how long a tripped domain stays blocked
)

// RateLimiter is the legacy per-domain token bucket with a circuit breaker
// (leadgen/rate_limiter.py). Process-local, safe for concurrent use.
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	order   []string
	now     func() time.Time
}

type bucket struct {
	rpm      float64
	tokens   float64
	last     time.Time
	failures int
	tripped  time.Time // zero = not tripped
}

// NewRateLimiter returns a limiter with the legacy domain limits.
func NewRateLimiter() *RateLimiter {
	return &RateLimiter{buckets: map[string]*bucket{}, now: time.Now}
}

func (l *RateLimiter) bucket(domain string) *bucket {
	if b, ok := l.buckets[domain]; ok {
		return b
	}
	rpm := defaultRPM
	for _, d := range domainLimits {
		if domain == d.domain || strings.HasSuffix(domain, "."+d.domain) {
			rpm = d.rpm
			break
		}
	}
	b := &bucket{rpm: float64(rpm), tokens: float64(rpm), last: l.now()}
	l.buckets[domain] = b
	l.order = append(l.order, domain)
	return b
}

func (l *RateLimiter) tripped(b *bucket) bool {
	if b.tripped.IsZero() {
		return false
	}
	if l.now().After(b.tripped) {
		b.tripped, b.failures = time.Time{}, 0
		return false
	}
	return true
}

func (l *RateLimiter) refill(b *bucket) {
	now := l.now()
	b.tokens = min(b.rpm, b.tokens+now.Sub(b.last).Seconds()*(b.rpm/60))
	b.last = now
}

func (l *RateLimiter) consume(b *bucket) bool {
	if l.tripped(b) {
		return false
	}
	l.refill(b)
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

func (l *RateLimiter) wait(b *bucket) time.Duration {
	if l.tripped(b) {
		return max(0, b.tripped.Sub(l.now()))
	}
	l.refill(b)
	if b.tokens >= 1 {
		return 0
	}
	return time.Duration((1 - b.tokens) / (b.rpm / 60) * float64(time.Second))
}

// TryAcquire takes a token for domain without waiting.
func (l *RateLimiter) TryAcquire(domain string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.consume(l.bucket(domain))
}

// Wait is how long until a request to domain is allowed.
func (l *RateLimiter) Wait(domain string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.wait(l.bucket(domain))
}

// Acquire blocks until a request to domain is allowed (sleeping at most 5 s
// at a time, as legacy) or ctx ends.
func (l *RateLimiter) Acquire(ctx context.Context, domain string) error {
	for {
		l.mu.Lock()
		b := l.bucket(domain)
		if l.consume(b) {
			l.mu.Unlock()
			return nil
		}
		w := min(l.wait(b), 5*time.Second)
		l.mu.Unlock()
		if w <= 0 {
			w = 10 * time.Millisecond
		}
		t := time.NewTimer(w)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// ReportFailure counts a failed request; the fifth in a row trips the breaker.
func (l *RateLimiter) ReportFailure(domain string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.bucket(domain)
	b.failures++
	if b.failures >= breakerThreshold {
		b.tripped = l.now().Add(breakerRecovery)
		b.failures = 0
	}
}

// ReportSuccess resets the failure count.
func (l *RateLimiter) ReportSuccess(domain string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.bucket(domain).failures = 0
}

// IsBlocked reports whether domain's breaker is tripped.
func (l *RateLimiter) IsBlocked(domain string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.tripped(l.bucket(domain))
}

// LimiterStats is the legacy rate_limiter.stats() shape.
type LimiterStats struct {
	TrackedDomains int      `json:"tracked_domains"`
	TrippedDomains []string `json:"tripped_domains"`
}

// Stats lists tracked and tripped domains (tripped in first-seen order).
func (l *RateLimiter) Stats() LimiterStats {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := LimiterStats{TrackedDomains: len(l.buckets), TrippedDomains: []string{}}
	for _, d := range l.order {
		if l.tripped(l.buckets[d]) {
			st.TrippedDomains = append(st.TrippedDomains, d)
		}
	}
	return st
}
