package crm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type trackedBody struct {
	io.Reader
	closed *bool
	read   *bool
}

func (b trackedBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err == io.EOF {
		*b.read = true
	}
	return n, err
}
func (b trackedBody) Close() error { *b.closed = true; return nil }

func resp429(retryAfter string, closed, drained *bool) *http.Response {
	h := http.Header{}
	if retryAfter != "" {
		h.Set("Retry-After", retryAfter)
	}
	return &http.Response{StatusCode: 429, Header: h,
		Body: trackedBody{Reader: strings.NewReader("slow down"), closed: closed, read: drained}}
}

func okResp() *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}"))}
}

func newReq() (*http.Request, error) { return http.NewRequest("GET", "https://example.invalid/x", nil) }

func recordSleeps(into *[]float64) func(context.Context, time.Duration) error {
	return func(_ context.Context, d time.Duration) error {
		*into = append(*into, d.Seconds())
		return nil
	}
}

func TestRequestWithRetryHonorsRetryAfterAndCaps(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   []float64
	}{
		{"seconds", "3", []float64{3}},
		{"fraction", "0.5", []float64{0.5}},
		{"cap", "3600", []float64{15}},
		{"negative clamps to zero", "-5", []float64{0}},
		{"whitespace", " 4 ", []float64{4}},
		{"inf caps", "inf", []float64{15}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var closed, drained bool
			calls := 0
			c := &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return resp429(tc.header, &closed, &drained), nil
				}
				return okResp(), nil
			})}
			var sleeps []float64
			resp, err := RequestWithRetry(context.Background(), c, newReq, 3, recordSleeps(&sleeps))
			if err != nil || resp.StatusCode != 200 {
				t.Fatalf("got %v %v", resp, err)
			}
			if len(sleeps) != 1 || sleeps[0] != tc.want[0] {
				t.Fatalf("sleeps %v want %v", sleeps, tc.want)
			}
			if !closed || !drained {
				t.Fatalf("retried 429 body must be drained and closed (closed=%v drained=%v)", closed, drained)
			}
		})
	}
}

func TestRetryExponentialFallback(t *testing.T) {
	calls := 0
	c := &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) {
		calls++
		var a, b bool
		return resp429("soon", &a, &b), nil
	})}
	var sleeps []float64
	resp, err := RequestWithRetry(context.Background(), c, newReq, 3, recordSleeps(&sleeps))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 429 || calls != 4 {
		t.Fatalf("status %d calls %d (want 429 after 4 attempts)", resp.StatusCode, calls)
	}
	want := []float64{1, 2, 4}
	if len(sleeps) != 3 || sleeps[0] != want[0] || sleeps[1] != want[1] || sleeps[2] != want[2] {
		t.Fatalf("sleeps %v want %v", sleeps, want)
	}
}

func TestRetryOnly429(t *testing.T) {
	for _, status := range []int{200, 500, 503, 401} {
		calls := 0
		c := &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) {
			calls++
			r := okResp()
			r.StatusCode = status
			return r, nil
		})}
		var sleeps []float64
		resp, err := RequestWithRetry(context.Background(), c, newReq, 3, recordSleeps(&sleeps))
		if err != nil || resp.StatusCode != status || calls != 1 || len(sleeps) != 0 {
			t.Fatalf("status %d: calls %d sleeps %v err %v", status, calls, sleeps, err)
		}
	}
}

func TestRetryTransportErrorPropagates(t *testing.T) {
	boom := errors.New("dial failed")
	calls := 0
	c := &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, boom })}
	_, err := RequestWithRetry(context.Background(), c, newReq, 3, recordSleeps(new([]float64)))
	if err == nil || calls != 1 {
		t.Fatalf("err %v calls %d", err, calls)
	}
}

func TestRetryStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	c := &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) {
		calls++
		var a, b bool
		return resp429("30", &a, &b), nil
	})}
	// the real sleeper must return as soon as the context is cancelled
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := RequestWithRetry(ctx, c, newReq, 3, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if time.Since(start) > 2*time.Second || calls != 1 {
		t.Fatalf("took %v with %d calls", time.Since(start), calls)
	}
	// an already-cancelled context never issues a retry either
	calls = 0
	_, err = RequestWithRetry(ctx, c, newReq, 3, nil)
	if err == nil || calls > 1 {
		t.Fatalf("pre-cancelled: err %v calls %d", err, calls)
	}
}

func TestRetryBuildError(t *testing.T) {
	want := errors.New("bad request")
	_, err := RequestWithRetry(context.Background(), http.DefaultClient,
		func() (*http.Request, error) { return nil, want }, 3, nil)
	if !errors.Is(err, want) {
		t.Fatalf("err %v", err)
	}
}

func TestMaxRetrySleepIs15s(t *testing.T) {
	if MaxRetrySleep != 15*time.Second {
		t.Fatalf("MaxRetrySleep = %v", MaxRetrySleep)
	}
}
