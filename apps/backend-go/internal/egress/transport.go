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
	return newClientSet(allowSet(appEnv, allowHosts), res, dial)
}

// NewOperatorClient is the provider egress client with the operator allow-list
// (EGRESS_ALLOW_HOSTS): each exact "host" or "host:port" entry skips the
// private-IP block in every APP_ENV, so a self-hosted private service such as
// Reacher is reachable. It is deliberately NOT used for the http column, SMTP
// probes or any user-supplied URL: only operator-configured provider targets.
// Everything not listed is vetted exactly as NewClient does.
func NewOperatorClient(allowHosts []string) *http.Client {
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	return newOperatorClient(allowHosts, net.DefaultResolver, d.DialContext)
}

func newOperatorClient(allowHosts []string, res resolver, dial dialFunc) *http.Client {
	return newClientSet(operatorAllowSet(allowHosts), res, dial)
}

func newClientSet(allow map[string]struct{}, res resolver, dial dialFunc) *http.Client {
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
		if _, ok := allow[net.JoinHostPort(host, port)]; ok {
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

// allowSet is the lower-cased allowlist, empty unless appEnv is test or bench.
func allowSet(appEnv string, allowHosts []string) map[string]struct{} {
	allow := map[string]struct{}{}
	if env := strings.ToLower(strings.TrimSpace(appEnv)); env == "test" || env == "bench" {
		for _, h := range allowHosts {
			if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
				allow[h] = struct{}{}
			}
		}
	}
	return allow
}

// operatorAllowSet is the lower-cased operator allow-list, honoured in every
// environment. Entries are exact: "host" matches that host on any port,
// "host:port" only that port. Blank entries are ignored.
func operatorAllowSet(allowHosts []string) map[string]struct{} {
	allow := map[string]struct{}{}
	for _, h := range allowHosts {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			allow[h] = struct{}{}
		}
	}
	return allow
}
