// Package egress is the single guarded HTTP client used by every plugin and
// by Go providers. It enforces OpenGTM's outbound-request protections:
//
//   - SSRF guard (port of apps/api/core/url_guard.py): http/https only,
//     blocked hostnames, private/loopback/link-local/metadata/CGNAT/multicast
//     and reserved ranges (IPv4, IPv6, IPv4-mapped/NAT64/6to4), and
//     decimal/octal/hex host encodings.
//   - DNS pinning: a host is resolved once, every address is validated, and
//     the connection is dialed to a validated IP, so DNS rebinding cannot swap
//     the target. Every redirect hop (max 5) is validated the same way, and
//     credentials are dropped on cross-origin redirects.
//   - Response size cap (default 5 MiB, after gzip decoding), timeouts, and a
//     single pooled Transport.
//   - robots.txt per origin (cached 1 h, bounded LRU), honoring the
//     "OpenGTM" group then "*", longest-match Allow/Disallow and Crawl-delay.
//     A missing robots.txt (4xx) allows everything; a 5xx or network failure
//     disallows (conservative) and is re-checked after a minute.
//   - Per-domain (eTLD+1) rate limiting shared across goroutines.
//   - An identifying User-Agent that plugins cannot override.
//   - Optional HTTP CONNECT proxy; the tunnel is opened to the validated IP,
//     so the guard still pins the final target.
//
// Every response carries Evidence: final URL, status, fetch time, content
// type, byte count and SHA-256 of the body.
package egress

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Defaults.
const (
	DefaultMaxBodyBytes = 5 << 20
	DefaultTimeout      = 30 * time.Second
	DefaultMaxRedirects = 5
	DefaultRPS          = 1.0
	RobotsTTL           = time.Hour
	robotsFailureTTL    = time.Minute
	ProjectURL          = "https://github.com/debpalash/OpenGTM"
)

// Options configures a Client.
type Options struct {
	// Version is embedded in the User-Agent ("OpenGTM/<version> (+url)").
	Version string
	// MaxBodyBytes caps decoded response bodies (default 5 MiB).
	MaxBodyBytes int64
	// Timeout bounds one Do call including redirects (default 30s).
	Timeout time.Duration
	// MaxRedirects bounds redirect hops (default 5).
	MaxRedirects int
	// DefaultRPS is the per-domain rate when a request does not set one (default 1).
	DefaultRPS float64
	// ProxyURL is an optional HTTP proxy (http://[user:pass@]host:port) used
	// through CONNECT tunnels to the validated target IP.
	ProxyURL string
	// RobotsCacheSize bounds cached origins (default 1024).
	RobotsCacheSize int
	// AllowPrivateForTesting disables private-address blocking so tests can
	// use httptest servers. It must never be set from configuration.
	AllowPrivateForTesting bool
	// TLSConfig overrides TLS settings (tests use it to trust httptest certs).
	TLSConfig *tls.Config
	// Resolver overrides DNS resolution (tests use it to simulate rebinding).
	Resolver interface {
		LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
	}
	// Transport replaces the network entirely (fixture replay). The guard,
	// robots, rate limits and caps still apply on top of it.
	Transport http.RoundTripper
	// ReplayWithoutRateLimit disables per-domain spacing. Only for offline
	// fixture replay through Transport, where no real server is contacted.
	ReplayWithoutRateLimit bool
	// OnResponse observes every completed exchange, including redirect hops
	// and robots.txt fetches (used by `opengtm plugin record`).
	OnResponse func(method, url string, resp *Response)
	// Now overrides the clock (tests).
	Now func() time.Time
}

// Client is safe for concurrent use. Create one per process and share it.
type Client struct {
	opts      Options
	guard     *Guard
	transport http.RoundTripper
	ua        string
	robots    *robotsCache
	limiter   *domainLimiter
	proxy     *url.URL
	now       func() time.Time
}

// Request is one outbound request.
type Request struct {
	Method string
	URL    string
	Header http.Header
	Body   []byte
	// Robots makes the fetch honor robots.txt (scrapers). API providers
	// calling documented endpoints leave it off, like the Python runtime.
	Robots bool
	// RPS is the per-domain rate for this request (0 = client default).
	RPS float64
	// MaxBodyBytes overrides the client cap when > 0 (it can only lower it
	// below the plugin's declared limit; callers pass the plugin limit).
	MaxBodyBytes int64
	// Timeout overrides the client timeout when > 0.
	Timeout time.Duration
	// Allow is applied to the initial URL and every redirect hop, e.g. the
	// plugin's network capability.
	Allow func(*url.URL) error
	// SensitiveHeaders are removed when a redirect leaves the origin
	// (Authorization, Cookie and Proxy-Authorization always are).
	SensitiveHeaders []string
}

// Evidence describes what was fetched.
type Evidence struct {
	URL         string    `json:"url"`
	Status      int       `json:"status"`
	FetchedAt   time.Time `json:"fetched_at"`
	ContentType string    `json:"content_type"`
	Bytes       int       `json:"bytes"`
	SHA256      string    `json:"sha256"`
}

// Response is a fully read response.
type Response struct {
	Status   int
	Header   http.Header
	Body     []byte
	Evidence Evidence
}

// Sentinel errors.
var (
	ErrBodyTooLarge       = errors.New("response body exceeds the size limit")
	ErrTooManyRedirects   = errors.New("too many redirects")
	ErrRobotsDisallowed   = errors.New("robots.txt disallows this URL for OpenGTM")
	ErrRobotsUnavailable  = errors.New("robots.txt is unavailable (server error); fetch refused")
	errNoLocationRedirect = errors.New("redirect without Location")
)

// New builds a Client with one pooled Transport.
func New(opts Options) (*Client, error) {
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.MaxRedirects <= 0 {
		opts.MaxRedirects = DefaultMaxRedirects
	}
	if opts.DefaultRPS <= 0 {
		opts.DefaultRPS = DefaultRPS
	}
	if opts.Version == "" {
		opts.Version = "dev"
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	c := &Client{
		opts:    opts,
		guard:   &Guard{AllowPrivateForTesting: opts.AllowPrivateForTesting, Resolver: opts.Resolver},
		ua:      fmt.Sprintf("OpenGTM/%s (+%s)", opts.Version, ProjectURL),
		robots:  newRobotsCache(opts.RobotsCacheSize),
		limiter: newDomainLimiter(now),
		now:     now,
	}
	if opts.ProxyURL != "" {
		p, err := url.Parse(opts.ProxyURL)
		if err != nil || p.Scheme != "http" || p.Host == "" {
			return nil, fmt.Errorf("egress: proxy must be an http://host:port URL")
		}
		c.proxy = p
	}
	if opts.Transport != nil {
		c.transport = opts.Transport
	} else {
		c.transport = &http.Transport{
			Proxy:                 nil, // proxying is done by dial() to keep IP pinning
			DialContext:           c.dial,
			TLSClientConfig:       opts.TLSConfig,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          256,
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			ExpectContinueTimeout: time.Second,
		}
	}
	return c, nil
}

// UserAgent returns the identifying User-Agent.
func (c *Client) UserAgent() string { return c.ua }

// Guard exposes the URL guard.
func (c *Client) Guard() *Guard { return c.guard }

// dial resolves and validates the target, then connects to a validated IP
// (directly or through a CONNECT tunnel).
func (c *Client) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := c.guard.Resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	var lastErr error
	for _, ip := range ips {
		target := net.JoinHostPort(ip.String(), port)
		var conn net.Conn
		if c.proxy != nil {
			conn, err = c.dialProxy(ctx, d, target)
		} else {
			conn, err = d.DialContext(ctx, network, target)
		}
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// dialProxy opens a CONNECT tunnel to an already validated ip:port.
func (c *Client) dialProxy(ctx context.Context, d *net.Dialer, target string) (net.Conn, error) {
	proxyAddr := c.proxy.Host
	if c.proxy.Port() == "" {
		proxyAddr = net.JoinHostPort(c.proxy.Hostname(), "80")
	}
	conn, err := d.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("proxy dial: %w", err)
	}
	req := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	if u := c.proxy.User; u != nil {
		pw, _ := u.Password()
		cred := base64.StdEncoding.EncodeToString([]byte(u.Username() + ":" + pw))
		req += "Proxy-Authorization: Basic " + cred + "\r\n"
	}
	req += "\r\n"
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if _, err := io.WriteString(conn, req); err != nil {
		conn.Close()
		return nil, fmt.Errorf("proxy connect: %w", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("proxy connect: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("proxy connect: %s", resp.Status)
	}
	_ = conn.SetDeadline(time.Time{})
	if br.Buffered() > 0 {
		conn.Close()
		return nil, errors.New("proxy connect: unexpected data after response")
	}
	return conn, nil
}

func origin(u *url.URL) string {
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
}

// checkHop applies the static guard and the caller's capability check.
func (c *Client) checkHop(u *url.URL, allow func(*url.URL) error) error {
	if err := c.guard.CheckURL(u); err != nil {
		return err
	}
	if u.User != nil {
		return blocked("credentials in URLs are not allowed")
	}
	if allow != nil {
		if err := allow(u); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) interval(rps float64, crawlDelay time.Duration) time.Duration {
	if rps <= 0 {
		rps = c.opts.DefaultRPS
	}
	iv := time.Duration(float64(time.Second) / rps)
	if crawlDelay > iv {
		iv = crawlDelay
	}
	return iv
}

// Do performs a guarded request, following and validating redirects.
func (c *Client) Do(ctx context.Context, r Request) (*Response, error) {
	timeout := c.opts.Timeout
	if r.Timeout > 0 {
		timeout = r.Timeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	maxBody := c.opts.MaxBodyBytes
	if r.MaxBodyBytes > 0 {
		maxBody = r.MaxBodyBytes
	}
	method := strings.ToUpper(r.Method)
	if method == "" {
		method = http.MethodGet
	}
	u, err := url.Parse(strings.TrimSpace(r.URL))
	if err != nil {
		return nil, blocked("unparseable url")
	}
	header := r.Header.Clone()
	if header == nil {
		header = http.Header{}
	}
	body := r.Body
	firstOrigin := origin(u)
	for hop := 0; ; hop++ {
		if hop > c.opts.MaxRedirects {
			return nil, ErrTooManyRedirects
		}
		if err := c.checkHop(u, r.Allow); err != nil {
			return nil, err
		}
		var crawlDelay time.Duration
		if r.Robots {
			rb, err := c.robotsFor(ctx, u)
			if err != nil {
				return nil, err
			}
			if !rb.Allowed(u) {
				return nil, ErrRobotsDisallowed
			}
			crawlDelay = rb.CrawlDelay()
		}
		if !(c.opts.ReplayWithoutRateLimit && c.opts.Transport != nil) {
			if err := c.limiter.wait(ctx, DomainKey(u.Hostname()), c.interval(r.RPS, crawlDelay)); err != nil {
				return nil, err
			}
		}
		resp, err := c.roundTrip(ctx, method, u, header, body, maxBody)
		if err != nil {
			return nil, err
		}
		if c.opts.OnResponse != nil {
			c.opts.OnResponse(method, u.String(), resp)
		}
		if !isRedirect(resp.Status) {
			return resp, nil
		}
		loc := resp.Header.Get("Location")
		if loc == "" {
			return resp, nil
		}
		next, err := u.Parse(loc)
		if err != nil {
			return nil, blocked("invalid redirect location")
		}
		if resp.Status == http.StatusSeeOther || ((resp.Status == http.StatusMovedPermanently || resp.Status == http.StatusFound) && method == http.MethodPost) {
			method, body = http.MethodGet, nil
			header.Del("Content-Type")
			header.Del("Content-Length")
		}
		if origin(next) != firstOrigin {
			for _, h := range append([]string{"Authorization", "Cookie", "Proxy-Authorization"}, r.SensitiveHeaders...) {
				header.Del(h)
			}
		}
		u = next
	}
}

func isRedirect(status int) bool {
	switch status {
	case 301, 302, 303, 307, 308:
		return true
	}
	return false
}

func (c *Client) roundTrip(ctx context.Context, method string, u *url.URL, header http.Header, body []byte, maxBody int64) (*Response, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rdr)
	if err != nil {
		return nil, err
	}
	for k, vs := range header {
		if strings.EqualFold(k, "Host") || strings.EqualFold(k, "User-Agent") {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("User-Agent", c.ua)
	fetchedAt := c.now().UTC()
	resp, err := c.transport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBody {
		return nil, fmt.Errorf("%w (%d bytes)", ErrBodyTooLarge, maxBody)
	}
	sum := sha256.Sum256(data)
	return &Response{
		Status: resp.StatusCode,
		Header: resp.Header,
		Body:   data,
		Evidence: Evidence{
			URL:         u.String(),
			Status:      resp.StatusCode,
			FetchedAt:   fetchedAt,
			ContentType: resp.Header.Get("Content-Type"),
			Bytes:       len(data),
			SHA256:      hex.EncodeToString(sum[:]),
		},
	}, nil
}

// robotsFor returns the (cached) robots rules for u's origin.
func (c *Client) robotsFor(ctx context.Context, u *url.URL) (*Robots, error) {
	key := origin(u)
	now := c.now()
	if rb, ok := c.robots.get(key, now); ok {
		if rb.disallowAll {
			return nil, ErrRobotsUnavailable
		}
		return rb, nil
	}
	robotsURL := &url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/robots.txt"}
	// The host fetches robots.txt itself: the plugin's capability (which may
	// be path-restricted) does not apply; the SSRF guard and pinning do.
	resp, err := c.Do(ctx, Request{Method: http.MethodGet, URL: robotsURL.String(), MaxBodyBytes: robotsMaxBytes * 2})
	var rb *Robots
	ttl := RobotsTTL
	switch {
	case err != nil && errors.Is(err, ErrBodyTooLarge):
		rb = DisallowAllRobots()
		ttl = robotsFailureTTL
	case err != nil:
		var be *BlockedError
		var rl *RateLimitError
		if errors.As(err, &be) || errors.As(err, &rl) || ctx.Err() != nil {
			return nil, err
		}
		rb = DisallowAllRobots()
		ttl = robotsFailureTTL
	case resp.Status >= 200 && resp.Status < 300:
		rb = ParseRobots(resp.Body)
	case resp.Status >= 400 && resp.Status < 500:
		rb = AllowAllRobots()
	default: // 5xx, unexpected 3xx/1xx
		rb = DisallowAllRobots()
		ttl = robotsFailureTTL
	}
	c.robots.put(key, rb, now.Add(ttl))
	if rb.disallowAll {
		return nil, ErrRobotsUnavailable
	}
	return rb, nil
}
