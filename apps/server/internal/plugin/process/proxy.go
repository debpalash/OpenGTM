package process

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/egress"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/manifest"
)

// tunnelProxy is a loopback HTTP CONNECT proxy for plugin code that opens its
// own TLS connections (vendor SDKs, a Playwright browser) and so cannot use
// ctx.fetch. It is a convenience with real enforcement on the paths it covers,
// not a sandbox: it lets an honest library reach exactly the hosts the
// manifest declares, through the guarded egress dialer (SSRF guard, DNS
// pinning, OPENGTM_EGRESS_PROXY), and refuses everything else. Code that
// ignores the proxy settings and opens sockets itself is stopped only by a
// network namespace (BwrapLauncher).
//
// What it deliberately does not do: tunnels carry opaque TLS, so there is no
// URL to check against a path-restricted capability (such patterns never match
// a tunnel), no robots.txt and no per-domain rate limit. Scrapers therefore
// get a tunnel only when they declare capabilities.browser, which admins see.
//
// Credentials are per run (user "run", a random password), bound to that
// run's manifest, and revoked, with every open tunnel closed, when the run
// ends.
type tunnelProxy struct {
	client *egress.Client
	ln     net.Listener
	srv    *http.Server

	mu   sync.Mutex
	runs map[string]*proxyRun // by password
}

type proxyRun struct {
	plugin *manifest.Plugin
	token  string

	mu      sync.Mutex
	closed  bool
	tunnels map[net.Conn]struct{}
	hosts   map[string]*tunnelStats
	denied  atomic.Int64
}

type tunnelStats struct {
	Up, Down atomic.Int64
	Count    atomic.Int64
}

// TunnelEvidence is what the host observed of a run's tunnels.
type TunnelEvidence struct {
	Host      string `json:"host"`
	Tunnels   int64  `json:"tunnels"`
	BytesUp   int64  `json:"bytes_up"`
	BytesDown int64  `json:"bytes_down"`
}

const maxTunnelsPerRun = 16

func newTunnelProxy(client *egress.Client) (*tunnelProxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &tunnelProxy{client: client, ln: ln, runs: map[string]*proxyRun{}}
	p.srv = &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = p.srv.Serve(ln) }()
	return p, nil
}

func (p *tunnelProxy) close() {
	_ = p.srv.Close()
	p.mu.Lock()
	runs := make([]*proxyRun, 0, len(p.runs))
	for _, r := range p.runs {
		runs = append(runs, r)
	}
	p.mu.Unlock()
	for _, r := range runs {
		r.close()
	}
}

// register creates credentials for one run and returns the proxy URL.
func (p *tunnelProxy) register(plugin *manifest.Plugin) (*proxyRun, string) {
	r := &proxyRun{plugin: plugin, token: randHex(16), tunnels: map[net.Conn]struct{}{}, hosts: map[string]*tunnelStats{}}
	p.mu.Lock()
	p.runs[r.token] = r
	p.mu.Unlock()
	return r, "http://run:" + r.token + "@" + p.ln.Addr().String()
}

func (p *tunnelProxy) unregister(r *proxyRun) {
	p.mu.Lock()
	delete(p.runs, r.token)
	p.mu.Unlock()
	r.close()
}

func (r *proxyRun) close() {
	r.mu.Lock()
	r.closed = true
	conns := make([]net.Conn, 0, len(r.tunnels))
	for c := range r.tunnels {
		conns = append(conns, c)
	}
	r.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

func (r *proxyRun) track(c net.Conn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || len(r.tunnels) >= maxTunnelsPerRun {
		return false
	}
	r.tunnels[c] = struct{}{}
	return true
}

func (r *proxyRun) untrack(c net.Conn) {
	r.mu.Lock()
	delete(r.tunnels, c)
	r.mu.Unlock()
}

func (r *proxyRun) stats(host string) *tunnelStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.hosts[host]
	if s == nil {
		s = &tunnelStats{}
		r.hosts[host] = s
	}
	return s
}

// evidence lists what the run's tunnels carried.
func (r *proxyRun) evidence() []TunnelEvidence {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []TunnelEvidence
	for host, s := range r.hosts {
		out = append(out, TunnelEvidence{Host: host, Tunnels: s.Count.Load(), BytesUp: s.Up.Load(), BytesDown: s.Down.Load()})
	}
	return out
}

func (p *tunnelProxy) lookup(r *http.Request) *proxyRun {
	user, pass, ok := parseBasic(r.Header.Get("Proxy-Authorization"))
	if !ok || user != "run" {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runs[pass]
}

func parseBasic(h string) (user, pass string, ok bool) {
	const prefix = "Basic "
	if !strings.HasPrefix(h, prefix) {
		return "", "", false
	}
	raw, err := base64.StdEncoding.DecodeString(h[len(prefix):])
	if err != nil {
		return "", "", false
	}
	user, pass, ok = strings.Cut(string(raw), ":")
	return
}

func deny(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-OpenGTM-Error", code)
	w.WriteHeader(status)
	_, _ = io.WriteString(w, code+": "+msg+"\n")
}

// ServeHTTP handles one proxy request.
func (p *tunnelProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	run := p.lookup(r)
	if run == nil {
		w.Header().Set("Proxy-Authenticate", `Basic realm="opengtm-plugin"`)
		deny(w, http.StatusProxyAuthRequired, "unauthorized", "unknown or finished plugin run")
		return
	}
	if r.Method != http.MethodConnect {
		run.denied.Add(1)
		deny(w, http.StatusMethodNotAllowed, "method_not_allowed",
			"this proxy only tunnels HTTPS (CONNECT); use ctx.fetch for plain requests")
		return
	}
	target := r.Host
	host, port, err := net.SplitHostPort(target)
	if err != nil || host == "" || port == "" {
		deny(w, http.StatusBadRequest, "bad_request", "CONNECT needs host:port")
		return
	}
	// A tunnel carries opaque TLS, so it is judged as an https request to the
	// host's root: capabilities with a path prefix never match it.
	if err := run.plugin.Network().AllowsURL(&url.URL{Scheme: "https", Host: net.JoinHostPort(strings.ToLower(host), port), Path: "/"}); err != nil {
		run.denied.Add(1)
		deny(w, http.StatusForbidden, "capability_denied", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	upstream, err := p.client.DialGuarded(ctx, "tcp", target)
	cancel()
	if err != nil {
		run.denied.Add(1)
		var be *egress.BlockedError
		if errors.As(err, &be) {
			deny(w, http.StatusForbidden, "blocked_url", err.Error())
		} else {
			deny(w, http.StatusBadGateway, "dial_failed", err.Error())
		}
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = upstream.Close()
		deny(w, http.StatusInternalServerError, "internal", "connection cannot be hijacked")
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		_ = upstream.Close()
		return
	}
	if !run.track(client) {
		_, _ = io.WriteString(client, "HTTP/1.1 429 Too Many Requests\r\nConnection: close\r\n\r\n")
		_ = client.Close()
		_ = upstream.Close()
		return
	}
	defer run.untrack(client)
	_, _ = io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
	st := run.stats(strings.ToLower(target))
	st.Count.Add(1)

	var wg sync.WaitGroup
	wg.Add(2)
	pipe := func(dst io.Writer, src io.Reader, counter *atomic.Int64, closeWrite func()) {
		defer wg.Done()
		n, _ := io.Copy(dst, src)
		counter.Add(n)
		closeWrite()
	}
	go func() { // client -> upstream; bytes the client sent before the hijack come first
		defer wg.Done()
		var n int64
		if k := buf.Reader.Buffered(); k > 0 {
			m, _ := io.CopyN(upstream, buf, int64(k))
			n += m
		}
		m, _ := io.Copy(upstream, client)
		st.Up.Add(n + m)
		halfClose(upstream)
	}()
	go pipe(client, upstream, &st.Down, func() { halfClose(client) })
	wg.Wait()
	_ = client.Close()
	_ = upstream.Close()
}

func halfClose(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}
