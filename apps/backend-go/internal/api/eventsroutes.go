package api

import (
	"context"
	"errors"
	"net/http"
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

// registerEvents mounts GET /api/events. It deliberately has no Authenticate or
// Workspace middleware: EventSource cannot send headers, so the access token
// rides in ?token= (identity.AuthenticateQueryTokenCtx). The handler never logs
// the raw query. Redis may be nil (heartbeats only; decision D3).
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
		byUser: map[int32]int{}, byWS: map[string]int{}}
	r.Handle("GET /api/events", http.HandlerFunc(h.serve))
}

type eventsRoute struct {
	d     Deps
	sem   chan struct{} // process-wide stream slots
	relay *leadevents.Relay

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
	p, err := identity.AuthenticateQueryTokenCtx(ctx, h.d.Signer, h.d.Identity, q.Get("token"), false)
	if err != nil {
		var qe *identity.QueryTokenError
		if errors.As(err, &qe) {
			httpx.Detail(w, qe.Status, qe.Detail)
			return
		}
		httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	uid := p.User.ID
	ws := q.Get("workspace_id")
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
	if enforce && role != "owner" && !slices.Contains(p.Claims.AMR, "sso") {
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

// deadlineWriter arms a fresh write deadline before every frame so a client
// that stops reading cannot pin a goroutine and a stream slot.
type deadlineWriter struct {
	w   http.ResponseWriter
	set func()
}

func (d deadlineWriter) Write(b []byte) (int, error) { d.set(); return d.w.Write(b) }
