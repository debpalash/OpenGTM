package authz

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// gatedLegacy is a fake workspace-context endpoint whose answers can be held
// back (gate), to model a slow or wedged FastAPI.
type gatedLegacy struct {
	calls    atomic.Int64
	inflight atomic.Int64
	peak     atomic.Int64
	status   atomic.Int64  // forced status when non-zero
	gate     chan struct{} // when non-nil, requests block until it is closed
}

func (g *gatedLegacy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.calls.Add(1)
	n := g.inflight.Add(1)
	defer g.inflight.Add(-1)
	for {
		p := g.peak.Load()
		if n <= p || g.peak.CompareAndSwap(p, n) {
			break
		}
	}
	if g.gate != nil {
		select {
		case <-g.gate:
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if s := g.status.Load(); s != 0 {
		w.WriteHeader(int(s))
		json.NewEncoder(w).Encode(map[string]string{"detail": "forced"})
		return
	}
	json.NewEncoder(w).Encode(Workspace{UserID: "7", Username: "ana", WorkspaceID: "ws-1", Slug: "s", Role: "owner"})
}

func newGated(t *testing.T, g *gatedLegacy, opts Options) *Client {
	t.Helper()
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, opts)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestConcurrentMissesShareOneUpstreamCall(t *testing.T) {
	g := &gatedLegacy{gate: make(chan struct{})}
	c := newGated(t, g, Options{})

	const callers = 50
	var wg sync.WaitGroup
	errs := make([]error, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = c.Resolve(context.Background(), "tok", "ws-1")
		}()
	}
	// Let every caller reach the in-flight lookup before the answer is released.
	deadline := time.Now().Add(5 * time.Second)
	for g.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(g.gate)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	if n := g.calls.Load(); n != 1 {
		t.Fatalf("%d concurrent callers made %d upstream calls, want 1", callers, n)
	}
	// A different workspace for the same token is a different key.
	if _, err := c.Resolve(context.Background(), "tok", "ws-2"); err != nil || g.calls.Load() != 2 {
		t.Fatalf("distinct key: err=%v calls=%d", err, g.calls.Load())
	}
}

func TestSharedLookupSurvivesTheFirstCallerLeaving(t *testing.T) {
	g := &gatedLegacy{gate: make(chan struct{})}
	c := newGated(t, g, Options{})

	first, cancelFirst := context.WithCancel(context.Background())
	firstErr := make(chan error, 1)
	go func() { _, err := c.Resolve(first, "tok", "ws-1"); firstErr <- err }()
	for g.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	second := make(chan error, 1)
	go func() { _, err := c.Resolve(context.Background(), "tok", "ws-1"); second <- err }()
	time.Sleep(30 * time.Millisecond)

	cancelFirst()
	if err := <-firstErr; !errors.Is(err, ErrUnavailable) {
		t.Fatalf("cancelled caller err = %v", err)
	}
	close(g.gate)
	if err := <-second; err != nil {
		t.Fatalf("the remaining caller must still get the answer: %v", err)
	}
	if n := g.calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1", n)
	}
}

func TestSlowUpstreamTimesOutQuickly(t *testing.T) {
	g := &gatedLegacy{gate: make(chan struct{})} // never released
	defer close(g.gate)
	c := newGated(t, g, Options{Timeout: 100 * time.Millisecond, BreakerThreshold: 100})

	start := time.Now()
	_, err := c.Resolve(context.Background(), "tok", "ws-1")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("a hung upstream held the caller for %v", d)
	}
}

func TestInflightLookupsAreBounded(t *testing.T) {
	g := &gatedLegacy{gate: make(chan struct{})}
	c := newGated(t, g, Options{MaxInflight: 3, Timeout: 5 * time.Second, BreakerThreshold: 100})

	const callers = 12
	results := make(chan error, callers)
	for i := range callers {
		go func() {
			_, err := c.Resolve(context.Background(), "tok-"+strconv.Itoa(i), "ws-1")
			results <- err
		}()
	}
	// The 9 callers over the bound are shed immediately, while the 3 admitted
	// lookups are still held by the gate.
	shed := 0
	for shed < callers-3 {
		select {
		case err := <-results:
			if !errors.Is(err, ErrOverloaded) || !errors.Is(err, ErrUnavailable) {
				t.Fatalf("shed caller err = %v, want ErrOverloaded", err)
			}
			shed++
		case <-time.After(3 * time.Second):
			t.Fatalf("only %d callers were shed; the rest queued behind the slow upstream", shed)
		}
	}
	close(g.gate)
	for range 3 {
		if err := <-results; err != nil {
			t.Fatalf("admitted lookup failed: %v", err)
		}
	}
	if p := g.peak.Load(); p > 3 {
		t.Fatalf("peak concurrent upstream calls = %d, bound is 3", p)
	}
}

func TestCircuitBreakerOpensProbesAndRecovers(t *testing.T) {
	g := &gatedLegacy{}
	g.status.Store(http.StatusInternalServerError)
	now := time.Unix(1_700_000_000, 0)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	c := newGated(t, g, Options{BreakerThreshold: 3, BreakerCooldown: 10 * time.Second, Now: clock})
	ctx := context.Background()

	for i := range 3 {
		if _, err := c.Resolve(ctx, "tok-"+strconv.Itoa(i), "ws"); !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrCircuitOpen) {
			t.Fatalf("failure %d: %v", i, err)
		}
	}
	if !c.brk.isOpen() {
		t.Fatal("circuit should be open after 3 consecutive failures")
	}
	before := g.calls.Load()
	for i := range 20 {
		_, err := c.Resolve(ctx, "other-"+strconv.Itoa(i), "ws")
		if !errors.Is(err, ErrCircuitOpen) {
			t.Fatalf("open circuit err = %v", err)
		}
	}
	if g.calls.Load() != before {
		t.Fatalf("open circuit still called upstream %d times", g.calls.Load()-before)
	}

	// After the cool-down one probe goes through; while it fails the circuit
	// re-opens for another full cool-down.
	advance(11 * time.Second)
	if _, err := c.Resolve(ctx, "probe-1", "ws"); errors.Is(err, ErrCircuitOpen) || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("probe err = %v, want the upstream failure", err)
	}
	if g.calls.Load() != before+1 {
		t.Fatalf("expected exactly one probe call, got %d", g.calls.Load()-before)
	}
	if _, err := c.Resolve(ctx, "probe-2", "ws"); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("circuit must re-open after a failed probe: %v", err)
	}

	// The upstream recovers; the next probe closes the circuit.
	g.status.Store(0)
	advance(11 * time.Second)
	if _, err := c.Resolve(ctx, "probe-3", "ws"); err != nil {
		t.Fatalf("recovery probe: %v", err)
	}
	if c.brk.isOpen() {
		t.Fatal("circuit should be closed after a successful probe")
	}
	if _, err := c.Resolve(ctx, "after", "ws"); err != nil {
		t.Fatalf("closed circuit: %v", err)
	}
}

func TestRefusalsDoNotOpenTheCircuit(t *testing.T) {
	g := &gatedLegacy{}
	g.status.Store(http.StatusUnauthorized)
	c := newGated(t, g, Options{BreakerThreshold: 2})
	for i := range 10 {
		_, err := c.Resolve(context.Background(), "tok-"+strconv.Itoa(i), "ws")
		var se *StatusError
		if !errors.As(err, &se) || se.Status != http.StatusUnauthorized {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if c.brk.isOpen() {
		t.Fatal("401s are healthy answers and must not open the circuit")
	}
}

func TestSheddingDoesNotCountAsAFailure(t *testing.T) {
	g := &gatedLegacy{gate: make(chan struct{})}
	c := newGated(t, g, Options{MaxInflight: 1, BreakerThreshold: 2, Timeout: 5 * time.Second})
	held := make(chan error, 1)
	go func() { _, err := c.Resolve(context.Background(), "held", "ws"); held <- err }()
	for g.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	for i := range 10 {
		if _, err := c.Resolve(context.Background(), "x-"+strconv.Itoa(i), "ws"); !errors.Is(err, ErrOverloaded) {
			t.Fatalf("err = %v", err)
		}
	}
	if c.brk.isOpen() {
		t.Fatal("load shedding must not trip the breaker")
	}
	close(g.gate)
	if err := <-held; err != nil {
		t.Fatal(err)
	}
}

// A wedged FastAPI (accepts connections, never answers) must not wedge the Go
// server: every request is answered within the timeout, nothing piles up, and
// once the breaker is open requests are refused instantly.
func TestWedgedUpstreamCannotWedgeTheServer(t *testing.T) {
	g := &gatedLegacy{gate: make(chan struct{})} // never released
	defer close(g.gate)
	c := newGated(t, g, Options{Timeout: 200 * time.Millisecond, MaxInflight: 8, BreakerThreshold: 4, BreakerCooldown: time.Minute})
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	srv := httptest.NewServer(h)
	defer srv.Close()

	const clients = 200
	codes := make([]int, clients)
	var wg sync.WaitGroup
	start := time.Now()
	for i := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("GET", srv.URL, nil)
			req.Header.Set("Authorization", "Bearer tok-"+strconv.Itoa(i))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				codes[i] = -1
				return
			}
			resp.Body.Close()
			codes[i] = resp.StatusCode
			if codes[i] == http.StatusServiceUnavailable && resp.Header.Get("Retry-After") == "" {
				codes[i] = -2
			}
		}()
	}
	wg.Wait()
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("200 requests against a wedged upstream took %v", d)
	}
	for i, code := range codes {
		if code != http.StatusBadGateway && code != http.StatusServiceUnavailable {
			t.Fatalf("request %d: status %d, want 502 or 503 with Retry-After", i, code)
		}
	}
	if p := g.peak.Load(); p > 8 {
		t.Fatalf("peak upstream concurrency %d exceeds the bound of 8", p)
	}
	if n := g.calls.Load(); n > 8+4 {
		t.Fatalf("%d upstream calls reached the wedged API; the breaker should have cut them off", n)
	}
}

func TestWriteErrorFailFastIs503WithRetryAfter(t *testing.T) {
	for _, err := range []error{ErrCircuitOpen, ErrOverloaded} {
		w := httptest.NewRecorder()
		WriteError(w, err)
		if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "1" {
			t.Fatalf("%v -> %d %v", err, w.Code, w.Header())
		}
	}
	w := httptest.NewRecorder()
	WriteError(w, ErrUnavailable)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("plain unavailable = %d, want 502", w.Code)
	}
}
