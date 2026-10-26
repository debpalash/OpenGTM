// Package ratelimit is a slowapi/limits-compatible fixed-window rate limiter.
package ratelimit

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
)

const maxKeys = 100_000

type keyOverride struct{}

// WithKey makes the limiter use key for requests carrying the returned context. Public (unauthenticated)
// routes use it to bucket by a value they derived themselves (a verified token, their own client-IP logic);
// no request header ever chooses a bucket.
func WithKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, keyOverride{}, key)
}

// key is the bucket for r, from server-side state only (plan 09 final security M2; slowapi's
// workspace_key read the raw X-Workspace-Id and a bearer prefix, both client-chosen):
//   - an explicit WithKey,
//   - the authenticated workspace (identity.Workspace has already checked membership),
//   - the authenticated user,
//   - else the client IP: X-Real-IP only from a trusted proxy peer, never X-Forwarded-For.
func (l *Limiter) key(r *http.Request) string {
	ctx := r.Context()
	if k, ok := ctx.Value(keyOverride{}).(string); ok && k != "" {
		return k
	}
	if wc, ok := identity.WorkspaceFrom(ctx); ok && wc.WorkspaceID != "" {
		return "ws:" + wc.WorkspaceID
	}
	if p, ok := identity.PrincipalFrom(ctx); ok && p.User.ID != 0 {
		return "user:" + strconv.Itoa(int(p.User.ID))
	}
	return "ip:" + ClientIP(r, l.trusted)
}

type window struct {
	count   int
	expires time.Time
}

// Limiter holds fixed-window counters. Like limits' memory storage, a window
// starts at the first hit for a key and expires one period later.
type Limiter struct {
	now  func() time.Time
	mu   sync.Mutex
	hits map[string]*window
	seq  int
	// trusted are the proxy networks whose X-Real-IP is believed (TRUST_PROXY_CIDRS, default: loopback and private ranges).
	trusted []netip.Prefix
}

// SetTrustedProxies replaces the trusted proxy networks (see ParseTrustedProxies).
func (l *Limiter) SetTrustedProxies(p []netip.Prefix) { l.trusted = p }

func New(now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{now: now, hits: map[string]*window{}, trusted: ParseTrustedProxies(os.Getenv("TRUST_PROXY_CIDRS"), nil)}
}

var units = map[string]struct {
	d    time.Duration
	name string
}{
	"second": {time.Second, "second"},
	"minute": {time.Minute, "minute"},
	"hour":   {time.Hour, "hour"},
	"day":    {24 * time.Hour, "day"},
}

func parseSpec(spec string) (n int, period time.Duration, unit string, err error) {
	num, u, ok := strings.Cut(strings.TrimSpace(spec), "/")
	if !ok {
		return 0, 0, "", fmt.Errorf("ratelimit: bad spec %q", spec)
	}
	n, e := strconv.Atoi(strings.TrimSpace(num))
	un, found := units[strings.TrimSpace(u)]
	if e != nil || n <= 0 || !found {
		return 0, 0, "", fmt.Errorf("ratelimit: bad spec %q", spec)
	}
	return n, un.d, un.name, nil
}

// Limit returns middleware allowing n requests per window per Key. It panics
// on an invalid spec (programmer error at wiring time). Each call has its own
// budget, like a slowapi per-route limit.
func (l *Limiter) Limit(spec string) httpx.Middleware {
	n, period, unit, err := parseSpec(spec)
	if err != nil {
		panic(err)
	}
	l.mu.Lock()
	l.seq++
	ns := strconv.Itoa(l.seq) + "|"
	l.mu.Unlock()
	body := map[string]string{"error": fmt.Sprintf("Rate limit exceeded: %d per 1 %s", n, unit)}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !l.hit(ns+l.key(r), n, period) {
				httpx.WriteJSON(w, http.StatusTooManyRequests, body)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func (l *Limiter) hit(key string, limit int, period time.Duration) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	win := l.hits[key]
	if win == nil || !now.Before(win.expires) {
		if win == nil && len(l.hits) >= maxKeys {
			l.makeRoom(now)
		}
		win = &window{expires: now.Add(period)}
		l.hits[key] = win
	}
	win.count++
	return win.count <= limit
}

// makeRoom sweeps expired windows; if the map is still full it drops the
// window closest to expiry so memory stays bounded.
func (l *Limiter) makeRoom(now time.Time) {
	for k, w := range l.hits {
		if !now.Before(w.expires) {
			delete(l.hits, k)
		}
	}
	if len(l.hits) < maxKeys {
		return
	}
	var oldK string
	var oldT time.Time
	for k, w := range l.hits {
		if oldK == "" || w.expires.Before(oldT) {
			oldK, oldT = k, w.expires
		}
	}
	delete(l.hits, oldK)
}
