package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
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
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// The public routes are unauthenticated: the signed token (unsubscribe) or the platform secret (bounce webhook) is the only
// credential, and neither the token, the address nor the secret is ever logged or echoed except where Python echoes it
// (the landing page, HTML-escaped).
const (
	unsubRateMax     = 20 // per 60 s, per client IP and per token prefix (Python _UNSUB_MAX / _UNSUB_WINDOW)
	unsubRateWindow  = 60 * time.Second
	unsubRateMaxKeys = 100_000
	unsubTokenPrefix = 32
	whMaxBodyBytes   = 64 << 10
	whMessageIDMax   = 998 // RFC 5322 line limit
	whEventMax       = 256
	whImapUIDWidth   = 64 // outreach_inbound_messages.imap_uid is VARCHAR(64)

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
	r.Handle("POST /api/outreach/webhooks/bounce", http.HandlerFunc(h.bounce))
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

// secretsEqual compares through fixed-length digests in constant time, so neither the length nor a common prefix of
// the configured secret leaks, and a non-ASCII header value cannot raise (Python compare_digest would).
func secretsEqual(a, b string) bool {
	ha, hb := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

// pubBool is pydantic's lax bool validation.
func pubBool(v any, loc []any) (bool, *httpx.FieldError) {
	bad := func(typ, msg string) (bool, *httpx.FieldError) {
		e := seqFE(typ, msg, loc, v, nil)
		return false, &e
	}
	const unclear = "Input should be a valid boolean, unable to interpret input"
	switch t := v.(type) {
	case bool:
		return t, nil
	case json.Number:
		switch string(t) {
		case "0", "0.0":
			return false, nil
		case "1", "1.0":
			return true, nil
		}
		return bad("bool_parsing", unclear)
	case string:
		switch strings.ToLower(t) {
		case "0", "off", "f", "false", "n", "no":
			return false, nil
		case "1", "on", "t", "true", "y", "yes":
			return true, nil
		}
		return bad("bool_parsing", unclear)
	}
	return bad("bool_type", "Input should be a valid boolean")
}

// bounceEvent is the validated BounceEvent{message_id: str, event: str = "bounce", hard: bool = True}.
type bounceEvent struct {
	MessageID, Event string
	Hard             bool
}

func parseBounceEvent(raw []byte) (bounceEvent, []httpx.FieldError) {
	ev := bounceEvent{Event: "bounce", Hard: true}
	obj, errs := wbcrud.DecodeBodyObject(raw)
	if errs != nil {
		return ev, errs
	}
	if v, present := obj.Get("message_id"); !present {
		errs = append(errs, seqFE("missing", "Field required", seqLoc("message_id"), obj, nil))
	} else if s, e := seqString(v, seqLoc("message_id"), whMessageIDMax); e != nil {
		errs = append(errs, *e)
	} else {
		ev.MessageID = s
	}
	if v, present := obj.Get("event"); present {
		if s, e := seqString(v, seqLoc("event"), whEventMax); e != nil {
			errs = append(errs, *e)
		} else {
			ev.Event = s
		}
	}
	if v, present := obj.Get("hard"); present {
		if b, e := pubBool(v, seqLoc("hard")); e != nil {
			errs = append(errs, *e)
		} else {
			ev.Hard = b
		}
	}
	return ev, errs
}

// bounceKind maps the ESP event to the shared bounce kind (Python: complaint, else hard, else soft).
func bounceKind(ev bounceEvent) string {
	switch {
	case ev.Event == "complaint":
		return "complaint"
	case ev.Hard:
		return "hard"
	}
	return "soft"
}

// ledgerUID keys the inbound ledger row by the message id; the column is 64 characters wide, so a longer id is keyed by
// its digest rather than failing the insert.
func ledgerUID(messageID string) string {
	if len([]rune(messageID)) <= whImapUIDWidth {
		return messageID
	}
	sum := sha256.Sum256([]byte(messageID))
	return "sha256:" + hex.EncodeToString(sum[:])[:whImapUIDWidth-len("sha256:")]
}

// bounce is the platform-authenticated bounce/complaint hook. Order: the platform secret (before anything else is read),
// the body, the SECURITY DEFINER message-id lookup (the only statement that spans tenants; it returns a routing tuple),
// then everything else in the owning workspace, in one transaction so the ledger insert gates apply_bounce exactly once.
func (h *pubRoutes) bounce(w http.ResponseWriter, r *http.Request) {
	secret := h.d.Flags.Outreach.BounceWebhookSecret.Reveal()
	if secret == "" {
		httpx.Detail(w, http.StatusServiceUnavailable, "bounce webhook not configured")
		return
	}
	if !secretsEqual(r.Header.Get("X-Webhook-Secret"), secret) {
		httpx.Detail(w, http.StatusForbidden, "invalid signature")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, whMaxBodyBytes+1))
	if err != nil {
		httpx.Detail(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(raw) > whMaxBodyBytes {
		httpx.Detail(w, http.StatusRequestEntityTooLarge, "Request body too large")
		return
	}
	ev, errs := parseBounceEvent(raw)
	if errs != nil {
		httpx.ValidationError(w, errs...)
		return
	}
	mid := strings.TrimSpace(ev.MessageID)
	ref, err := outreachstore.LookupSendAnyWorkspace(r.Context(), h.d.Pool, mid)
	if err != nil {
		h.fail(w, "bounce lookup", err)
		return
	}
	if ref == nil {
		httpx.Detail(w, http.StatusNotFound, "message not found")
		return
	}
	kind := bounceKind(ev)
	sendID := ref.SendID
	var enrollment *int
	if ref.EnrollmentID != nil {
		n := int(*ref.EnrollmentID)
		enrollment = &n
	}
	duplicate := false
	err = tenancy.InWorkspace(r.Context(), h.d.Pool, ref.WorkspaceID, func(tx pgx.Tx) error {
		created, e := outreachstore.RecordInboundProcessed(r.Context(), tx, outreachstore.InboundIn{
			WorkspaceID: ref.WorkspaceID, IMAPUID: ledgerUID(mid), UIDValidity: "webhook", SourceMessageID: mid,
			MatchedSendID: &sendID, Kind: kind, Recipient: ref.ToEmail, Diagnostic: "webhook",
		})
		if e != nil {
			return e
		}
		if !created {
			duplicate = true
			return nil
		}
		return outreachstore.ApplyBounce(r.Context(), tx, outreachstore.BounceApply{
			WorkspaceID: ref.WorkspaceID, SendID: &sendID, SequenceID: ref.SequenceID, EnrollmentID: enrollment,
			ToEmail: ref.ToEmail, Kind: kind, Diagnostic: "webhook", Source: "webhook",
		}, outreachstore.ThresholdsFrom(h.d.Flags.Outreach))
	})
	if err != nil {
		h.fail(w, "bounce apply", err)
		return
	}
	if duplicate {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "duplicate"})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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
