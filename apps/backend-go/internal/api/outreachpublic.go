package api

import (
	"html"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachstore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachtoken"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// The public routes are unauthenticated: the signed token (unsubscribe) or the platform secret (bounce webhook) is the only
// credential, and neither the token, the address nor the secret is ever logged or echoed except where Python echoes it
// (the landing page, HTML-escaped).
const (
	unsubRateMax     = 20 // per 60 s, per client IP and per token prefix (Python _UNSUB_MAX / _UNSUB_WINDOW)
	unsubRateWindow  = 60 * time.Second
	unsubRateMaxKeys = 100_000
	unsubTokenPrefix = 32

	unsubNotConfigured = "unsubscribe not configured"
	unsubInvalidHTML   = "<h2>Invalid unsubscribe link</h2>"
)

type pubRoutes struct {
	d      Deps
	signer *outreachtoken.Signer // nil when the master key cannot be resolved (503)
	lim    *unsubLimiter
}

// registerOutreachPublic registers the 3 public unsubscribe and bounce webhook routes (Task 25). No authentication
// middleware and no CSRF check: RFC 8058 one-click must work from a mail client.
func registerOutreachPublic(r *httpx.Router, d Deps) {
	h := &pubRoutes{d: d, lim: newUnsubLimiter(unsubRateMax, unsubRateWindow, unsubRateMaxKeys, nil)}
	get := d.getenv()
	master, err := secrets.MasterKeyBytes(get("SECRETS_MASTER_KEY"), d.Config.SecretKey,
		strings.Contains(d.Config.SecretKey, config.InsecureDefaultSecretKey), config.IsDevEnv(d.Config.AppEnv))
	if err != nil {
		d.logger().Warn("outreach public: unsubscribe disabled, no usable master key")
	} else {
		h.signer = outreachtoken.NewSigner(master, d.Flags.Outreach.PublicBaseURL, d.Flags.Outreach.UnsubTTLDays, nil)
	}
	r.Handle("GET /api/outreach/unsubscribe", http.HandlerFunc(h.landing))
	r.Handle("POST /api/outreach/unsubscribe", http.HandlerFunc(h.unsubscribe))
}

// pubQueryLast is the value FastAPI/Starlette gives a scalar query parameter: the last one, "" when absent.
func pubQueryLast(r *http.Request, name string) string {
	v := r.URL.Query()[name]
	if len(v) == 0 {
		return ""
	}
	return v[len(v)-1]
}

// landingHTML is Python's confirmation page with the two reflected values HTML/attribute-escaped (D5); the markup
// is byte-identical to Python's unless a value contains & < > " '.
func landingHTML(email, token string) string {
	return "<h2>Unsubscribe</h2><p>Click below to stop receiving emails at " + html.EscapeString(email) + ".</p>" +
		`<form method="post" action="/api/outreach/unsubscribe?token=` + html.EscapeString(token) + `">` +
		`<button type="submit">Unsubscribe</button></form>`
}

func writeHTML(w http.ResponseWriter, status int, body string) {
	hd := w.Header()
	hd.Set("Content-Type", "text/html; charset=utf-8")
	hd.Set("Cache-Control", "no-store") // the URL carries a credential
	hd.Set("Referrer-Policy", "no-referrer")
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Content-Security-Policy", "default-src 'none'; form-action 'self'; frame-ancestors 'none'")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func (h *pubRoutes) fail(w http.ResponseWriter, what string, err error) {
	h.d.logger().Error("outreach public: request failed", "op", what, "err_type", errTypeName(err))
	httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
}

// landing renders the confirmation page for a signed link, expired or not (Python allow_expired=True: the POST is
// what refuses an expired token).
func (h *pubRoutes) landing(w http.ResponseWriter, r *http.Request) {
	if h.signer == nil {
		httpx.Detail(w, http.StatusServiceUnavailable, unsubNotConfigured)
		return
	}
	token := pubQueryLast(r, "token")
	p, ok := h.signer.Verify(token, true)
	if !ok {
		writeHTML(w, http.StatusBadRequest, unsubInvalidHTML)
		return
	}
	writeHTML(w, http.StatusOK, landingHTML(p.Email, token))
}

func (h *pubRoutes) unsubscribe(w http.ResponseWriter, r *http.Request) {
	token := pubQueryLast(r, "token")
	prefix := token
	if rs := []rune(token); len(rs) > unsubTokenPrefix {
		prefix = string(rs[:unsubTokenPrefix])
	}
	// IP first, then token, short-circuiting like Python (the token budget is not spent by a refused client).
	if h.lim.limited("ip:"+publicClientIP(r, h.d.Config.TrustProxy)) || h.lim.limited("tok:"+prefix) {
		httpx.Detail(w, http.StatusTooManyRequests, "rate limited")
		return
	}
	if h.signer == nil {
		httpx.Detail(w, http.StatusServiceUnavailable, unsubNotConfigured)
		return
	}
	p, ok := h.signer.Verify(token, false)
	if !ok {
		httpx.Detail(w, http.StatusBadRequest, "invalid or expired token")
		return
	}
	source := p.Seq
	if source == "" {
		source = "unsubscribe"
	}
	err := tenancy.InWorkspace(r.Context(), h.d.Pool, p.WS, func(tx pgx.Tx) error {
		_, e := outreachstore.AddSuppression(r.Context(), tx, p.WS, p.Email, "unsubscribe", source, true)
		return e
	})
	if err != nil {
		h.fail(w, "unsubscribe", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "unsubscribed"})
}

// publicClientIP is the rate-limit identity. Behind nginx every request arrives from the proxy, and nginx overwrites
// X-Real-IP with $remote_addr (infra/nginx/default.conf), so that header is the client when TRUST_PROXY is on. X-Forwarded-For
// is never used (nginx appends to a client-supplied value), and without TRUST_PROXY the socket peer is the client, so a
// header cannot be used to dodge the limit.
func publicClientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if a, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get("X-Real-IP"))); err == nil {
			return a.Unmap().WithZone("").String()
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	if r.RemoteAddr != "" {
		return r.RemoteAddr
	}
	return "?"
}

// unsubLimiter is Python's _rate_limited: a sliding window of timestamps per key, hardened with a bounded key set and
// bounded per-key history (an attacker choosing endless tokens cannot grow memory). When the map is full a random key is
// evicted, so a flood can reset counters but never exhausts memory or locks real unsubscribes out.
type unsubLimiter struct {
	mu        sync.Mutex
	now       func() time.Time
	max       int
	window    time.Duration
	maxKeys   int
	hits      map[string][]int64
	lastSweep time.Time
}

func newUnsubLimiter(max int, window time.Duration, maxKeys int, now func() time.Time) *unsubLimiter {
	if now == nil {
		now = time.Now
	}
	return &unsubLimiter{now: now, max: max, window: window, maxKeys: maxKeys, hits: map[string][]int64{}, lastSweep: now()}
}

// limited records a hit and reports whether the key is over its budget (the hit counts even when refused, as in Python).
func (l *unsubLimiter) limited(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if now.Sub(l.lastSweep) >= l.window {
		l.sweep(now)
	}
	ts, known := l.hits[key]
	if !known && len(l.hits) >= l.maxKeys {
		for k := range l.hits { // map iteration order is random: evict one arbitrary key in O(1)
			delete(l.hits, k)
			break
		}
	}
	cutoff := now.Add(-l.window).UnixNano()
	keep := ts[:0]
	for _, t := range ts {
		if t > cutoff {
			keep = append(keep, t)
		}
	}
	keep = append(keep, now.UnixNano())
	if len(keep) > l.max+1 {
		keep = append(keep[:0], keep[len(keep)-l.max-1:]...)
	}
	l.hits[key] = keep
	return len(keep) > l.max
}

func (l *unsubLimiter) sweep(now time.Time) {
	l.lastSweep = now
	cutoff := now.Add(-l.window).UnixNano()
	for k, ts := range l.hits {
		if len(ts) == 0 || ts[len(ts)-1] <= cutoff {
			delete(l.hits, k)
		}
	}
}

func (l *unsubLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.hits)
}

func (l *unsubLimiter) maxEntry() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, ts := range l.hits {
		n = max(n, len(ts))
	}
	return n
}
