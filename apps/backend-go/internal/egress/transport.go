package egress

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"time"
)

type resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// NewClient returns the shared SSRF-guarded client. Bench allowlist hosts are
// honoured only when APP_ENV is test or bench (as in the Python guard).
func NewClient(allowHosts []string) *http.Client {
	return NewClientForEnv(os.Getenv("APP_ENV"), allowHosts)
}

// NewClientForEnv is NewClient with an explicit app environment (config.AppEnv).
func NewClientForEnv(appEnv string, allowHosts []string) *http.Client {
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	return newClient(allowHosts, appEnv, net.DefaultResolver, d.DialContext)
}

func newClient(allowHosts []string, appEnv string, res resolver, dial dialFunc) *http.Client {
	allow := map[string]struct{}{}
	if env := strings.ToLower(strings.TrimSpace(appEnv)); env == "test" || env == "bench" {
		for _, h := range allowHosts {
			if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
				allow[h] = struct{}{}
			}
		}
	}
	tr := &http.Transport{
		Proxy:               nil, // a proxy would bypass the dial-time vetting
		DialContext:         guardedDial(allow, res, dial),
		ForceAttemptHTTP2:   true,
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	return &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// guardedDial resolves the host once, rejects if ANY address is blocked, then
// dials a vetted IP literal (never the hostname), closing the rebinding window.
func guardedDial(allow map[string]struct{}, res resolver, dial dialFunc) dialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		host = strings.ToLower(host)
		if _, ok := allow[host]; ok {
			return dial(ctx, network, addr)
		}
		if _, ok := blockedHostnames[host]; ok {
			return nil, blocked("host '%s' is blocked", host)
		}
		var ips []netip.Addr
		if ip, ok := parseIPAnyEncoding(host); ok {
			ips = []netip.Addr{ip}
		} else if ips, err = res.LookupNetIP(ctx, "ip", host); err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, blocked("host %q did not resolve", host)
		}
		for _, ip := range ips {
			if IsBlockedIP(ip) {
				return nil, blocked("host resolves to private ip %s", ip)
			}
		}
		var lastErr error
		for _, ip := range ips {
			c, err := dial(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return c, nil
			}
			lastErr = err
		}
		return nil, errors.Join(lastErr)
	}
}
