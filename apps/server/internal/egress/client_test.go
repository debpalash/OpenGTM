package egress

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(t *testing.T, opts Options) *Client {
	t.Helper()
	opts.AllowPrivateForTesting = true
	if opts.DefaultRPS == 0 {
		opts.DefaultRPS = 1000
	}
	c, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestFetchEvidenceAndUserAgent(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()
	c := testClient(t, Options{Version: "1.2.3"})
	resp, err := c.Do(context.Background(), Request{URL: srv.URL + "/x?y=1", Header: http.Header{"User-Agent": {"evil-bot"}}})
	if err != nil {
		t.Fatal(err)
	}
	if gotUA != "OpenGTM/1.2.3 (+https://github.com/debpalash/OpenGTM)" {
		t.Fatalf("user agent = %q", gotUA)
	}
	sum := sha256.Sum256([]byte(`{"ok":true}`))
	ev := resp.Evidence
	if ev.Status != 200 || ev.Bytes != 11 || ev.SHA256 != hex.EncodeToString(sum[:]) || ev.ContentType != "application/json" || ev.URL != srv.URL+"/x?y=1" || ev.FetchedAt.IsZero() {
		t.Fatalf("evidence = %+v", ev)
	}
}

func TestGuardBlocksPrivateTargetsWhenNotTesting(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request must never reach the server")
	}))
	defer srv.Close()
	c, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{srv.URL, strings.Replace(srv.URL, "127.0.0.1", "2130706433", 1), "http://localhost:1/", "http://169.254.169.254/latest/meta-data/", "http://[::1]:1/"} {
		_, err := c.Do(context.Background(), Request{URL: u})
		var be *BlockedError
		if !errors.As(err, &be) {
			t.Errorf("%s: expected BlockedError, got %v", u, err)
		}
	}
}

func TestDNSPinningDialsValidatedAddress(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.WriteString(w, r.Host)
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))

	// A name that resolves to loopback is refused before any connection.
	blocking, _ := New(Options{Resolver: fakeResolver{"rebind.test": {"127.0.0.1"}}})
	if _, err := blocking.Do(context.Background(), Request{URL: "http://rebind.test:" + port + "/"}); err == nil {
		t.Fatal("rebinding name must be blocked")
	}
	// The dialer connects to the address the resolver returned (pinning):
	// "pinned.test" only exists in the fake resolver.
	c := testClient(t, Options{Resolver: fakeResolver{"pinned.test": {"127.0.0.1"}}})
	resp, err := c.Do(context.Background(), Request{URL: "http://pinned.test:" + port + "/"})
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Body) != "pinned.test:"+port || hits.Load() != 1 {
		t.Fatalf("body=%q hits=%d", resp.Body, hits.Load())
	}
}

// scripted is a RoundTripper that answers by URL, for redirect-chain tests
// with the guard fully enabled.
type scripted func(*http.Request) *http.Response

func (s scripted) RoundTrip(r *http.Request) (*http.Response, error) { return s(r), nil }

func redirectTo(loc string) *http.Response {
	return &http.Response{StatusCode: 302, Header: http.Header{"Location": {loc}}, Body: io.NopCloser(strings.NewReader(""))}
}

func TestRedirectHopsAreValidated(t *testing.T) {
	var seen []string
	c, _ := New(Options{DefaultRPS: 1000, Transport: scripted(func(r *http.Request) *http.Response {
		seen = append(seen, r.URL.String())
		switch r.URL.Host {
		case "public.example":
			return redirectTo("http://169.254.169.254/latest/meta-data/")
		case "decimal.example":
			return redirectTo("http://2130706433/admin")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}
	})})
	for _, start := range []string{"https://public.example/", "https://decimal.example/"} {
		seen = nil
		_, err := c.Do(context.Background(), Request{URL: start})
		var be *BlockedError
		if !errors.As(err, &be) {
			t.Fatalf("%s: redirect to private target must be blocked, got %v", start, err)
		}
		if len(seen) != 1 {
			t.Fatalf("private hop was requested: %v", seen)
		}
	}
}

func TestRedirectLimitCapabilityAndCredentialStripping(t *testing.T) {
	var authOnOther string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authOnOther = r.Header.Get("Authorization") + "|" + r.Header.Get("X-Api-Key")
		io.WriteString(w, "other")
	}))
	defer other.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("/loop/", func(w http.ResponseWriter, r *http.Request) {
		var n int
		fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/loop/"), "%d", &n)
		http.Redirect(w, r, fmt.Sprintf("/loop/%d", n+1), http.StatusFound)
	})
	mux.HandleFunc("/hop/", func(w http.ResponseWriter, r *http.Request) {
		var n int
		fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/hop/"), "%d", &n)
		if n >= 5 {
			io.WriteString(w, "done")
			return
		}
		http.Redirect(w, r, fmt.Sprintf("/hop/%d", n+1), http.StatusFound)
	})
	mux.HandleFunc("/away", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/landing", http.StatusTemporaryRedirect)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := testClient(t, Options{})
	ctx := context.Background()

	if resp, err := c.Do(ctx, Request{URL: srv.URL + "/hop/0"}); err != nil || string(resp.Body) != "done" {
		t.Fatalf("5 redirects must be followed: %v", err)
	}
	if _, err := c.Do(ctx, Request{URL: srv.URL + "/loop/0"}); !errors.Is(err, ErrTooManyRedirects) {
		t.Fatalf("expected too many redirects, got %v", err)
	}
	srvURL, _ := url.Parse(srv.URL)
	onlySrv := func(u *url.URL) error {
		if u.Host != srvURL.Host {
			return errors.New("capability denied: " + u.Host)
		}
		return nil
	}
	if _, err := c.Do(ctx, Request{URL: srv.URL + "/away", Allow: onlySrv}); err == nil || !strings.Contains(err.Error(), "capability denied") {
		t.Fatalf("redirect outside capability must be denied, got %v", err)
	}
	resp, err := c.Do(ctx, Request{URL: srv.URL + "/away", Header: http.Header{"Authorization": {"Bearer s3cret"}, "X-Api-Key": {"k"}}, SensitiveHeaders: []string{"X-Api-Key"}})
	if err != nil || string(resp.Body) != "other" {
		t.Fatalf("cross-origin redirect: %v", err)
	}
	if authOnOther != "|" {
		t.Fatalf("credentials leaked across origins: %q", authOnOther)
	}
}

func TestBodyCapAndGzip(t *testing.T) {
	big := strings.Repeat("a", 2048)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/gz" {
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			io.WriteString(gz, big)
			gz.Close()
			return
		}
		io.WriteString(w, big)
	}))
	defer srv.Close()
	c := testClient(t, Options{MaxBodyBytes: 1024})
	if _, err := c.Do(context.Background(), Request{URL: srv.URL + "/plain"}); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("expected body cap, got %v", err)
	}
	// The cap applies to decoded bytes, so a small gzip bomb is still capped.
	if _, err := c.Do(context.Background(), Request{URL: srv.URL + "/gz"}); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("expected body cap on decoded gzip, got %v", err)
	}
	resp, err := c.Do(context.Background(), Request{URL: srv.URL + "/gz", MaxBodyBytes: 4096})
	if err != nil || string(resp.Body) != big {
		t.Fatalf("gzip decode: %v", err)
	}
}

func TestTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	c := testClient(t, Options{})
	start := time.Now()
	_, err := c.Do(context.Background(), Request{URL: srv.URL, Timeout: 100 * time.Millisecond})
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("timeout not enforced: %v after %s", err, time.Since(start))
	}
}

func TestRobots(t *testing.T) {
	var robotsHits atomic.Int32
	robots := "User-agent: *\nDisallow: /\n\nUser-agent: OpenGTM\nUser-agent: other\nDisallow: /private\nAllow: /private/ok\nDisallow: /*.pdf$\nCrawl-delay: 0.2\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			robotsHits.Add(1)
			io.WriteString(w, robots)
			return
		}
		io.WriteString(w, "page")
	}))
	defer srv.Close()
	c := testClient(t, Options{})
	ctx := context.Background()
	check := func(path string, want error) {
		t.Helper()
		_, err := c.Do(ctx, Request{URL: srv.URL + path, Robots: true, RPS: 1000})
		if !errors.Is(err, want) && !(want == nil && err == nil) {
			t.Fatalf("%s: got %v want %v", path, err, want)
		}
	}
	start := time.Now()
	check("/public", nil)
	check("/private/x", ErrRobotsDisallowed)
	check("/private/ok/1", nil)
	check("/doc.pdf", ErrRobotsDisallowed)
	check("/doc.pdf?x=1", nil)
	if robotsHits.Load() != 1 {
		t.Fatalf("robots.txt fetched %d times, want 1 (cached)", robotsHits.Load())
	}
	// Three allowed fetches after robots: Crawl-delay 0.2s spaces them.
	if el := time.Since(start); el < 400*time.Millisecond {
		t.Fatalf("crawl-delay not honored: %s", el)
	}
}

func TestRobotsMissingAndServerError(t *testing.T) {
	status := http.StatusNotFound
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(status)
			return
		}
		io.WriteString(w, "page")
	}))
	defer srv.Close()
	c := testClient(t, Options{})
	if _, err := c.Do(context.Background(), Request{URL: srv.URL + "/a", Robots: true}); err != nil {
		t.Fatalf("404 robots must allow: %v", err)
	}
	mu.Lock()
	status = http.StatusServiceUnavailable
	mu.Unlock()
	c2 := testClient(t, Options{})
	if _, err := c2.Do(context.Background(), Request{URL: srv.URL + "/a", Robots: true}); !errors.Is(err, ErrRobotsUnavailable) {
		t.Fatalf("5xx robots must disallow, got %v", err)
	}
}

func TestRobotsParsing(t *testing.T) {
	r := ParseRobots([]byte("# comment\nUser-agent: *\nDisallow: /a\nAllow: /a/b\nDisallow: /a/b/c$\nDisallow:\n"))
	cases := map[string]bool{"/": true, "/a": false, "/a/x": false, "/a/b": true, "/a/b/x": true, "/a/b/c": false, "/a/b/cd": true, "/robots.txt": true}
	for p, want := range cases {
		u, _ := url.Parse("https://x.example" + p)
		if got := r.Allowed(u); got != want {
			t.Errorf("%s: got %v want %v", p, got, want)
		}
	}
	tie := ParseRobots([]byte("User-agent: *\nDisallow: /x\nAllow: /x\n"))
	if u, _ := url.Parse("https://x.example/x"); !tie.Allowed(u) {
		t.Error("allow must win equal-length ties")
	}
	none := ParseRobots([]byte("User-agent: googlebot\nDisallow: /\n"))
	if u, _ := url.Parse("https://x.example/x"); !none.Allowed(u) {
		t.Error("no matching group allows everything")
	}
}

func TestRateLimitSharedAcrossGoroutinesAndSubdomains(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	res := fakeResolver{"a.shop.example.com": {"127.0.0.1"}, "b.shop.example.com": {"127.0.0.1"}}
	c := testClient(t, Options{Resolver: res})
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			host := "a.shop.example.com"
			if i%2 == 1 {
				host = "b.shop.example.com"
			}
			if _, err := c.Do(context.Background(), Request{URL: "http://" + host + ":" + port + "/", RPS: 20}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if el := time.Since(start); el < 190*time.Millisecond {
		t.Fatalf("5 requests at 20 rps to one registrable domain took %s, want >= 200ms", el)
	}
	if DomainKey("a.b.example.co.uk") != "example.co.uk" || DomainKey("127.0.0.1") != "127.0.0.1" {
		t.Fatal("domain keys")
	}
	// A wait longer than the deadline fails fast.
	c2 := testClient(t, Options{Resolver: res})
	_, _ = c2.Do(context.Background(), Request{URL: "http://a.shop.example.com:" + port + "/", RPS: 0.1})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := c2.Do(ctx, Request{URL: "http://a.shop.example.com:" + port + "/", RPS: 0.1})
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("expected RateLimitError, got %v", err)
	}
}

// connectProxy is a minimal HTTP CONNECT proxy that records targets.
func connectProxy(t *testing.T, targets *[]string, mu *sync.Mutex) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "connect only", http.StatusMethodNotAllowed)
			return
		}
		mu.Lock()
		*targets = append(*targets, r.Host+"|"+r.Header.Get("Proxy-Authorization"))
		mu.Unlock()
		up, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		hj, _ := w.(http.Hijacker)
		conn, buf, err := hj.Hijack()
		if err != nil {
			up.Close()
			return
		}
		io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n")
		go func() { io.Copy(up, buf); up.Close() }()
		go func() { io.Copy(conn, up); conn.Close() }()
	}))
}

func TestProxyTunnelsToValidatedIP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "via proxy "+r.Host)
	}))
	defer srv.Close()
	var targets []string
	var mu sync.Mutex
	proxy := connectProxy(t, &targets, &mu)
	defer proxy.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	pu, _ := url.Parse(proxy.URL)
	pu.User = url.UserPassword("u", "p")
	c := testClient(t, Options{ProxyURL: pu.String(), Resolver: fakeResolver{"site.test": {"127.0.0.1"}}})
	resp, err := c.Do(context.Background(), Request{URL: "http://site.test:" + port + "/"})
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Body) != "via proxy site.test:"+port {
		t.Fatalf("body %q", resp.Body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(targets) != 1 || !strings.HasPrefix(targets[0], "127.0.0.1:"+port+"|Basic ") {
		t.Fatalf("CONNECT must target the validated IP with credentials: %v", targets)
	}
	// With the guard on, a target resolving to a private IP never reaches the proxy.
	guarded, _ := New(Options{ProxyURL: proxy.URL, Resolver: fakeResolver{"site.test": {"10.1.2.3"}}})
	if _, err := guarded.Do(context.Background(), Request{URL: "http://site.test:" + port + "/"}); err == nil {
		t.Fatal("private target behind proxy must be blocked")
	}
	if len(targets) != 1 {
		t.Fatalf("blocked target reached the proxy: %v", targets)
	}
}

func TestTLSWithPinnedDial(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "secure")
	}))
	defer srv.Close()
	c := testClient(t, Options{TLSConfig: srv.Client().Transport.(*http.Transport).TLSClientConfig})
	resp, err := c.Do(context.Background(), Request{URL: srv.URL})
	if err != nil || string(resp.Body) != "secure" {
		t.Fatalf("tls: %v", err)
	}
}

func TestPostRedirectSemantics(t *testing.T) {
	var methods []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		methods = append(methods, r.Method+" "+r.URL.Path+" "+string(body))
		mu.Unlock()
		switch r.URL.Path {
		case "/303":
			http.Redirect(w, r, "/end", http.StatusSeeOther)
		case "/307":
			http.Redirect(w, r, "/end", http.StatusTemporaryRedirect)
		default:
			io.WriteString(w, "end")
		}
	}))
	defer srv.Close()
	c := testClient(t, Options{})
	for _, p := range []string{"/303", "/307"} {
		if _, err := c.Do(context.Background(), Request{Method: "POST", URL: srv.URL + p, Body: []byte("payload")}); err != nil {
			t.Fatal(err)
		}
	}
	want := "POST /303 payload,GET /end ,POST /307 payload,POST /end payload"
	if got := strings.Join(methods, ","); got != want {
		t.Fatalf("got %s", got)
	}
}
