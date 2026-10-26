// Package ratelimit is a slowapi/limits-compatible fixed-window rate limiter.
package ratelimit

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

const maxKeys = 100_000

// Key mirrors core/ratelimit.py workspace_key: workspace, then bearer token
// fingerprint, then client IP.
func Key(r *http.Request) string {
	if ws := r.Header.Get("X-Workspace-Id"); ws != "" {
		return "ws:" + ws
	}
	if auth := r.Header.Get("Authorization"); len(auth) >= 7 && strings.EqualFold(auth[:7], "bearer ") {
		if tok := strings.TrimSpace(auth[7:]); tok != "" {
			if len(tok) > 32 {
				tok = tok[:32]
			}
			return "tok:" + tok
		}
	}
	return "ip:" + remoteHost(r)
}

func remoteHost(r *http.Request) string {
	if strings.EqualFold(os.Getenv("TRUST_PROXY"), "true") {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			if first = strings.TrimSpace(first); first != "" {
				return first
			}
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	if r.RemoteAddr != "" {
		return r.RemoteAddr
	}
	return "127.0.0.1"
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
}

func New(now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{now: now, hits: map[string]*window{}}
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
			if !l.hit(ns+Key(r), n, period) {
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
