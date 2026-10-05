// Package authz resolves a caller's workspace authorization by forwarding
// their credentials to the legacy FastAPI service.
//
// Workspace membership, SSO enforcement and roles still live in FastAPI's
// control plane (SQLite workspace metadata), so the Go server must not
// re-implement them: a second implementation would drift and become the weaker
// check. Instead it asks GET /api/auth/workspace-context, which runs the same
// current_workspace dependency as every FastAPI data endpoint, and caches only
// successful answers briefly.
package authz

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// ContextPath is the FastAPI endpoint consulted for every cache miss.
const ContextPath = "/api/auth/workspace-context"

// Workspace is the authorized identity for one request.
type Workspace struct {
	UserID      string `json:"user_id"`
	Username    string `json:"username"`
	WorkspaceID string `json:"workspace_id"`
	Slug        string `json:"slug"`
	Role        string `json:"role"`
}

// StatusError carries an authorization refusal (401/403/404) from FastAPI so
// the Go server answers exactly as the legacy API would.
type StatusError struct {
	Status int
	Detail string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("authz: %d %s", e.Status, e.Detail)
}

// ErrUnavailable means the legacy API could not answer; callers should fail
// closed with 502/503 rather than treat it as an anonymous request.
var ErrUnavailable = errors.New("authz: workspace authorization service unavailable")

// ErrForbidden is returned by RequireRole.
var ErrForbidden = &StatusError{Status: http.StatusForbidden, Detail: "Insufficient workspace role"}

// Options tune a Client.
//
// A slow or sick legacy API must never wedge this server, so every lookup is
// bounded three ways: a per-call timeout, a cap on lookups in flight, and a
// circuit breaker that refuses new lookups outright while the API keeps
// failing. Lookups for the same (token, workspace) are also collapsed into one.
type Options struct {
	TTL        time.Duration // positive-result lifetime, default 30s
	MaxEntries int           // cache bound, default 10000
	HTTPClient *http.Client  // default: a pooled transport sized to MaxInflight
	Now        func() time.Time

	// Timeout bounds one upstream lookup, default 3s. FastAPI answers in a few
	// milliseconds when healthy, so anything slower is already a failure.
	Timeout time.Duration
	// MaxInflight bounds concurrent upstream lookups across all keys, default
	// 32 (16 per uvicorn worker, below a worker's database pool). Beyond it
	// requests fail immediately with ErrOverloaded instead of queueing.
	MaxInflight int
	// BreakerThreshold consecutive upstream failures open the circuit, default
	// 5. BreakerCooldown is how long it stays open before one probe, default 5s.
	BreakerThreshold int
	BreakerCooldown  time.Duration
}

// Client resolves and caches workspace authorization.
type Client struct {
	endpoint string
	http     *http.Client
	ttl      time.Duration
	max      int
	now      func() time.Time

	timeout time.Duration
	slots   chan struct{} // bounds concurrent upstream lookups
	brk     *breaker

	mu     sync.Mutex
	cache  map[[32]byte]entry
	flight map[[32]byte]*call
}

type entry struct {
	ws      Workspace
	expires time.Time
}

// New returns a client for the legacy API at baseURL.
func New(baseURL string, opts Options) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("authz: invalid legacy API URL %q", baseURL)
	}
	if opts.TTL <= 0 {
		opts.TTL = 30 * time.Second
	}
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = 10000
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 3 * time.Second
	}
	if opts.MaxInflight <= 0 {
		opts.MaxInflight = 32
	}
	if opts.BreakerThreshold <= 0 {
		opts.BreakerThreshold = 5
	}
	if opts.BreakerCooldown <= 0 {
		opts.BreakerCooldown = 5 * time.Second
	}
	if opts.HTTPClient == nil {
		// The default transport keeps only 2 idle connections per host, which
		// would reconnect on nearly every lookup under load.
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.MaxIdleConnsPerHost = opts.MaxInflight
		tr.ResponseHeaderTimeout = opts.Timeout
		opts.HTTPClient = &http.Client{Transport: tr}
	}
	return &Client{
		endpoint: u.String() + ContextPath,
		http:     opts.HTTPClient,
		ttl:      opts.TTL,
		max:      opts.MaxEntries,
		now:      opts.Now,
		timeout:  opts.Timeout,
		slots:    make(chan struct{}, opts.MaxInflight),
		brk:      &breaker{threshold: opts.BreakerThreshold, cooldown: opts.BreakerCooldown, now: opts.Now},
		cache:    map[[32]byte]entry{},
		flight:   map[[32]byte]*call{},
	}, nil
}

// Credentials extracts the bearer token (Authorization header, or ?token=
// for EventSource/WebSocket, which cannot set headers) and the requested
// workspace (X-Workspace-Id, or ?workspace_id= for the same reason). An
// empty workspace means the user's active workspace.
func Credentials(r *http.Request) (token, workspaceID string) {
	if h := r.Header.Get("Authorization"); h != "" {
		if scheme, rest, ok := strings.Cut(h, " "); ok && strings.EqualFold(scheme, "bearer") {
			token = strings.TrimSpace(rest)
		}
	}
	if token == "" {
		token = r.URL.Query().Get("token")
	}
	workspaceID = r.Header.Get("X-Workspace-Id")
	if workspaceID == "" {
		workspaceID = r.URL.Query().Get("workspace_id")
	}
	return token, workspaceID
}

// Authorize resolves the request's workspace authorization.
func (c *Client) Authorize(r *http.Request) (Workspace, error) {
	token, workspaceID := Credentials(r)
	return c.Resolve(r.Context(), token, workspaceID)
}

// Resolve returns the authorization for token in workspaceID ("" = active).
func (c *Client) Resolve(ctx context.Context, token, workspaceID string) (Workspace, error) {
	if token == "" {
		return Workspace{}, &StatusError{Status: http.StatusUnauthorized, Detail: "Not authenticated"}
	}
	key := sha256.Sum256([]byte(token + "\x00" + workspaceID))
	now := c.now()
	c.mu.Lock()
	if e, ok := c.cache[key]; ok {
		if now.Before(e.expires) {
			c.mu.Unlock()
			return e.ws, nil
		}
		delete(c.cache, key)
	}
	// Collapse concurrent misses for the same (token, workspace) into one
	// upstream lookup. The lookup runs detached from any single caller's
	// context, so one client hanging up cannot fail the others; each caller
	// still stops waiting when its own context ends.
	cl, inflight := c.flight[key]
	if !inflight {
		cl = &call{done: make(chan struct{})}
		c.flight[key] = cl
	}
	c.mu.Unlock()
	if !inflight {
		go c.lookup(key, cl, token, workspaceID, now)
	}

	select {
	case <-cl.done:
		return cl.ws, cl.err
	case <-ctx.Done():
		return Workspace{}, fmt.Errorf("%w: %v", ErrUnavailable, ctx.Err())
	}
}

// lookup performs the shared upstream call for cl and publishes the result.
func (c *Client) lookup(key [32]byte, cl *call, token, workspaceID string, now time.Time) {
	cl.ws, cl.err = c.guardedFetch(token, workspaceID)
	if cl.err == nil {
		c.store(key, cl.ws, now)
	} // failures are never cached: a revoked membership must bite now
	c.mu.Lock()
	delete(c.flight, key)
	c.mu.Unlock()
	close(cl.done)
}

// guardedFetch applies the circuit breaker and the in-flight bound around one
// upstream lookup.
func (c *Client) guardedFetch(token, workspaceID string) (Workspace, error) {
	if !c.brk.allow() {
		return Workspace{}, ErrCircuitOpen
	}
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	default:
		c.brk.release()
		return Workspace{}, ErrOverloaded
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	ws, err := c.fetch(ctx, token, workspaceID)
	if errors.Is(err, ErrUnavailable) {
		c.brk.failure()
	} else {
		c.brk.success() // success, or a healthy 401/403/404 about the caller
	}
	return ws, err
}

func (c *Client) fetch(ctx context.Context, token, workspaceID string) (Workspace, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint, nil)
	if err != nil {
		return Workspace{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if workspaceID != "" {
		req.Header.Set("X-Workspace-Id", workspaceID)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Workspace{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return Workspace{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		var detail struct {
			Detail any `json:"detail"`
		}
		_ = json.Unmarshal(body, &detail)
		msg, _ := detail.Detail.(string)
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return Workspace{}, &StatusError{Status: resp.StatusCode, Detail: msg}
	default:
		return Workspace{}, fmt.Errorf("%w: status %d", ErrUnavailable, resp.StatusCode)
	}
	var ws Workspace
	if err := json.Unmarshal(body, &ws); err != nil || ws.WorkspaceID == "" || ws.UserID == "" {
		return Workspace{}, fmt.Errorf("%w: malformed workspace context", ErrUnavailable)
	}
	return ws, nil
}

func (c *Client) store(key [32]byte, ws Workspace, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.cache) >= c.max {
		for k, e := range c.cache {
			if !now.Before(e.expires) {
				delete(c.cache, k)
			}
		}
		// Still full of live entries: drop arbitrary ones. Correctness never
		// depends on the cache, only latency does.
		for k := range c.cache {
			if len(c.cache) < c.max {
				break
			}
			delete(c.cache, k)
		}
	}
	c.cache[key] = entry{ws: ws, expires: now.Add(c.ttl)}
}

type ctxKey struct{}

// WithWorkspace attaches an authorization to ctx.
func WithWorkspace(ctx context.Context, ws Workspace) context.Context {
	return context.WithValue(ctx, ctxKey{}, ws)
}

// FromContext returns the authorization attached by WithWorkspace.
func FromContext(ctx context.Context) (Workspace, bool) {
	ws, ok := ctx.Value(ctxKey{}).(Workspace)
	return ws, ok
}

// RequireRole succeeds when the caller holds one of roles. The workspace
// owner satisfies any role, matching require_workspace_role in FastAPI.
func RequireRole(ctx context.Context, roles ...string) error {
	ws, ok := FromContext(ctx)
	if !ok {
		return &StatusError{Status: http.StatusUnauthorized, Detail: "Not authenticated"}
	}
	if ws.Role == "owner" || (ws.Role != "" && slices.Contains(roles, ws.Role)) {
		return nil
	}
	return ErrForbidden
}

// Middleware authorizes every request, attaching the Workspace to its
// context, and answers refusals with FastAPI-style {"detail": ...} bodies.
func (c *Client) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := c.Authorize(r)
		if err != nil {
			WriteError(w, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithWorkspace(r.Context(), ws)))
	})
}

// WriteError renders an authorization error.
func WriteError(w http.ResponseWriter, err error) {
	status, detail := http.StatusBadGateway, "Workspace authorization unavailable"
	var se *StatusError
	if errors.Is(err, ErrCircuitOpen) || errors.Is(err, ErrOverloaded) {
		// Shed on purpose: the legacy API is unhealthy or saturated. Tell the
		// client to back off rather than retry immediately.
		status = http.StatusServiceUnavailable
		w.Header().Set("Retry-After", "1")
	} else if errors.As(err, &se) {
		status, detail = se.Status, se.Detail
		if status == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", "Bearer")
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"detail": detail})
}
