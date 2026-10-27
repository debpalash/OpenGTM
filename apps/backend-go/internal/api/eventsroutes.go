package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadevents"
)

const (
	defaultSSEMaxStreams = 1000
	defaultSSEPerUser    = 10
	defaultSSEPerWS      = 200
	defaultSSELifetime   = 30 * time.Minute
	sseWriteTimeout      = 10 * time.Second // per frame; a stalled client is dropped
	ssoRequiredDetail    = "This workspace requires SSO. Sign in with your organization identity."
)

// sseHeartbeat is the silence interval before a heartbeat frame (Python: 15 s).
// A variable so tests can shorten it.
var sseHeartbeat = 15 * time.Second

// registerEvents mounts GET /api/events and POST /api/events/ticket. GET deliberately has no
// Authenticate or Workspace middleware: EventSource cannot send headers, so it authenticates
// with ?ticket= (eventsticket.go, S-L23) or, until SSE_QUERY_TOKEN_UNTIL, the deprecated
// ?token= access token (identity.AuthenticateQueryTokenCtx). The handler never logs the raw
// query. Redis may be nil (heartbeats only; decision D3).
func registerEvents(r *httpx.Router, d Deps) {
	max := d.SSEMaxStreams
	if max <= 0 {
		max = defaultSSEMaxStreams
	}
	sem := d.SSEStreamSem
	if sem == nil {
		sem = make(chan struct{}, max)
	}
	h := &eventsRoute{d: d, sem: sem, relay: leadevents.NewRelay(d.Redis, sseHeartbeat),
		perUser: orDefault(d.SSEMaxPerUser, defaultSSEPerUser), perWS: orDefault(d.SSEMaxPerWorkspace, defaultSSEPerWS),
		byUser: map[int32]int{}, byWS: map[string]int{}, tickets: newTicketer(d.Signer.Secret, d.Redis)}
	var ok bool
	if h.tokenUntil, ok = queryTokenUntil(d.getenv()("SSE_QUERY_TOKEN_UNTIL")); !ok {
		log := d.Log
		if log == nil {
			log = slog.Default()
		}
		log.Error("SSE_QUERY_TOKEN_UNTIL is not RFC 3339 or YYYY-MM-DD: ?token= on /api/events is refused")
	}
	r.Handle("GET /api/events", http.HandlerFunc(h.serve))
	r.Handle("POST /api/events/ticket", http.HandlerFunc(h.ticket), identity.Authenticate(d.Signer, d.Identity), identity.Workspace(d.Identity))
}

type eventsRoute struct {
	d     Deps
	sem   chan struct{} // process-wide stream slots
	relay *leadevents.Relay

	tickets    *ticketer
	tokenUntil time.Time // ?token= is accepted before this instant (zero: never)

	perUser, perWS int
	mu             sync.Mutex
	byUser         map[int32]int
	byWS           map[string]int
}

func orDuration(v, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	return v
}

func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

// acquire takes a per-user and a per-workspace slot (a user or workspace cannot starve the
// process-wide pool). It returns a release func, or the 429 detail that explains the refusal.
func (h *eventsRoute) acquire(uid int32, ws string) (release func(), refusal string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.byUser[uid] >= h.perUser {
		return nil, "Too many event streams for this user"
	}
	if h.byWS[ws] >= h.perWS {
		return nil, "Too many event streams for this workspace"
	}
	h.byUser[uid]++
	h.byWS[ws]++
	return func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.byUser[uid]--; h.byUser[uid] <= 0 {
			delete(h.byUser, uid)
		}
		if h.byWS[ws]--; h.byWS[ws] <= 0 {
			delete(h.byWS, ws)
		}
	}, ""
}

func (h *eventsRoute) serve(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	uid, ws, amr, ok := h.authenticate(w, r, q)
	if !ok {
		return
	}
	var err error
	if ws == "" {
		if ws, err = h.d.Identity.ActiveWorkspaceID(ctx, uid); err != nil {
			httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
			return
		}
	}
	member := false
	if ws != "" {
		if member, err = h.d.Identity.IsMember(ctx, ws, uid); err != nil {
			httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
			return
		}
	}
	if !member { // fail closed; never reveal whether the workspace exists
		httpx.Detail(w, http.StatusForbidden, "Workspace access denied")
		return
	}
	role, err := h.d.Identity.MemberRole(ctx, ws, uid)
	if err != nil {
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	enforce, err := h.d.Identity.EnforceSSO(ctx, h.d.Pool, ws)
	if err != nil {
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	if enforce && role != "owner" && !slices.Contains(amr, "sso") {
		httpx.Detail(w, http.StatusForbidden, ssoRequiredDetail)
		return
	}

	release, refusal := h.acquire(uid, ws)
	if release == nil {
		httpx.Detail(w, http.StatusTooManyRequests, refusal)
		return
	}
	defer release()
	select {
	case h.sem <- struct{}{}:
		defer func() { <-h.sem }()
	default:
		httpx.Detail(w, http.StatusServiceUnavailable, "Too many event streams")
		return
	}

	if sd := h.d.ShutdownCtx; sd != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		stop := context.AfterFunc(sd, cancel)
		defer stop()
	}

	// Max lifetime: authorization is checked once at connect, so end the stream after a while;
	// EventSource reconnects and re-authenticates (token expiry, membership, SSO).
	var lcancel context.CancelFunc
	ctx, lcancel = context.WithTimeout(ctx, orDuration(h.d.SSEMaxLifetime, defaultSSELifetime))
	defer lcancel()

	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream; charset=utf-8")
	hd.Set("Cache-Control", "no-cache")
	hd.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	deadline := func() { _ = rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout)) } // ErrNotSupported ignored
	flush := func() { deadline(); _ = rc.Flush() }
	flush()
	_ = h.relay.Stream(ctx, ws, deadlineWriter{w: w, set: deadline}, flush)
}

// authenticate resolves the caller of GET /api/events from ?ticket= (bound workspace; a different
// workspace_id is 403) or, inside the deprecation window, ?token= (workspace_id or the active one).
// It writes the refusal itself and reports ok=false.
func (h *eventsRoute) authenticate(w http.ResponseWriter, r *http.Request, q url.Values) (uid int32, ws string, amr []string, ok bool) {
	ctx := r.Context()
	if tk := q.Get("ticket"); tk != "" {
		c, err := h.tickets.redeem(ctx, tk)
		if errors.Is(err, errBadTicket) {
			httpx.Detail(w, http.StatusUnauthorized, invalidTicketDetail)
			return 0, "", nil, false
		}
		if err != nil {
			httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
			return 0, "", nil, false
		}
		if want := q.Get("workspace_id"); want != "" && want != c.WS {
			httpx.Detail(w, http.StatusForbidden, "Workspace access denied")
			return 0, "", nil, false
		}
		u, err := h.d.Identity.ByID(ctx, c.UID)
		if errors.Is(err, identity.ErrNotFound) || (err == nil && !u.IsActive) {
			httpx.Detail(w, http.StatusUnauthorized, invalidTicketDetail)
			return 0, "", nil, false
		}
		if err != nil {
			httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
			return 0, "", nil, false
		}
		return u.ID, c.WS, c.AMR, true
	}
	tok := q.Get("token")
	if tok != "" && !ticketNow().Before(h.tokenUntil) {
		httpx.Detail(w, http.StatusUnauthorized, queryTokenGoneDetail)
		return 0, "", nil, false
	}
	p, err := identity.AuthenticateQueryTokenCtx(ctx, h.d.Signer, h.d.Identity, tok, false)
	if err != nil {
		var qe *identity.QueryTokenError
		if errors.As(err, &qe) {
			httpx.Detail(w, qe.Status, qe.Detail)
			return 0, "", nil, false
		}
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
		return 0, "", nil, false
	}
	return p.User.ID, q.Get("workspace_id"), p.Claims.AMR, true
}

// deadlineWriter arms a fresh write deadline before every frame so a client
// that stops reading cannot pin a goroutine and a stream slot.
type deadlineWriter struct {
	w   http.ResponseWriter
	set func()
}

func (d deadlineWriter) Write(b []byte) (int, error) { d.set(); return d.w.Write(b) }
