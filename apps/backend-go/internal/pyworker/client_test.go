package pyworker

import (
	"context"
	"fmt"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testToken = "tok-SECRET-0123456789abcdef"

func okHandler(t *testing.T, got *atomic.Value) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if got != nil {
			var m map[string]any
			_ = json.NewDecoder(r.Body).Decode(&m)
			m["_auth"] = r.Header.Get("Authorization")
			m["_path"] = r.URL.Path
			m["_method"] = r.Method
			got.Store(m)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","output":{"success":true,"fields":{"a":1}},"error":null,"cost_usd":0.25}`))
	}
}

func TestCallSendsBearerAndCallID(t *testing.T) {
	var got atomic.Value
	srv := httptest.NewServer(okHandler(t, &got))
	defer srv.Close()
	c := New(srv.URL, testToken, nil)
	res, err := c.Call(context.Background(), ProviderCall{CallID: "c1", Provider: "p", WorkspaceID: "w1",
		Input: json.RawMessage(`{"x":1}`), Timeout: 7 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "ok" || res.CostUSD != 0.25 || !strings.Contains(string(res.Output), `"fields"`) {
		t.Fatalf("result %+v", res)
	}
	m := got.Load().(map[string]any)
	if m["_auth"] != "Bearer "+testToken || m["_path"] != "/internal/provider-call" || m["_method"] != "POST" {
		t.Fatalf("request %v", m)
	}
	if m["call_id"] != "c1" || m["provider"] != "p" || m["workspace_id"] != "w1" || m["timeout_s"] != float64(7) {
		t.Fatalf("body %v", m)
	}
	if in, _ := m["input"].(map[string]any); in["x"] != float64(1) {
		t.Fatalf("input %v", m["input"])
	}
}

func TestTimeoutCappedAt120(t *testing.T) {
	var got atomic.Value
	srv := httptest.NewServer(okHandler(t, &got))
	defer srv.Close()
	_, err := New(srv.URL, testToken, nil).Call(context.Background(), ProviderCall{CallID: "c", Provider: "p", WorkspaceID: "w", Timeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if got.Load().(map[string]any)["timeout_s"] != float64(120) {
		t.Fatalf("timeout_s not capped: %v", got.Load())
	}
}

func TestCallTimeoutAndCancel(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	c := New(srv.URL, testToken, nil)
	c.slack = 50 * time.Millisecond
	start := time.Now()
	res, err := c.Call(context.Background(), ProviderCall{CallID: "c", Provider: "p", WorkspaceID: "w", Timeout: 100 * time.Millisecond})
	if err != nil || res.Status != "timeout" {
		t.Fatalf("want timeout result, got %+v err=%v", res, err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("did not time out promptly")
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start = time.Now()
	_, err = c.Call(ctx, ProviderCall{CallID: "c2", Provider: "p", WorkspaceID: "w", Timeout: 30 * time.Second})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("cancel did not abort the request")
	}
}

func TestCallRetriesOnceWithSameCallID(t *testing.T) {
	var n int32
	var ids []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		ids = append(ids, m["call_id"].(string))
		if atomic.AddInt32(&n, 1) == 1 {
			http.Error(w, "boom", 502)
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok","output":{},"error":null,"cost_usd":0}`))
	}))
	defer srv.Close()
	res, err := New(srv.URL, testToken, nil).Call(context.Background(), ProviderCall{CallID: "same", Provider: "p", WorkspaceID: "w"})
	if err != nil || res.Status != "ok" || n != 2 || ids[0] != "same" || ids[1] != "same" {
		t.Fatalf("res=%+v err=%v n=%d ids=%v", res, err, n, ids)
	}
	// persistent 5xx: exactly two attempts, then error result
	n = 0
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&n, 1); http.Error(w, "x", 500) }))
	defer srv2.Close()
	res, err = New(srv2.URL, testToken, nil).Call(context.Background(), ProviderCall{CallID: "c", Provider: "p", WorkspaceID: "w"})
	if err != nil || res.Status != "error" || res.Error == "" || n != 2 {
		t.Fatalf("res=%+v err=%v n=%d", res, err, n)
	}
	// 4xx is not retried
	n = 0
	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&n, 1); http.Error(w, "x", 404) }))
	defer srv3.Close()
	res, _ = New(srv3.URL, testToken, nil).Call(context.Background(), ProviderCall{CallID: "c", Provider: "p", WorkspaceID: "w"})
	if res.Status != "error" || n != 1 {
		t.Fatalf("res=%+v n=%d", res, n)
	}
}

func TestCallBodyCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok","output":{"x":"` + strings.Repeat("a", 5<<20) + `"},"cost_usd":0}`))
	}))
	defer srv.Close()
	res, err := New(srv.URL, testToken, nil).Call(context.Background(), ProviderCall{CallID: "c", Provider: "p", WorkspaceID: "w"})
	if err != nil || res.Status != "error" || !strings.Contains(res.Error, "too large") {
		t.Fatalf("res.Status=%s err=%v", res.Status, err)
	}
}

func TestCallErrorsNeverContainToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "echo "+r.Header.Get("Authorization"), 500)
	}))
	url := srv.URL
	c := New(url, testToken, nil)
	res, err := c.Call(context.Background(), ProviderCall{CallID: "c", Provider: "p", WorkspaceID: "w"})
	if err != nil || strings.Contains(res.Error, testToken) || strings.Contains(string(res.Output), testToken) {
		t.Fatalf("leak: %+v %v", res, err)
	}
	srv.Close() // connection refused path; url error text must not carry the token
	res, err = c.Call(context.Background(), ProviderCall{CallID: "c", Provider: "p", WorkspaceID: "w"})
	if err != nil || res.Status != "error" || strings.Contains(res.Error, testToken) {
		t.Fatalf("leak/err: %+v %v", res, err)
	}
	if strings.Contains(c.String(), testToken) {
		t.Fatal("String leaks token")
	}
	var buf strings.Builder
	buf.WriteString(strings.TrimSpace(strings.Join([]string{c.String()}, "")))
	if strings.Contains(buf.String(), testToken) {
		t.Fatal("leak")
	}
}

func TestUnavailableWhenNoURL(t *testing.T) {
	_, err := New("", testToken, nil).Call(context.Background(), ProviderCall{CallID: "c", Provider: "p", WorkspaceID: "w"})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
	var nilc *Client
	if _, err := nilc.Call(context.Background(), ProviderCall{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil client: %v", err)
	}
}

func TestEmptyTokenIsUnavailableWithoutRequest(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&n, 1) }))
	defer srv.Close()
	_, err := New(srv.URL, "", nil).Call(context.Background(), ProviderCall{CallID: "c", Provider: "p", WorkspaceID: "w"})
	if !errors.Is(err, ErrUnavailable) || n != 0 {
		t.Fatalf("err=%v requests=%d", err, n)
	}
}

func statusServer(code int, n *int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(n, 1)
		http.Error(w, "x", code)
	}))
}

func TestStatusSentinelsAndNoRetryOn503(t *testing.T) {
	cases := []struct {
		code int
		want error
	}{{404, ErrUnknownProvider}, {401, ErrUnauthorized}, {503, ErrUnauthorized}}
	for _, tc := range cases {
		var n int32
		srv := statusServer(tc.code, &n)
		res, err := New(srv.URL, testToken, nil).Call(context.Background(), ProviderCall{CallID: "c", Provider: "p", WorkspaceID: "w"})
		srv.Close()
		if err != nil || res.Status != "error" || n != 1 {
			t.Fatalf("%d: res=%+v err=%v n=%d", tc.code, res, err, n)
		}
		if !errors.Is(res.Err, tc.want) || res.HTTPStatus != tc.code {
			t.Fatalf("%d: Err=%v HTTPStatus=%d", tc.code, res.Err, res.HTTPStatus)
		}
	}
	var n int32
	srv := statusServer(500, &n)
	defer srv.Close()
	res, _ := New(srv.URL, testToken, nil).Call(context.Background(), ProviderCall{CallID: "c", Provider: "p", WorkspaceID: "w"})
	if res.HTTPStatus != 500 || n != 2 {
		t.Fatalf("500: %+v n=%d", res, n)
	}
}

func TestRetryBackoffHonoursContext(t *testing.T) {
	var n int32
	srv := statusServer(500, &n)
	defer srv.Close()
	c := New(srv.URL, testToken, nil)
	c.backoff = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := c.Call(ctx, ProviderCall{CallID: "c", Provider: "p", WorkspaceID: "w"})
	if !errors.Is(err, context.Canceled) || n != 1 || time.Since(start) > 2*time.Second {
		t.Fatalf("err=%v n=%d", err, n)
	}
}

func TestUnknownStatusCoercedToError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"weird","output":null,"cost_usd":1.5}`))
	}))
	defer srv.Close()
	res, _ := New(srv.URL, testToken, nil).Call(context.Background(), ProviderCall{CallID: "c", Provider: "p", WorkspaceID: "w"})
	if res.Status != "error" || res.CostUSD != 0 {
		t.Fatalf("%+v", res)
	}
}

func TestCallerClientTimeoutIgnoredAndTokenRedacted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		_, _ = w.Write([]byte(`{"status":"ok","output":{},"cost_usd":0}`))
	}))
	defer srv.Close()
	c := New(srv.URL, testToken, &http.Client{Timeout: 20 * time.Millisecond})
	res, err := c.Call(context.Background(), ProviderCall{CallID: "c", Provider: "p", WorkspaceID: "w", Timeout: 5 * time.Second})
	if err != nil || res.Status != "ok" {
		t.Fatalf("hc.Timeout cut the call: %+v %v", res, err)
	}
	if strings.Contains(fmt.Sprintf("%v %+v %#v", c, *c, *c), testToken) {
		t.Fatal("token printed")
	}
}
