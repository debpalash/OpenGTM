package browserclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/pausectx"
)

func TestMaxConcurrencyFromEnv(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want int
	}{{"", 4}, {"7", 7}, {"0", 1}, {"999", 64}, {"junk", 4}} {
		if got := parseMaxConcurrency(tc.env); got != tc.want {
			t.Errorf("BROWSER_MAX_CONCURRENCY=%q: got %d want %d", tc.env, got, tc.want)
		}
	}
}

// slowServer counts the calls in flight and the highest count seen.
func slowServer(t *testing.T, hold time.Duration, cur, peak *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := cur.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(hold)
		cur.Add(-1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCallsNeverExceedTheLimit(t *testing.T) {
	restore := SetMaxConcurrency(3)
	defer restore()
	var cur, peak atomic.Int32
	srv := slowServer(t, 40*time.Millisecond, &cur, &peak)
	c := New(srv.URL, testToken, nil)
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Render(context.Background(), RenderRequest{URL: "https://e.com"}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if p := peak.Load(); p > 3 || p < 2 {
		t.Fatalf("peak in flight %d, want 2..3", p)
	}
}

func TestSeveralClientsShareOneLimiter(t *testing.T) {
	restore := SetMaxConcurrency(2)
	defer restore()
	var cur, peak atomic.Int32
	srv := slowServer(t, 40*time.Millisecond, &cur, &peak)
	var wg sync.WaitGroup
	for range 8 {
		c := New(srv.URL, testToken, nil) // each call site builds its own client
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.Render(context.Background(), RenderRequest{URL: "https://e.com"})
		}()
	}
	wg.Wait()
	if p := peak.Load(); p > 2 {
		t.Fatalf("peak in flight %d across clients, limit 2", p)
	}
}

func TestWaitingForASlotDoesNotSpendTheProviderTimeout(t *testing.T) {
	restore := SetMaxConcurrency(1)
	defer restore()
	var cur, peak atomic.Int32
	srv := slowServer(t, 80*time.Millisecond, &cur, &peak)
	c := New(srv.URL, testToken, nil)
	go func() { _, _ = c.Render(context.Background(), RenderRequest{URL: "https://e.com"}) }()
	time.Sleep(20 * time.Millisecond) // the first call holds the only slot
	// A provider timeout shorter than the wait: the second call waits ~60 ms for the slot and must still finish.
	ctx, cancel := pausectx.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.Render(ctx, RenderRequest{URL: "https://e.com"}); err != nil {
		t.Fatalf("queue wait was counted against the timeout: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("context expired: %v", ctx.Err())
	}
}

func TestWaitIsBoundedByTheContext(t *testing.T) {
	restore := SetMaxConcurrency(1)
	defer restore()
	var cur, peak atomic.Int32
	srv := slowServer(t, 400*time.Millisecond, &cur, &peak)
	c := New(srv.URL, testToken, nil)
	go func() { _, _ = c.Render(context.Background(), RenderRequest{URL: "https://e.com"}) }()
	time.Sleep(30 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Render(ctx, RenderRequest{URL: "https://e.com"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want the run context's error, got %v", err)
	}
	if time.Since(start) > 300*time.Millisecond {
		t.Fatalf("waited %v", time.Since(start))
	}
	var be *Error
	if errors.As(err, &be) {
		t.Fatalf("a slot wait must not look like a browser timeout: %v", err)
	}
}
