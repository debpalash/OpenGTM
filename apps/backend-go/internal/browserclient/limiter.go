package browserclient

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/pausectx"
)

// The browser service runs at most BROWSER_MAX_CONCURRENCY jobs at once (apps/browser-service config.py: default 4,
// 1..64) and makes the rest wait up to BROWSER_QUEUE_WAIT_S before answering "busy". The same variable, with the
// same default and bounds, sizes this process-wide limiter, so a call waits here for a slot instead of queueing
// inside the service with its provider timeout already running.
//
// The limiter is per process: the api and every worker replica each hold their own, so the service sees up to
// (processes x BROWSER_MAX_CONCURRENCY) callers. Size BROWSER_MAX_CONCURRENCY on the service for the sum, or lower
// it on the callers; see docs/go-port/runbooks/browser-service.md.
const (
	defaultMaxConcurrency = 4
	minMaxConcurrency     = 1
	maxMaxConcurrency     = 64
)

func parseMaxConcurrency(v string) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return defaultMaxConcurrency
	}
	return min(max(n, minMaxConcurrency), maxMaxConcurrency)
}

var (
	limMu sync.Mutex
	lim   chan struct{}
)

func limiter() chan struct{} {
	limMu.Lock()
	defer limMu.Unlock()
	if lim == nil {
		lim = make(chan struct{}, parseMaxConcurrency(os.Getenv("BROWSER_MAX_CONCURRENCY")))
	}
	return lim
}

// SetMaxConcurrency replaces the process-wide limiter (tests). It returns the function that restores the
// previous one.
func SetMaxConcurrency(n int) (restore func()) {
	limMu.Lock()
	defer limMu.Unlock()
	prev := lim
	lim = make(chan struct{}, min(max(n, minMaxConcurrency), maxMaxConcurrency))
	return func() { limMu.Lock(); lim = prev; limMu.Unlock() }
}

// acquire waits for a browser slot. Time spent waiting is taken off the caller's provider timeout (pausectx), and
// the wait ends with ctx (the run context), returning ctx's own error, never a browser *Error.
func acquire(ctx context.Context) (release func(), err error) {
	l := limiter()
	select {
	case l <- struct{}{}:
		return func() { <-l }, nil
	default:
	}
	resume := pausectx.Pause(ctx)
	defer resume()
	select {
	case l <- struct{}{}:
		return func() { <-l }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
