package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/personintel"
)

// Variables so tests can shorten them.
var (
	// personWSFirstFrameTimeout: Python waits forever for the request frame; Go drops a silent client.
	personWSFirstFrameTimeout = 15 * time.Second
	// personWSDeadline bounds the whole enrichment (Python has no bound).
	personWSDeadline = 120 * time.Second
	// personWSSaveTimeout bounds saving the profile, including the wait for a pool connection under load
	// (was 10 s).
	personWSSaveTimeout = 30 * time.Second
)

const (
	personWSGenericError = "Enrichment failed"
	personWSTimeoutError = "Enrichment timed out"
	personWSNoURL        = "No LinkedIn URL provided"
	personWSBusy         = "Enrichment already running"
	personWSInvalidURL   = "Invalid LinkedIn URL. Expected format: https://www.linkedin.com/in/username"
)

type pwsEnrichFn = func(ctx context.Context, d personintel.Deps, url string, deep bool, notify func(string, map[string]any)) (personintel.Profile, error)

// personWS serves GET /ws/person/enrich (routers/person_intel.py websocket_person_enrich). Like the
// workbook socket it has no Authenticate middleware: the admin access token rides in ?token= and the
// handler never logs the query. person_intel is global, so there is no workspace.
type personWS struct {
	d      Deps
	egress *http.Client
	// enrich is a seam for tests; production runs personintel.Enrich.
	enrich pwsEnrichFn

	sem     chan struct{}
	maxMsg  int64
	perUser int

	origins   []string
	anyOrigin bool

	mu      sync.Mutex
	conns   map[int32]int
	working map[int32]bool
}

func newPersonWS(d Deps) *personWS {
	cl := d.Egress
	if cl == nil {
		cl = egress.NewClientForEnv(d.Config.AppEnv, d.EgressAllowHosts)
	}
	w := &personWS{d: d, egress: cl,
		enrich: func(ctx context.Context, pd personintel.Deps, u string, deep bool, notify func(string, map[string]any)) (personintel.Profile, error) {
			return personintel.Enrich(ctx, pd, u, deep, notify)
		},
		sem:     personStreamPool(d),
		maxMsg:  int64(orDefault(d.WSMaxMessageBytes, defaultWSMaxMessage)),
		perUser: orDefault(d.SSEMaxPerUser, defaultSSEPerUser),
		conns:   map[int32]int{}, working: map[int32]bool{}}
	w.origins, w.anyOrigin = originPatterns(d.Config.CORSOrigins)
	return w
}

// personStreamPool is the long-lived-stream pool person intel draws on: the process-wide semaphore shared
// with /api/events and chat when cmd/api provides it, else a private one sized by SSE_MAX_STREAMS.
func personStreamPool(d Deps) chan struct{} {
	if d.SSEStreamSem != nil {
		return d.SSEStreamSem
	}
	return make(chan struct{}, orDefault(d.SSEMaxStreams, defaultSSEMaxStreams))
}

// registerPersonIntelWS mounts GET /ws/person/enrich.
//
// Deviations from FastAPI (hardening, listed for testdata/contract/README.md):
//   - failures send the generic "Enrichment failed" instead of str(exc), except the username parse error;
//   - one enrichment per admin at a time ("Enrichment already running"), per-user/process connection caps
//     (429/503), 15 s to send the request frame, a 120 s enrichment deadline ("Enrichment timed out");
//   - frames over WS_MAX_MESSAGE_BYTES close with 1009, unresponsive clients are dropped by pings, and
//     shutdown closes with 1001; Origin must match the Host or CORS_ORIGINS;
//   - a client disconnect cancels the pipeline and nothing is saved (Python keeps running and saves);
//   - a non-text first frame gets the generic error (Python raises KeyError into the same handler).
func registerPersonIntelWS(r *httpx.Router, d Deps) { newPersonWS(d).register(r) }

func (h *personWS) register(r *httpx.Router) {
	r.Handle("GET /ws/person/enrich", http.HandlerFunc(h.serve))
}

func (h *personWS) log() *slog.Logger {
	if h.d.Log != nil {
		return h.d.Log
	}
	return slog.Default()
}

func (h *personWS) running(uid int32) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.working[uid]
}

func (h *personWS) acquire(uid int32) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conns[uid] >= h.perUser {
		return false
	}
	h.conns[uid]++
	return true
}

func (h *personWS) releaseConn(uid int32) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conns[uid]--; h.conns[uid] <= 0 {
		delete(h.conns, uid)
	}
}

func (h *personWS) startRun(uid int32) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.working[uid] {
		return false
	}
	h.working[uid] = true
	return true
}

func (h *personWS) endRun(uid int32) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.working, uid)
}

// wsTruthy is Python truthiness for a value decoded with encoding/json (numbers are float64).
func wsTruthy(v any) bool {
	if f, ok := v.(float64); ok {
		return f != 0
	}
	return pyTruthy(v)
}

func errFrame(msg string) []byte {
	b, _ := personintel.EventJSON("error", map[string]any{"message": msg})
	return b
}

func (h *personWS) serve(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, err := identity.AuthenticateQueryTokenCtx(ctx, h.d.Signer, h.d.Identity, r.URL.Query().Get("token"), true)
	if err != nil {
		w.WriteHeader(handshakeStatusForClose(wsCloseForbidden))
		return
	}
	uid := p.User.ID
	if !h.acquire(uid) {
		httpx.Detail(w, http.StatusTooManyRequests, "Too many websocket connections for this user")
		return
	}
	defer h.releaseConn(uid)
	select {
	case h.sem <- struct{}{}:
		defer func() { <-h.sem }()
	default:
		httpx.Detail(w, http.StatusServiceUnavailable, "Too many websocket connections")
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: h.origins, InsecureSkipVerify: h.anyOrigin})
	if err != nil {
		return
	}
	conn.SetReadLimit(h.maxMsg)

	cctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopReq := context.AfterFunc(ctx, cancel)
	defer stopReq()
	if sd := h.d.ShutdownCtx; sd != nil {
		stop := context.AfterFunc(sd, func() { _ = conn.Close(websocket.StatusGoingAway, "server shutting down"); cancel() })
		defer stop()
	}
	var wg sync.WaitGroup
	defer func() { cancel(); _ = conn.CloseNow(); wg.Wait() }()

	write := func(b []byte) error {
		wctx, wc := context.WithTimeout(cctx, wsWriteTimeout)
		defer wc()
		return conn.Write(wctx, websocket.MessageText, b)
	}
	finish := func(b []byte) { // optional last frame, then a clean close
		if b != nil {
			if write(b) != nil {
				return
			}
		}
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}

	// 1) the request frame, within personWSFirstFrameTimeout.
	idle := time.AfterFunc(personWSFirstFrameTimeout, func() {
		_ = conn.Close(websocket.StatusPolicyViolation, "request frame timeout")
		cancel()
	})
	typ, data, err := conn.Read(cctx)
	if !idle.Stop() || err != nil {
		return
	}
	var req any
	if typ != websocket.MessageText || json.Unmarshal(data, &req) != nil {
		finish(errFrame(personWSGenericError))
		return
	}
	obj, isObj := req.(map[string]any)
	if !isObj {
		finish(errFrame(personWSGenericError))
		return
	}
	rawURL, deepV := obj["linkedin_url"], obj["deep"]
	if !wsTruthy(rawURL) {
		finish(errFrame(personWSNoURL))
		return
	}
	linkedinURL, isStr := rawURL.(string)
	if !isStr { // Python: `"..." not in <non-str>` raises TypeError
		finish(errFrame(personWSGenericError))
		return
	}
	if !personintel.ValidLinkedInURL(linkedinURL) {
		finish(errFrame(personWSInvalidURL))
		return
	}
	if !h.startRun(uid) {
		finish(errFrame(personWSBusy))
		return
	}
	defer h.endRun(uid)

	// 2) from here nobody else reads the socket: this goroutine services control frames and notices
	// a disconnect (which cancels the pipeline); another one pings.
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer cancel()
		for {
			if _, _, err := conn.Read(cctx); err != nil {
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		t := time.NewTicker(wsPingInterval)
		defer t.Stop()
		for {
			select {
			case <-cctx.Done():
				return
			case <-t.C:
				pctx, pc := context.WithTimeout(cctx, wsPongTimeout)
				err := conn.Ping(pctx)
				pc()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()

	rctx, rcancel := context.WithTimeout(cctx, personWSDeadline)
	defer rcancel()
	_, parseErr := personintel.ParseUsername(linkedinURL)
	notify := func(step string, d map[string]any) {
		if step == "error" && parseErr == nil { // never str(exc) of a network/DB error
			d = map[string]any{"message": personWSGenericError}
		}
		if b, err := personintel.EventJSON(step, d); err == nil {
			_ = write(b) // like Python's ws_callback, a failed send is ignored
		}
	}
	deps := personintel.Deps{HTTP: h.egress, Browser: h.d.browser()}
	profile, err := h.run(rctx, deps, linkedinURL, wsTruthy(deepV), notify)
	switch {
	case cctx.Err() != nil:
		return // disconnect or shutdown: nothing to send, nothing saved
	case rctx.Err() != nil:
		finish(errFrame(personWSTimeoutError))
		return
	case err != nil:
		h.log().Error("person intel ws: enrichment failed", "err_type", errTypeName(err))
		finish(errFrame(personWSGenericError))
		return
	}

	// Python saves (also a failed profile) and then sends the saved frame.
	sctx, sc := context.WithTimeout(cctx, personWSSaveTimeout)
	defer sc()
	var id int64
	tx, err := h.d.Pool.Begin(sctx)
	if err == nil {
		defer tx.Rollback(sctx) //nolint:errcheck
		if id, err = personintel.Save(sctx, tx, profile); err == nil {
			err = tx.Commit(sctx)
		}
	}
	if err != nil {
		h.log().Error("person intel ws: save failed", "err_type", errTypeName(err))
		finish(errFrame(personWSGenericError))
		return
	}
	b, err := personintel.EventJSON("saved", map[string]any{"id": id, "profile": profile})
	if err != nil {
		h.log().Error("person intel ws: encode failed", "err_type", fmt.Sprintf("%T", err))
		b = errFrame(personWSGenericError)
	}
	finish(b)
}

// run calls the pipeline, turning a panic into an error (the stack never reaches the client).
func (h *personWS) run(ctx context.Context, d personintel.Deps, u string, deep bool, notify func(string, map[string]any)) (p personintel.Profile, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("enrichment panic: %T", r)
		}
	}()
	return h.enrich(ctx, d, u, deep, notify)
}
