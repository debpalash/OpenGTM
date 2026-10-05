//go:build linux

package process

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/egress"
)

// echoServer accepts TCP connections and echoes bytes back.
func echoServer(t testing.TB) (port int, conns func() int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var mu sync.Mutex
	open := 0
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			open++
			mu.Unlock()
			go func() {
				defer func() { mu.Lock(); open--; mu.Unlock() }()
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, func() int { mu.Lock(); defer mu.Unlock(); return open }
}

func localClient(t testing.TB, allowPrivate bool) *egress.Client {
	t.Helper()
	c, err := egress.New(egress.Options{AllowPrivateForTesting: allowPrivate, Transport: okTransport(), ReplayWithoutRateLimit: true})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func tunnelResult(t testing.TB, out *Outcome) map[string]any {
	t.Helper()
	return fieldsOf(t, out)
}

func TestTunnelReachesOnlyDeclaredHosts(t *testing.T) {
	port, _ := echoServer(t)
	other, _ := echoServer(t)
	s := newSup(t, Options{Client: localClient(t, true)})
	p := pspec{network: []string{fmt.Sprintf("https://127.0.0.1:%d", port)}}.build(t)
	target := fmt.Sprintf("127.0.0.1:%d", port)

	out := mustRun(t, s, p, map[string]any{"mode": "tunnel", "target": target}, nil)
	f := tunnelResult(t, out)
	if f["status"] != float64(200) || f["echo"] != "ping-through-tunnel" {
		t.Fatalf("declared host: %v", f)
	}
	// The same URL is in the init frame and the environment, for SDKs that
	// read HTTPS_PROXY and for code that wants the URL explicitly.
	if f["proxy_url"] == nil || f["proxy_url"] != f["env_https"] || f["env_http"] != f["env_https"] {
		t.Fatalf("proxy settings: %v", f)
	}
	// The host records what crossed each tunnel.
	tunnels := out.Records[0].Evidence["tunnels"].([]TunnelEvidence)
	if len(tunnels) != 1 || tunnels[0].Host != target || tunnels[0].Tunnels != 1 ||
		tunnels[0].BytesUp != int64(len("ping-through-tunnel")) || tunnels[0].BytesDown != int64(len("ping-through-tunnel")) {
		t.Fatalf("tunnel evidence: %+v", tunnels)
	}

	for _, denied := range []string{
		fmt.Sprintf("127.0.0.1:%d", other), // right host, undeclared port
		"evil.example.net:443",             // undeclared host
		fmt.Sprintf("localhost:%d", port),  // a different name for the same machine
	} {
		f := tunnelResult(t, mustRun(t, s, p, map[string]any{"mode": "tunnel", "target": denied}, nil))
		if f["status"] != float64(403) || !strings.Contains(fmt.Sprint(f["header"]), "X-Opengtm-Error: capability_denied") {
			t.Errorf("%s: %v", denied, f)
		}
	}
}

func TestTunnelNeverBypassesTheDestinationGuard(t *testing.T) {
	port, _ := echoServer(t)
	s := newSup(t, Options{Client: localClient(t, false)}) // production guard: no loopback
	// Even a manifest that declares the loopback address cannot tunnel to it.
	p := pspec{network: []string{fmt.Sprintf("https://127.0.0.1:%d", port), "https://*"}}.build(t)
	for _, target := range []string{fmt.Sprintf("127.0.0.1:%d", port), "169.254.169.254:443", "10.0.0.1:443", "[::1]:443"} {
		f := tunnelResult(t, mustRun(t, s, p, map[string]any{"mode": "tunnel", "target": target}, nil))
		if f["status"] != float64(403) || !strings.Contains(fmt.Sprint(f["header"]), "blocked_url") {
			t.Errorf("%s: %v", target, f)
		}
	}
}

func TestTunnelCredentialsArePerRunAndRevoked(t *testing.T) {
	port, _ := echoServer(t)
	s := newSup(t, Options{Client: localClient(t, true)})
	p := pspec{network: []string{fmt.Sprintf("https://127.0.0.1:%d", port)}}.build(t)
	target := fmt.Sprintf("127.0.0.1:%d", port)

	for name, cred := range map[string]string{"no credentials": "", "wrong password": "run:not-the-token", "wrong user": "admin:x"} {
		f := tunnelResult(t, mustRun(t, s, p, map[string]any{"mode": "tunnel", "target": target, "credentials": cred}, nil))
		if f["status"] != float64(407) {
			t.Errorf("%s: %v", name, f)
		}
	}

	// A token leaked from a finished run is useless.
	f := tunnelResult(t, mustRun(t, s, p, map[string]any{"mode": "tunnel", "target": target}, nil))
	proxyURL := f["proxy_url"].(string)
	if status := connectStatus(t, proxyURL, target); status != 407 {
		t.Fatalf("a finished run's proxy credentials still work: %d", status)
	}
}

func TestTunnelCredentialsAreBoundToTheirPluginsCapabilities(t *testing.T) {
	portA, _ := echoServer(t)
	portB, _ := echoServer(t)
	s := newSup(t, Options{Client: localClient(t, true), MaxProcesses: 4, MaxPerPlugin: 2})
	a := pspec{name: "plugin_a", network: []string{fmt.Sprintf("https://127.0.0.1:%d", portA)}, timeout: 30}.build(t)
	b := pspec{name: "plugin_b", network: []string{fmt.Sprintf("https://127.0.0.1:%d", portB)}}.build(t)

	// Plugin A holds its run open, so its credentials are live while B runs.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, _ = s.Run(ctx, a, Request{Inputs: map[string]any{"mode": "sleep", "seconds": 30}}) }()
	for s.Stats().Running == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	f := tunnelResult(t, mustRun(t, s, b, map[string]any{"mode": "tunnel", "target": fmt.Sprintf("127.0.0.1:%d", portA)}, nil))
	if f["status"] != float64(403) {
		t.Fatalf("plugin B reached plugin A's host: %v", f)
	}
}

func TestPlainHTTPThroughTheProxyIsRefused(t *testing.T) {
	port, _ := echoServer(t)
	s := newSup(t, Options{Client: localClient(t, true)})
	p := pspec{network: []string{fmt.Sprintf("http://127.0.0.1:%d", port)}}.build(t)
	f := tunnelResult(t, mustRun(t, s, p, map[string]any{"mode": "plain_proxy"}, nil))
	if f["status"] != float64(405) {
		t.Fatalf("plain HTTP via the proxy: %v", f)
	}
}

func TestProxyIsOnlyOfferedWhereItIsSafeAndUseful(t *testing.T) {
	client := localClient(t, true)
	cases := []struct {
		name string
		opts Options
		spec pspec
		want bool
	}{
		{"provider with network", Options{}, pspec{network: []string{"https://api.example.com"}}, true},
		{"function with network", Options{}, pspec{kind: "function", network: []string{"https://api.example.com"}}, true},
		{"no declared network", Options{}, pspec{}, false},
		{"scraper without browser: tunnels would bypass robots.txt", Options{}, pspec{kind: "scraper", network: []string{"https://api.example.com"}}, false},
		{"scraper declaring browser", Options{}, pspec{kind: "scraper", browser: true, network: []string{"https://api.example.com"}}, true},
		{"proxy disabled", Options{DisableProxy: true}, pspec{network: []string{"https://api.example.com"}}, false},
		{"sandbox without a network", Options{Launcher: BwrapLauncher{}}, pspec{network: []string{"https://api.example.com"}}, false},
		{"sandbox sharing the network", Options{Launcher: BwrapLauncher{ShareNet: true}}, pspec{network: []string{"https://api.example.com"}}, true},
		{"no egress client", Options{}, pspec{network: []string{"https://api.example.com"}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opts := c.opts
			if c.name != "no egress client" {
				opts.Client = client
			}
			s := newSup(t, opts)
			tp, run, u := s.tunnelFor(c.spec.build(t))
			if (tp != nil) != c.want || (u != "") != c.want {
				t.Fatalf("offered=%v, want %v", tp != nil, c.want)
			}
			if tp != nil {
				tp.unregister(run)
			}
		})
	}
	// And when it is not offered, nothing about it is in the environment.
	s := newSup(t, Options{Client: client, DisableProxy: true})
	f := fieldsOf(t, mustRun(t, s, pspec{network: []string{"https://api.example.com"}}.build(t), map[string]any{"mode": "noproxy"}, nil))
	if f["proxy"] != nil || f["https"] != nil || f["http"] != nil {
		t.Fatalf("proxy leaked into a plugin that should not have one: %v", f)
	}
}

func TestTunnelsCloseWhenTheRunEndsAndAreBounded(t *testing.T) {
	port, open := echoServer(t)
	tp, err := newTunnelProxy(localClient(t, true))
	if err != nil {
		t.Fatal(err)
	}
	defer tp.close()
	p := pspec{network: []string{fmt.Sprintf("https://127.0.0.1:%d", port)}}.build(t)
	run, proxyURL := tp.register(p)
	target := fmt.Sprintf("127.0.0.1:%d", port)

	var conns []net.Conn
	for i := 0; i < maxTunnelsPerRun; i++ {
		c, status := openTunnel(t, proxyURL, target)
		if status != 200 {
			t.Fatalf("tunnel %d: %d", i, status)
		}
		conns = append(conns, c)
	}
	if _, status := openTunnel(t, proxyURL, target); status != 429 {
		t.Fatalf("tunnel beyond the per-run limit: %d", status)
	}
	deadline := time.Now().Add(3 * time.Second)
	for open() != maxTunnelsPerRun && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if open() != maxTunnelsPerRun {
		t.Fatalf("upstream connections: %d", open())
	}

	tp.unregister(run) // the run ends
	for _, c := range conns {
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, err := c.Read(make([]byte, 1)); err == nil {
			t.Fatal("tunnel still open after the run ended")
		}
		_ = c.Close()
	}
	deadline = time.Now().Add(3 * time.Second)
	for open() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if open() != 0 {
		t.Fatalf("%d upstream connections left open", open())
	}
}

// connectStatus performs a CONNECT through proxyURL (which carries the
// credentials) and returns the status.
func connectStatus(t testing.TB, proxyURL, target string) int {
	t.Helper()
	c, status := openTunnel(t, proxyURL, target)
	if c != nil {
		_ = c.Close()
	}
	return status
}

func openTunnel(t testing.TB, proxyURL, target string) (net.Conn, int) {
	t.Helper()
	u, err := parseProxyURL(proxyURL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", u.host, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	req := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	if u.user != "" {
		req += "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(u.user+":"+u.pass)) + "\r\n"
	}
	_, _ = io.WriteString(conn, req+"\r\n")
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("CONNECT response: %v", err)
	}
	_ = conn.SetReadDeadline(time.Time{})
	if resp.StatusCode != 200 {
		_ = conn.Close()
		return nil, resp.StatusCode
	}
	return conn, 200
}

type proxyParts struct{ host, user, pass string }

func parseProxyURL(raw string) (proxyParts, error) {
	rest, ok := strings.CutPrefix(raw, "http://")
	if !ok {
		return proxyParts{}, fmt.Errorf("proxy URL %q", raw)
	}
	var p proxyParts
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		p.user, p.pass, _ = strings.Cut(rest[:at], ":")
		rest = rest[at+1:]
	}
	p.host = rest
	return p, nil
}
