// Package ratelimit is a slowapi/limits-compatible fixed-window rate limiter.
package ratelimit

import (
	"container/heap"
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
//
// authed is true only for the two authenticated identities; an explicit key and a client IP come from
// unauthenticated callers and use the unauthenticated pool (S-L19).
func (l *Limiter) classify(r *http.Request) (key string, authed bool) {
	ctx := r.Context()
	if k, ok := ctx.Value(keyOverride{}).(string); ok && k != "" {
		return k, false
	}
	if wc, ok := identity.WorkspaceFrom(ctx); ok && wc.WorkspaceID != "" {
		return "ws:" + wc.WorkspaceID, true
	}
	if p, ok := identity.PrincipalFrom(ctx); ok && p.User.ID != 0 {
		return "user:" + strconv.Itoa(int(p.User.ID)), true
	}
	return "ip:" + ClientIP(r, l.trusted), false
}

func (l *Limiter) key(r *http.Request) string {
	k, _ := l.classify(r)
	return k
}

type window struct {
	key     string
	count   int
	expires time.Time
	idx     int // position in expiryHeap
}

// expiryHeap orders windows by expiry so eviction never scans the map: the sweep pops expired
// windows from the top and a full map drops the top (the window closest to expiry), each O(log n).
type expiryHeap []*window

func (h expiryHeap) Len() int           { return len(h) }
func (h expiryHeap) Less(i, j int) bool { return h[i].expires.Before(h[j].expires) }
func (h expiryHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].idx, h[j].idx = i, j
}
func (h *expiryHeap) Push(x any) { w := x.(*window); w.idx = len(*h); *h = append(*h, w) }
func (h *expiryHeap) Pop() any {
	old := *h
	n := len(old)
	w := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return w
}

// Limiter holds fixed-window counters. Like limits' memory storage, a window
// starts at the first hit for a key and expires one period later.
type Limiter struct {
	now  func() time.Time
	mu   sync.Mutex
	hits map[string]*window
	exp  expiryHeap
	max  int // key cap (maxKeys unless a test lowers it)
	// parent is set on the per-Limit pools: their key cap follows the parent's.
	parent *Limiter
	// trusted are the proxy networks whose X-Real-IP is believed (TRUST_PROXY_CIDRS, default: loopback only).
	trusted []netip.Prefix
}

// SetMaxKeys lowers or raises the number of live windows kept per pool (default 100k). Every Limit spec has its own
// authenticated and unauthenticated pool, so a key flood on one spec or by anonymous callers cannot evict another
// pool's windows (S-L19). Call before use.
func (l *Limiter) SetMaxKeys(n int) {
	l.mu.Lock()
	l.max = n
	l.mu.Unlock()
}

// SetTrustedProxies replaces the trusted proxy networks (see ParseTrustedProxies).
func (l *Limiter) SetTrustedProxies(p []netip.Prefix) { l.trusted = p }

func New(now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{now: now, hits: map[string]*window{}, max: maxKeys, trusted: ParseTrustedProxies(os.Getenv("TRUST_PROXY_CIDRS"), nil)}
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
	// Separate pools per call and per caller class: a flood of unauthenticated keys fills only the unauthenticated
	// pool of this spec.
	authPool, anonPool := l.newPool(), l.newPool()
	body := map[string]string{"error": fmt.Sprintf("Rate limit exceeded: %d per 1 %s", n, unit)}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			k, authed := l.classify(r)
			pool := anonPool
			if authed {
				pool = authPool
			}
			if !pool.hit(k, n, period) {
				httpx.WriteJSON(w, http.StatusTooManyRequests, body)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func (l *Limiter) newPool() *Limiter {
	return &Limiter{now: l.now, hits: map[string]*window{}, parent: l}
}

func (l *Limiter) hit(key string, limit int, period time.Duration) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now)
	win := l.hits[key]
	if win == nil {
		limit := l.max
		if l.parent != nil {
			limit = l.parent.max
		}
		if len(l.hits) >= limit {
			l.evictOne()
		}
		win = &window{key: key, expires: now.Add(period)}
		l.hits[key] = win
		heap.Push(&l.exp, win)
	}
	win.count++
	return win.count <= limit
}

// sweep drops the windows that have expired. Each window is popped once, so the cost is amortised
// over the inserts that created them and nothing ever scans the whole map.
func (l *Limiter) sweep(now time.Time) {
	for len(l.exp) > 0 && !now.Before(l.exp[0].expires) {
		delete(l.hits, heap.Pop(&l.exp).(*window).key)
	}
}

// evictOne drops the window closest to expiry so memory stays bounded.
func (l *Limiter) evictOne() {
	if len(l.exp) > 0 {
		delete(l.hits, heap.Pop(&l.exp).(*window).key)
	}
}
