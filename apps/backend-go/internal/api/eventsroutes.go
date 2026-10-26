package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadevents"
)

const (
	defaultSSEMaxStreams = 1000
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
	h := &eventsRoute{d: d, sem: make(chan struct{}, max), relay: leadevents.NewRelay(d.Redis, sseHeartbeat)}
	r.Handle("GET /api/events", http.HandlerFunc(h.serve))
}

type eventsRoute struct {
	d     Deps
	sem   chan struct{} // process-wide stream slots
	relay *leadevents.Relay
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
