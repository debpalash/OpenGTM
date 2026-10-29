// Package crm holds the CRM vendor clients (HubSpot here; Salesforce lives in
// salesforce.go) and the shared 429 retry helper. Ports of
// apps/api/services/crm/{__init__,hubspot}.py.
package crm

import (
	"context"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// MaxRetrySleep caps any single wait between 429 retries
// (MAX_RETRY_SLEEP_SECONDS).
const MaxRetrySleep = 15 * time.Second

// DefaultMaxRetries is the number of extra attempts after the first 429.
const DefaultMaxRetries = 3

// SleepFunc waits for d or returns the context error.
type SleepFunc func(ctx context.Context, d time.Duration) error

// ctxSleep is the production SleepFunc: it returns as soon as ctx is done.
func ctxSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// RequestWithRetry is request_with_retry: it issues the request built by req
// (a fresh *http.Request per attempt) and retries only on HTTP 429, up to
// maxRetries extra attempts. The wait is the Retry-After header read as float
// seconds, else 2^attempt, clamped to [0, MaxRetrySleep]. The final response
// (possibly still a 429) is returned for the caller to read and close; the body
// of a retried 429 is drained and closed here. Transport errors, a request
// build error and a context cancel (also during the wait) return immediately.
// A nil sleep waits on a timer that honours ctx.
func RequestWithRetry(ctx context.Context, c *http.Client, req func() (*http.Request, error),
	maxRetries int, sleep func(context.Context, time.Duration) error) (*http.Response, error) {
	if sleep == nil {
		sleep = ctxSleep
	}
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		r, err := req()
		if err != nil {
			return nil, err
		}
		resp, err := c.Do(r)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusTooManyRequests || attempt >= maxRetries {
			return resp, nil
		}
		delay := retryDelay(resp.Header.Get("Retry-After"), attempt)
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if err := sleep(ctx, delay); err != nil {
			return nil, err
		}
	}
}

// retryDelay is min(max(float(Retry-After), 0), 15s); anything float() would
// reject falls back to 2**attempt seconds (still capped).
func retryDelay(header string, attempt int) time.Duration {
	secs, ok := pyFloat(header)
	if !ok {
		secs = math.Pow(2, float64(attempt))
	}
	if math.IsNaN(secs) {
		secs = 0
	}
	if secs < 0 {
		secs = 0
	}
	if secs >= MaxRetrySleep.Seconds() {
		return MaxRetrySleep
	}
	return time.Duration(secs * float64(time.Second))
}

// pyFloat is Python's float(str): surrounding whitespace is ignored, hex
// floats are not accepted, "inf"/"nan" are.
func pyFloat(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, "xXpP") {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
			return f, true // +-Inf like Python's float("1e999")
		}
		return 0, false
	}
	return f, true
}
