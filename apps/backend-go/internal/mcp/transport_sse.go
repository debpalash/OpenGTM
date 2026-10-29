package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

const (
	// maxMessageBody caps one POST /message body (same bound as Server.Handle).
	maxMessageBody = 4 << 20
	// maxConcurrentRequests bounds in-flight requests; more get 503.
	maxConcurrentRequests = 64
	// authFailureLimit failed authentications per authFailureWindow per client IP.
	authFailureLimit  = 30
	authFailureWindow = time.Minute
	maxFailureKeys    = 100_000
	// sseEndpointEvent is the byte-exact (Python json.dumps) endpoint event.
	sseEndpointEvent = "data: {\"type\": \"endpoint\", \"url\": \"/message\"}\n\n"
)

// resolveToken authenticates a bearer token; tests replace it.
var resolveToken = Resolve

// failLimiter is a fixed-window per-IP failed-auth counter. Unlike
// internal/ratelimit (which counts every request and keys on headers) it can
// be peeked before authentication and keys on the socket peer only.
type failLimiter struct {
	mu   sync.Mutex
	win  map[string]*failWindow
	nowF func() time.Time
}

type failWindow struct {
	n       int
	expires time.Time
}

func newFailLimiter() *failLimiter {
	return &failLimiter{win: map[string]*failWindow{}, nowF: time.Now}
}

// blocked reports whether ip has used up its failure budget (no counting).
func (l *failLimiter) blocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.win[ip]
	return w != nil && l.nowF().Before(w.expires) && w.n >= authFailureLimit
}

// record counts one failure for ip.
func (l *failLimiter) record(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.nowF()
	w := l.win[ip]
	if w == nil || !now.Before(w.expires) {
		if w == nil && len(l.win) >= maxFailureKeys {
			for k, v := range l.win {
				if !now.Before(v.expires) {
					delete(l.win, k)
				}
			}
			if len(l.win) >= maxFailureKeys {
				return // full of live windows: drop the count, never grow
			}
		}
		w = &failWindow{expires: now.Add(authFailureWindow)}
		l.win[ip] = w
	}
	w.n++
}

// peerIP is the socket peer address only: forwarded headers are never trusted
// on this loopback transport.
func peerIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// sseHandler is the loopback HTTP transport: GET /sse (the unauthenticated
// endpoint announcement, then the stream ends, as Python) and POST /message
// (authenticated JSON-RPC). There are no sessions: the Python endpoint is the
// fixed path /message and every request carries its own bearer token, so there
// is no per-session state to cap, expire or guess.
type sseHandler struct {
	s    *Server
	d    Deps
	sem  chan struct{}
	fail *failLimiter
}

// SSEHandler returns the HTTP handler for the MCP SSE transport.
func SSEHandler(s *Server, d Deps) http.Handler {
	return &sseHandler{s: s, d: d, sem: make(chan struct{}, maxConcurrentRequests), fail: newFailLimiter()}
}

// hostnameOf strips the port and IPv6 brackets from a Host-style value.
func hostnameOf(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return strings.Trim(hostport, "[]")
}

// loopbackRequest rejects DNS-rebinding and cross-site browser requests: the
// Host must name loopback and any Origin must be a loopback origin. Skipped
// when the operator explicitly allowed a non-loopback bind.
func loopbackRequest(r *http.Request) bool {
	if allowNonLoopback() {
		return true
	}
	if !loopbackAddr(hostnameOf(r.Host)) {
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil || !loopbackAddr(u.Hostname()) {
			return false
		}
	}
	return true
}

func notFound(w http.ResponseWriter) {
	httpx.WriteJSON(w, http.StatusNotFound, map[string]string{"detail": "Not Found"})
}
func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	httpx.WriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"detail": "Method Not Allowed"})
}

func (h *sseHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case h.sem <- struct{}{}:
		defer func() { <-h.sem }()
	default:
		w.Header().Set("Retry-After", "1")
		httpx.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "server busy"})
		return
	}
	defer func() {
		if rec := recover(); rec != nil && rec != http.ErrAbortHandler {
			slog.Error("mcp sse handler panic", "panic", fmt.Sprint(rec))
			httpx.WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		}
	}()
	if !loopbackRequest(r) {
		httpx.WriteJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "detail": "host not allowed"})
		return
	}
	switch r.URL.Path {
	case "/sse":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, "GET")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, sseEndpointEvent)
	case "/message":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return
		}
		h.message(w, r)
	default:
		notFound(w)
	}
}

func (h *sseHandler) message(w http.ResponseWriter, r *http.Request) {
	ip := peerIP(r)
	if h.fail.blocked(ip) {
		// Over the failed-auth budget: no token lookup at all.
		w.Header().Set("Retry-After", "60")
		httpx.WriteJSON(w, http.StatusTooManyRequests, map[string]string{"error": "Rate limit exceeded: 30 per 1 minute"})
		return
	}
	// Authenticate before the body is touched.
	tok, ok := BearerToken(r.Header.Get("Authorization"))
	if !ok {
		tok = ""
	}
	c, err := resolveToken(r.Context(), h.d, tok)
	if err != nil {
		var ae *AuthError
		if errors.As(err, &ae) {
			h.fail.record(ip)
			w.Header().Set("WWW-Authenticate", "Bearer")
			httpx.WriteJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized", "detail": ae.Msg})
			return
		}
		if IsUnavailable(err) {
			httpx.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "service unavailable"})
			return
		}
		slog.Error("mcp sse auth failed", "err", err.Error())
		httpx.WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMessageBody))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			httpx.WriteJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
			return
		}
		httpx.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "could not read request body"})
		return
	}
	resp, respond := h.s.Handle(r.Context(), c, body)
	w.Header().Set("Content-Type", "application/json")
	if !respond {
		_, _ = io.WriteString(w, "null")
		return
	}
	_, _ = w.Write(resp)
}

// loopbackAddr reports whether host names only the loopback interface.
func loopbackAddr(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func allowNonLoopback() bool {
	v, err := strconv.ParseBool(strings.TrimSpace(os.Getenv("OPENGTM_MCP_SSE_ALLOW_NONLOOPBACK")))
	return err == nil && v
}

// newHTTPServer applies the transport's server limits. toolTimeout is the
// tool deadline; the write deadline allows it plus 10 s.
func newHTTPServer(h http.Handler, toolTimeout time.Duration) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      toolTimeout + 10*time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
}

// ListenSSE serves h on host:port until ctx is done, then shuts down
// gracefully. A non-loopback host is refused unless
// OPENGTM_MCP_SSE_ALLOW_NONLOOPBACK is true (D6).
func ListenSSE(ctx context.Context, h http.Handler, host string, port int) error {
	if !loopbackAddr(host) && !allowNonLoopback() {
		return fmt.Errorf("mcp sse: refusing non-loopback host %q (set OPENGTM_MCP_SSE_ALLOW_NONLOOPBACK=true to override)", host)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	kind := "loopback"
	if !loopbackAddr(host) {
		kind = "NON-LOOPBACK override"
	}
	fmt.Fprintf(os.Stderr, "OpenGTM MCP SSE server on http://%s:%d (%s, bearer-auth)\n", host, port, kind)
	return serveSSE(ctx, h, ln)
}

func serveSSE(ctx context.Context, h http.Handler, ln net.Listener) error {
	timeout := 120 * time.Second
	if sh, ok := h.(*sseHandler); ok {
		timeout = sh.s.timeout()
	}
	srv := newHTTPServer(h, timeout)
	base, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()
	srv.BaseContext = func(net.Listener) context.Context { return base }
	var once sync.Once
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
		case <-done:
			return
		}
		once.Do(func() {
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if srv.Shutdown(sctx) != nil {
				cancelBase() // grace expired: cancel in-flight tool calls
				_ = srv.Close()
			}
		})
	}()
	err := srv.Serve(ln)
	close(done)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
