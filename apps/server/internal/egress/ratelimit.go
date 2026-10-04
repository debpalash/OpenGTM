package egress

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/publicsuffix"
)

// RateLimitError is returned when the next slot for a domain is later than
// the caller's deadline, so the request fails fast instead of sleeping past it.
type RateLimitError struct {
	Domain string
	Wait   time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("rate limit: next request to %s allowed in %s, after the deadline", e.Domain, e.Wait.Round(time.Millisecond))
}

// domainLimiter spaces requests per registrable domain (eTLD+1), so
// subdomain sharding cannot bypass the limit. It is a token bucket with a
// burst of one, shared by every goroutine using the Client.
type domainLimiter struct {
	mu   sync.Mutex
	next map[string]time.Time
	now  func() time.Time
}

func newDomainLimiter(now func() time.Time) *domainLimiter {
	return &domainLimiter{next: map[string]time.Time{}, now: now}
}

// DomainKey returns the rate-limit key for a host.
func DomainKey(host string) string {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if _, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return host
	}
	if d, err := publicsuffix.EffectiveTLDPlusOne(host); err == nil {
		return d
	}
	return host
}

// wait blocks until the domain's next slot. interval is the minimum spacing
// for this request (from the plugin rate and any robots Crawl-delay).
func (l *domainLimiter) wait(ctx context.Context, domain string, interval time.Duration) error {
	l.mu.Lock()
	now := l.now()
	slot := l.next[domain]
	if slot.Before(now) {
		slot = now
	}
	delay := slot.Sub(now)
	if dl, ok := ctx.Deadline(); ok && now.Add(delay).After(dl) {
		l.mu.Unlock()
		return &RateLimitError{Domain: domain, Wait: delay}
	}
	l.next[domain] = slot.Add(interval)
	if len(l.next) > 4096 {
		for k, v := range l.next {
			if v.Before(now) {
				delete(l.next, k)
			}
		}
	}
	l.mu.Unlock()
	if delay <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
