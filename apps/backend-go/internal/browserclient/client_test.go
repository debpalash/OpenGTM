package browserclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testToken = "browser-SECRET-token-0123456789abcdef"

type seen struct {
	method, path, auth string
	body               map[string]any
}

func server(t *testing.T, status int, reply string, got *atomic.Value) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got != nil {
			s := seen{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization")}
			_ = json.NewDecoder(r.Body).Decode(&s.body)
			got.Store(s)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestUnconfigured(t *testing.T) {
	for _, c := range []*Client{nil, New("", testToken, nil), New("http://x", "", nil)} {
		if _, err := c.Render(context.Background(), RenderRequest{URL: "https://e.com"}); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("want ErrUnavailable, got %v", err)
		}
	}
}

func TestRenderRequestAndResult(t *testing.T) {
	var got atomic.Value
	srv := server(t, 200, `{"url":"https://e.com","final_url":"https://e.com/f","status":200,"engine":"browser",
		"title":"T","description":"D","html":"<p>x</p>","text":null,"html_truncated":false,"text_truncated":false,
		"challenge":true,"blocked_requests":2,"elapsed_ms":15}`, &got)
	c := New(srv.URL+"/", testToken, nil)
	res, err := c.Render(context.Background(), RenderRequest{URL: "https://e.com", IncludeHTML: true, WaitMS: 2000,
		Cookies: []Cookie{{Name: "li_at", Value: "v", Domain: ".linkedin.com", Secure: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalURL != "https://e.com/f" || res.HTML != "<p>x</p>" || res.Text != "" || !res.Challenge || res.BlockedRequests != 2 {
		t.Fatalf("result %+v", res)
	}
	s := got.Load().(seen)
	if s.method != "POST" || s.path != "/v1/render" || s.auth != "Bearer "+testToken {
		t.Fatalf("request %+v", s)
	}
	b := s.body
	if b["url"] != "https://e.com" || b["timeout_s"] != float64(30) || b["include_html"] != true || b["include_text"] != false ||
		b["wait_ms"] != float64(2000) {
		t.Fatalf("body %v", b)
	}
	if _, ok := b["engine"]; ok {
		t.Fatalf("zero engine must be omitted (service default): %v", b)
	}
	if ck := b["cookies"].([]any)[0].(map[string]any); ck["http_only"] != false || ck["secure"] != true {
		t.Fatalf("cookie %v", ck)
	}
}

func TestTimeoutDefaultsAndCap(t *testing.T) {
	var got atomic.Value
	srv := server(t, 200, `{}`, &got)
	c := New(srv.URL, testToken, nil)
	cases := []struct {
		call func() error
		want float64
	}{
		{func() error {
			_, err := c.Render(context.Background(), RenderRequest{URL: "https://e.com/u", Timeout: time.Hour})
			return err
		}, 600},
		{func() error {
			_, err := c.GoogleMaps(context.Background(), GoogleMapsRequest{Query: "q", City: "c"})
			return err
		}, 120},
		{func() error {
			_, err := c.JustDialPage(context.Background(), JustDialPageRequest{URL: "https://e.com/u"})
			return err
		}, 45},
		{func() error {
			_, err := c.Crunchbase(context.Background(), CrunchbaseRequest{Queries: []string{"q"}, Cities: []string{"c"},
				SearchOptions: SearchOptions{Timeout: 7 * time.Second}})
			return err
		}, 7},
	}
	for i, tc := range cases {
		if err := tc.call(); err != nil {
			t.Fatal(err)
		}
		if b := got.Load().(seen).body; b["timeout_s"] != tc.want {
			t.Fatalf("case %d: timeout_s %v", i, b["timeout_s"])
		}
	}
}

func TestSearchPathsAndOptions(t *testing.T) {
	var got atomic.Value
	srv := server(t, 200, `{"source":"x","runs":[{"variant":"companies","query":"q","city":"c","term":"","search":"s",
		"hits":[{"title":"t","url":"u","body":"b","date":"","source":""}],"error":null},
		{"variant":"companies","query":"q2","city":"c","term":"","search":"s2","hits":[],"error":"rate_limited"}],
		"partial":true,"elapsed_ms":3}`, &got)
	c := New(srv.URL, testToken, nil)
	ctx := context.Background()
	zero := 0.0
	calls := map[string]func() (SearchResult, error){
		"/v1/scrape/linkedin": func() (SearchResult, error) {
			return c.LinkedIn(ctx, LinkedInRequest{Queries: []string{"q"}, Cities: []string{"c"}, SearchOptions: SearchOptions{DelayS: &zero}})
		},
		"/v1/scrape/job_boards": func() (SearchResult, error) {
			return c.JobBoards(ctx, JobBoardsRequest{Board: "naukri", Queries: []string{"q"}, Cities: []string{"c"}})
		},
		"/v1/scrape/crunchbase": func() (SearchResult, error) {
			return c.Crunchbase(ctx, CrunchbaseRequest{Queries: []string{"q"}, Cities: []string{"c"}})
		},
		"/v1/scrape/review_directories": func() (SearchResult, error) {
			return c.ReviewDirectories(ctx, ReviewDirectoriesRequest{Site: "g2", Queries: []string{"q"}})
		},
		"/v1/scrape/web_directories": func() (SearchResult, error) {
			return c.WebDirectories(ctx, WebDirectoriesRequest{Queries: []string{"q"}, Cities: []string{"c"}})
		},
		"/v1/scrape/google_search": func() (SearchResult, error) {
			return c.GoogleSearch(ctx, GoogleSearchRequest{Mode: "news", Queries: []string{"q"}})
		},
	}
	for path, call := range calls {
		res, err := call()
		if err != nil {
			t.Fatal(path, err)
		}
		if got.Load().(seen).path != path {
			t.Fatalf("path %s != %s", got.Load().(seen).path, path)
		}
		if !res.Partial || len(res.Runs) != 2 || res.Runs[0].Hits[0].URL != "u" || res.Runs[0].Error != "" || res.Runs[1].Error != "rate_limited" {
			t.Fatalf("%s result %+v", path, res)
		}
	}
	// DelayS pointer to 0 is sent (0 = no pause), nil is omitted (legacy default)
	_, _ = calls["/v1/scrape/linkedin"]()
	if b := got.Load().(seen).body; b["delay_s"] != float64(0) {
		t.Fatalf("delay_s %v", b)
	}
	_, _ = calls["/v1/scrape/crunchbase"]()
	if _, ok := got.Load().(seen).body["delay_s"]; ok {
		t.Fatal("nil DelayS must be omitted")
	}
}

func TestScreenshotDecodesImage(t *testing.T) {
	srv := server(t, 200, `{"url":"u","final_url":"u","status":200,"content_type":"image/png","image_base64":"iVBORw==","bytes":4,"elapsed_ms":1}`, nil)
	res, err := New(srv.URL, testToken, nil).Screenshot(context.Background(), ScreenshotRequest{URL: "https://e.com/u", Format: "png"})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Image) != "\x89PNG" {
		t.Fatalf("image %q", res.Image)
	}
}

func TestHealth(t *testing.T) {
	var got atomic.Value
	srv := server(t, 200, `{"status":"ok","version":"0.1.0","browser":"ready","active":1,"capacity":4,"engines":["browser"]}`, &got)
	h, err := New(srv.URL, testToken, nil).Health(context.Background())
	if err != nil || h.Browser != "ready" || h.Capacity != 4 {
		t.Fatalf("%+v %v", h, err)
	}
	if s := got.Load().(seen); s.method != "GET" || s.path != "/v1/health" {
		t.Fatalf("%+v", s)
	}
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   error
		code   string
	}{
		{401, `{"error":{"code":"unauthorized","message":"m"}}`, ErrUnauthorized, "unauthorized"},
		{403, `{"error":{"code":"egress_blocked","message":"m"}}`, ErrBlocked, "egress_blocked"},
		{429, `{"error":{"code":"busy","message":"m"}}`, ErrBusy, "busy"},
		{504, `{"error":{"code":"timeout","message":"m"}}`, ErrTimeout, "timeout"},
		{502, `{"error":{"code":"navigation_failed","message":"net::ERR_X upstream-secret"}}`, ErrNavigation, "navigation_failed"},
		{502, `{"error":{"code":"upstream_error","message":"m"}}`, ErrUpstream, "upstream_error"},
		{503, `{"error":{"code":"engine_unavailable","message":"m"}}`, ErrEngineUnavailable, "engine_unavailable"},
		{503, `{"error":{"code":"browser_unavailable","message":"m"}}`, ErrBrowserUnavailable, "browser_unavailable"},
		{413, `{"error":{"code":"too_large","message":"m"}}`, ErrTooLarge, "too_large"},
		{404, `{"error":{"code":"not_found","message":"m"}}`, ErrBadRequest, "not_found"},
		{500, `{"error":{"code":"internal","message":"m"}}`, ErrServer, "internal"},
		// unknown or missing codes fall back to the status
		{503, `<html>nginx upstream-secret</html>`, ErrBrowserUnavailable, "browser_unavailable"},
		{429, `{"error":{"code":"weird; upstream-secret"}}`, ErrBusy, "busy"},
		{502, ``, ErrServer, "internal"},
		{302, ``, ErrBadRequest, "bad_request"},
		{400, `{"error":{"code":"unreachable"}}`, ErrBadRequest, "bad_request"},
	}
	for _, tc := range cases {
		srv := server(t, tc.status, tc.body, nil)
		_, err := New(srv.URL, testToken, nil).Render(context.Background(), RenderRequest{URL: "https://e.com/u"})
		var e *Error
		if !errors.As(err, &e) || !errors.Is(err, tc.want) || e.Code != tc.code || e.HTTPStatus != tc.status {
			t.Fatalf("%d %s: got %v", tc.status, tc.body, err)
		}
		if strings.Contains(err.Error(), "upstream-secret") || strings.Contains(err.Error(), testToken) {
			t.Fatalf("error leaks text: %v", err)
		}
	}
}

func TestBadRequestFieldsAreFiltered(t *testing.T) {
	srv := server(t, 400, `{"error":{"code":"bad_request","message":"m","fields":["url","timeout_s","<script>","x y"]}}`, nil)
	_, err := New(srv.URL, testToken, nil).Render(context.Background(), RenderRequest{URL: "https://e.com/u"})
	var e *Error
	if !errors.As(err, &e) || strings.Join(e.Fields, ",") != "url,timeout_s" || !errors.Is(err, ErrBadRequest) {
		t.Fatalf("%v", err)
	}
}

func TestBodyCapAndInvalidJSON(t *testing.T) {
	srv := server(t, 200, `{"html":"`+strings.Repeat("a", 4096)+`"}`, nil)
	c := New(srv.URL, testToken, nil)
	c.MaxBodyBytes = 1024
	if _, err := c.Render(context.Background(), RenderRequest{URL: "https://e.com/u"}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want too large, got %v", err)
	}
	srv = server(t, 200, `not json`, nil)
	if _, err := New(srv.URL, testToken, nil).Render(context.Background(), RenderRequest{URL: "https://e.com/u"}); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("want invalid response, got %v", err)
	}
}

func TestTransportErrors(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	_, err := New(url, testToken, nil).Render(context.Background(), RenderRequest{URL: "https://e.com/u"})
	if !errors.Is(err, ErrUnreachable) || !Retryable(err) || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("got %v", err)
	}

	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	defer close(release)
	c := New(slow.URL, testToken, nil)
	c.slack = 10 * time.Millisecond
	_, err = c.Render(context.Background(), RenderRequest{URL: "https://e.com/u", Timeout: 50 * time.Millisecond})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("want timeout, got %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	_, err = New(slow.URL, testToken, nil).Render(ctx, RenderRequest{URL: "https://e.com/u"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want canceled, got %v", err)
	}
}

func TestRetryable(t *testing.T) {
	for _, e := range []error{&Error{Code: "busy"}, &Error{Code: "timeout"}, &Error{Code: "browser_unavailable"}} {
		if !Retryable(e) {
			t.Fatal(e)
		}
	}
	for _, e := range []error{&Error{Code: "egress_blocked"}, &Error{Code: "bad_request"}, &Error{Code: "unauthorized"}, ErrUnavailable} {
		if Retryable(e) {
			t.Fatal(e)
		}
	}
}

func TestTokenNeverFormatted(t *testing.T) {
	c := New("http://svc", testToken, nil)
	for _, s := range []string{fmt.Sprint(c), fmt.Sprintf("%v %+v %#v %s", c, c, *c, *c), c.String()} {
		if strings.Contains(s, testToken) {
			t.Fatalf("token leaked: %s", s)
		}
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/v1/render", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	if _, err := New(srv.URL, testToken, nil).Render(context.Background(), RenderRequest{URL: "https://e.com/u"}); err == nil {
		t.Fatal("redirect must fail")
	}
	if hits.Load() != 0 {
		t.Fatal("redirect followed (token would be forwarded)")
	}
}

func TestProxyURLIsRedactedButSentOnTheWire(t *testing.T) {
	p := ProxyURL("http://user:hunter2@1.2.3.4:8080")
	req := SearchRequest{Query: "q", UpstreamProxy: p}
	for _, s := range []string{fmt.Sprint(p), fmt.Sprintf("%v %+v %#v %s %q", p, req, req, p, p)} {
		if strings.Contains(s, "hunter2") {
			t.Fatalf("proxy credentials formatted: %s", s)
		}
	}
	var got map[string]any
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"query":"q","method":"text","hits":[],"error":"rate_limited","elapsed_ms":3}`))
	}))
	defer srv.Close()
	out, err := New(srv.URL, testToken, nil).Search(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/search" || got["upstream_proxy"] != string(p) || got["backends"] != nil || got["timeout_s"] != 30.0 {
		t.Fatalf("wire body %v path %s", got, gotPath)
	}
	if out.Error != "rate_limited" {
		t.Fatalf("out %+v", out)
	}
}

// review L8: no environment proxy, and URLs are pre-checked with the Go egress guard.

func TestClientTransportIgnoresEnvironmentProxy(t *testing.T) {
	for _, hc := range []*http.Client{nil, {Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}} {
		c := New("http://svc", testToken, hc)
		tr, ok := c.hc.Transport.(*http.Transport)
		if !ok || tr.Proxy != nil {
			t.Fatalf("transport %T proxy set", c.hc.Transport)
		}
	}
	if tr := http.DefaultTransport.(*http.Transport); tr.Proxy == nil {
		t.Fatal("the shared default transport must not be modified")
	}
}

func TestClientRefusesBadURLsBeforeCallingTheService(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(500)
	}))
	defer srv.Close()
	c := New(srv.URL, testToken, nil)
	ctx := context.Background()
	bad := []string{
		"file:///etc/passwd", "ftp://e.com/", "http://127.0.0.1/", "http://10.0.0.1/", "http://[::1]/",
		"http://0x7f000001/", "http://169.254.169.254/latest", "http://localhost/", "https://user:pw@e.com/",
		"https://e.com:22/", "http://e.com:6379/", "", "https://",
	}
	for _, u := range bad {
		errs := []error{}
		_, err := c.Render(ctx, RenderRequest{URL: u})
		errs = append(errs, err)
		_, err = c.Screenshot(ctx, ScreenshotRequest{URL: u})
		errs = append(errs, err)
		_, err = c.JustDialPage(ctx, JustDialPageRequest{URL: u})
		errs = append(errs, err)
		_, err = c.Extract(ctx, ExtractRequest{URL: u})
		errs = append(errs, err)
		for i, err := range errs {
			if !errors.Is(err, ErrBlocked) {
				t.Errorf("%q call %d: err %v, want ErrBlocked", u, i, err)
			}
		}
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("service called %d times for refused URLs", n)
	}
	// HTML given: the URL is only metadata and is not fetched, so it is not vetted
	if _, err := c.Extract(ctx, ExtractRequest{HTML: "<p>x</p>", URL: "http://10.0.0.1/"}); errors.Is(err, ErrBlocked) {
		t.Fatal("extract with html must not vet the metadata URL")
	}
	c.AllowedPorts = []int{80, 443, 8443}
	if _, err := c.Render(ctx, RenderRequest{URL: "https://e.com:8443/"}); errors.Is(err, ErrBlocked) {
		t.Fatal("configured port refused")
	}
}
