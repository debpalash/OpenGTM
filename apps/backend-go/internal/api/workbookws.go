package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/redis/go-redis/v9"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"

	"github.com/jackc/pgx/v5"
)

// wsCloseForbidden is the ASGI close code FastAPI sends before accept. Starlette turns a pre-accept
// close into an HTTP 403 handshake refusal, so that is what a client (and the recorded fixtures)
// see; the code itself never reaches the wire. Go refuses the upgrade the same way.
const wsCloseForbidden = 4403

const (
	defaultWSMaxMessage = 65536
	wsWriteTimeout      = 10 * time.Second
	wsSubscribeTimeout  = 2 * time.Second
)

// Variables so tests can shorten them.
var (
	wsPingInterval = 30 * time.Second // server ping cadence; a client that does not pong in wsPongTimeout is dropped
	wsPongTimeout  = 10 * time.Second
)

// handshakeStatusForClose is the HTTP status Starlette answers for a pre-accept close code.
func handshakeStatusForClose(code int) int {
	if code == wsCloseForbidden {
		return http.StatusForbidden
	}
	return http.StatusOK
}

// registerWorkbookWS mounts GET /api/workbooks/{workbook_id}/ws. Like /api/events it has no
// Authenticate/Workspace middleware: browsers cannot set headers on a WebSocket, so the access token
// rides in ?token= and the tenant in ?workspace_id=. The handler never logs the raw query.
//
// Deviations from FastAPI (all hardening, see testdata/contract/README.md):
//   - invalid JSON, non-object JSON and binary frames are ignored (Python drops the socket with 1006);
//   - frames over WS_MAX_MESSAGE_BYTES close with 1009, idle/unresponsive clients are dropped by
//     server pings, sockets end after SSE_MAX_LIFETIME_SECONDS and on shutdown (1001);
//   - the same per-user / per-workspace / process-wide caps as SSE apply (429 / 503);
//   - the subscription is made before accept so a client that sees 101 never misses a publish;
//   - Origin must match the Host or CORS_ORIGINS (Python checks nothing; CORS_ORIGINS=* keeps that).
func registerWorkbookWS(r *httpx.Router, d Deps) {
	max := orDefault(d.SSEMaxStreams, defaultSSEMaxStreams)
	h := &wsRoute{d: d, sem: make(chan struct{}, max), maxMsg: int64(orDefault(d.WSMaxMessageBytes, defaultWSMaxMessage)),
		pingEvery: wsPingInterval, pongWithin: wsPongTimeout,
		perUser: orDefault(d.SSEMaxPerUser, defaultSSEPerUser), perWS: orDefault(d.SSEMaxPerWorkspace, defaultSSEPerWS),
		byUser: map[int32]int{}, byWS: map[string]int{}}
	h.origins, h.anyOrigin = originPatterns(d.Config.CORSOrigins)
	r.Handle("GET /api/workbooks/{workbook_id}/ws", http.HandlerFunc(h.serve))
}

// originPatterns turns CORS_ORIGINS into host patterns for websocket.AcceptOptions. "*" (or an empty
// list) disables the check, matching Python which has none.
func originPatterns(cors string) (patterns []string, any bool) {
	for _, o := range strings.Split(cors, ",") {
		o = strings.TrimSpace(o)
		switch {
		case o == "":
		case o == "*":
			return nil, true
		default:
			if u, err := url.Parse(o); err == nil && u.Host != "" {
				patterns = append(patterns, u.Host)
			}
		}
	}
	return patterns, len(patterns) == 0
}

type wsRoute struct {
	d      Deps
	sem    chan struct{}
	maxMsg int64

	pingEvery, pongWithin time.Duration

	origins   []string
	anyOrigin bool

	perUser, perWS int
	mu             sync.Mutex
	byUser         map[int32]int
	byWS           map[string]int
}

func (h *wsRoute) acquire(uid int32, ws string) (release func(), refusal string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.byUser[uid] >= h.perUser {
		return nil, "Too many websocket connections for this user"
	}
	if h.byWS[ws] >= h.perWS {
		return nil, "Too many websocket connections for this workspace"
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

// authorize mirrors _ws_authorize: a valid access token, membership in the CLAIMED workspace, the
// workspace's SSO policy, and the workbook existing in that tenant. Any failure (including an
// internal one) denies, like Python's blanket except. The user id is returned for the stream caps.
func (h *wsRoute) authorize(ctx context.Context, token, workspaceID, workbookID string) (uid int32, ok bool) {
	if token == "" || workspaceID == "" {
		return 0, false
	}
	p, err := identity.AuthenticateQueryTokenCtx(ctx, h.d.Signer, h.d.Identity, token, false)
	if err != nil {
		var qe *identity.QueryTokenError
		if !errors.As(err, &qe) {
			h.logErr("ws authorize: token lookup failed", err)
		}
		return 0, false
	}
	member, err := h.d.Identity.IsMember(ctx, workspaceID, p.User.ID)
	if err != nil || !member {
		if err != nil {
			h.logErr("ws authorize: membership lookup failed", err)
		}
		return 0, false
	}
	role, err := h.d.Identity.MemberRole(ctx, workspaceID, p.User.ID)
	if err != nil {
		h.logErr("ws authorize: role lookup failed", err)
		return 0, false
	}
	enforce, err := h.d.Identity.EnforceSSO(ctx, h.d.Pool, workspaceID)
	if err != nil {
		h.logErr("ws authorize: sso lookup failed", err)
		return 0, false
	}
	if enforce && role != "owner" && !slices.Contains(p.Claims.AMR, "sso") {
		return 0, false
	}
	var present bool
	err = tenancy.InWorkspace(ctx, h.d.Pool, workspaceID, func(tx pgx.Tx) error {
		var qerr error
		present, qerr = gen.New(tx).SourceIngestWorkbookExists(ctx, gen.SourceIngestWorkbookExistsParams{WorkbookID: workbookID, WorkspaceID: workspaceID})
		return qerr
	})
	if err != nil {
		h.logErr("ws authorize: workbook lookup failed", err)
		return 0, false
	}
	return p.User.ID, present
}

func (h *wsRoute) logErr(msg string, err error) {
	if h.d.Log != nil {
		h.d.Log.Warn(msg, "err", err.Error()) // never the request URL or token
	}
}

func (h *wsRoute) serve(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	workbookID := r.PathValue("workbook_id")
	uid, ok := h.authorize(ctx, q.Get("token"), q.Get("workspace_id"), workbookID)
	if !ok {
		w.WriteHeader(handshakeStatusForClose(wsCloseForbidden))
		return
	}
	ws := q.Get("workspace_id")

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
		httpx.Detail(w, http.StatusServiceUnavailable, "Too many websocket connections")
		return
	}

	// Only this workbook's channel is ever subscribed. Redis unavailable (or no REDIS_URL) means echo
	// mode: the socket still answers pings.
	var ps *redis.PubSub
	if h.d.Redis != nil {
		sctx, cancel := context.WithTimeout(ctx, wsSubscribeTimeout)
		cand := h.d.Redis.Subscribe(sctx, "workbook:"+workbookID)
		if _, err := cand.Receive(sctx); err == nil {
			ps = cand
		} else {
			_ = cand.Close()
		}
		cancel()
	}
	if ps != nil {
		defer ps.Close()
	}

	opts := &websocket.AcceptOptions{OriginPatterns: h.origins, InsecureSkipVerify: h.anyOrigin}
	conn, err := websocket.Accept(w, r, opts)
	if err != nil {
		return // Accept already answered
	}
	conn.SetReadLimit(h.maxMsg)

	// The socket ends with the request, on shutdown, or at the max lifetime (authorization is checked
	// once at connect; the client reconnects and re-authenticates).
	cctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopReq := context.AfterFunc(ctx, cancel)
	defer stopReq()
	goingAway := func(reason string) { _ = conn.Close(websocket.StatusGoingAway, reason); cancel() }
	if sd := h.d.ShutdownCtx; sd != nil {
		stop := context.AfterFunc(sd, func() { goingAway("server shutting down") })
		defer stop()
	}
	life := time.AfterFunc(orDuration(h.d.SSEMaxLifetime, defaultSSELifetime), func() { goingAway("session lifetime reached") })
	defer life.Stop()

	pingEvery, pongWithin := h.pingEvery, h.pongWithin
	var wg sync.WaitGroup
	write := func(typ websocket.MessageType, b []byte) error {
		wctx, wc := context.WithTimeout(cctx, wsWriteTimeout)
		defer wc()
		return conn.Write(wctx, typ, b)
	}
	if ps != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer cancel()
			ch := ps.Channel()
			for {
				select {
				case <-cctx.Done():
					return
				case m, open := <-ch:
					if !open {
						return
					}
					if write(websocket.MessageText, []byte(m.Payload)) != nil {
						return
					}
				}
			}
		}()
	}
	wg.Add(1)
	go func() { // liveness: an unresponsive client is dropped instead of holding a slot
		defer wg.Done()
		t := time.NewTicker(pingEvery)
		defer t.Stop()
		for {
			select {
			case <-cctx.Done():
				return
			case <-t.C:
				pctx, pc := context.WithTimeout(cctx, pongWithin)
				err := conn.Ping(pctx)
				pc()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()

	// Reader (this goroutine); the writers watch cctx and end with it.
	for {
		typ, data, err := conn.Read(cctx)
		if err != nil {
			break
		}
		if typ != websocket.MessageText {
			continue
		}
		var msg map[string]any
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		if msg["type"] == "ping" {
			if write(websocket.MessageText, []byte(`{"type":"pong"}`)) != nil {
				break
			}
		}
	}
	cancel()
	_ = conn.CloseNow()
	wg.Wait()
}
