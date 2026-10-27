package leadweb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
)

// Upstream proxies are chosen here (Go owns PROXY_LIST and PROXY_MANAGER_URL)
// and passed per request to the browser service, which chains its own egress
// guard in front of them and never stores them. Proxy URLs can carry
// credentials: never log them.

// ── PROXY_LIST pool (leadgen/proxy_pool.py) ──

const proxyCooldown = 30 * time.Minute

// ProxyPool rotates PROXY_LIST proxies with domain affinity and a cooldown for
// proxies that got blocked. Process-local, safe for concurrent use.
//
// Deviations from legacy: the research/fresh-proxy-list files are not read
// (not shipped in any image); an entry that already has a scheme
// (socks5://h:p) is used as is (legacy prefixed a second scheme).
type ProxyPool struct {
	mu      sync.Mutex
	http    []string
	socks5  []string
	socks4  []string
	all     []string
	domain  map[string]string
	blocked map[string]time.Time
	now     func() time.Time
	choose  func(n int) int
}

// NewProxyPool parses a comma-separated PROXY_LIST.
func NewProxyPool(list string) *ProxyPool {
	p := &ProxyPool{domain: map[string]string{}, blocked: map[string]time.Time{}, now: time.Now,
		choose: func(n int) int { return rand.IntN(n) }} //nolint:gosec // rotation, not security
	for _, e := range strings.Split(list, ",") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if i := strings.Index(e, "://"); i > 0 {
			switch strings.ToLower(e[:i]) {
			case "socks5", "socks5h":
				p.socks5 = append(p.socks5, e)
			case "socks4", "socks4a":
				p.socks4 = append(p.socks4, e)
			default:
				p.http = append(p.http, e)
			}
			continue
		}
		switch {
		case strings.Contains(e, "socks5"):
			p.socks5 = append(p.socks5, "socks5://"+e)
		case strings.Contains(e, "socks4"):
			p.socks4 = append(p.socks4, "socks4://"+e)
		default:
			p.http = append(p.http, "http://"+e)
		}
	}
	p.all = append(append(append([]string{}, p.http...), p.socks5...), p.socks4...)
	return p
}

// Len is the number of proxies.
func (p *ProxyPool) Len() int { return len(p.all) }

func (p *ProxyPool) available(proxy string) bool {
	until, ok := p.blocked[proxy]
	if !ok {
		return true
	}
	if p.now().After(until) {
		delete(p.blocked, proxy)
		return true
	}
	return false
}

// Get returns a proxy for domain ("" when the pool is empty): the domain's
// sticky proxy while it is healthy, else a random available one (socks5 only
// for tier >= 3 when there are any).
func (p *ProxyPool) Get(domain string, tier int) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.all) == 0 {
		return ""
	}
	if a, ok := p.domain[domain]; domain != "" && ok && p.available(a) {
		return a
	}
	pool := p.all
	if tier >= 3 && len(p.socks5) > 0 {
		pool = p.socks5
	}
	var avail []string
	for _, x := range pool {
		if p.available(x) {
			avail = append(avail, x)
		}
	}
	if len(avail) == 0 {
		avail = pool
	}
	proxy := avail[p.choose(len(avail))]
	if domain != "" {
		p.domain[domain] = proxy
	}
	return proxy
}

// ReportBlocked puts proxy in cooldown and drops its domain assignment.
func (p *ProxyPool) ReportBlocked(proxy, domain string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.blocked[proxy] = p.now().Add(proxyCooldown)
	if domain != "" && p.domain[domain] == proxy {
		delete(p.domain, domain)
	}
}

// ReportSuccess makes proxy sticky for domain and clears its cooldown.
func (p *ProxyPool) ReportSuccess(proxy, domain string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if domain != "" {
		p.domain[domain] = proxy
	}
	delete(p.blocked, proxy)
}

// PoolStats is the legacy proxy_pool.stats() shape.
type PoolStats struct {
	Total             int `json:"total"`
	HTTP              int `json:"http"`
	Socks5            int `json:"socks5"`
	Socks4            int `json:"socks4"`
	Blocked           int `json:"blocked"`
	DomainAssignments int `json:"domain_assignments"`
}

// Stats counts proxies, cooldowns and domain assignments.
func (p *ProxyPool) Stats() PoolStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return PoolStats{Total: len(p.all), HTTP: len(p.http), Socks5: len(p.socks5), Socks4: len(p.socks4),
		Blocked: len(p.blocked), DomainAssignments: len(p.domain)}
}

// ── proxy manager service (leadgen/proxy_client.py) ──

// ProxyManager fetches rotating proxies from the proxy-manager service
// (PROXY_MANAGER_URL), hands them out round-robin and reports outcomes for
// its scoring. Failures are silent, as legacy: no manager means no proxy.
type ProxyManager struct {
	base string // never logged: may carry credentials
	hc   *http.Client

	mu    sync.Mutex
	cache []managerProxy
	idx   int
}

type managerProxy struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
}

// NewProxyManager returns a client for base. Its host is the only internal
// address the client may reach (explicit egress allow-list); hc nil builds that client.
func NewProxyManager(base string, hc *http.Client) *ProxyManager {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if hc == nil {
		hc = egress.NewOperatorClient(allowHostsFor(base))
	}
	return &ProxyManager{base: base, hc: hc}
}

// allowHostsFor is the egress allow-list entry ("host:port") for an operator
// configured internal service URL.
func allowHostsFor(raw string) []string {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return nil
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if strings.EqualFold(u.Scheme, "https") {
			port = "443"
		}
	}
	return []string{strings.ToLower(u.Hostname()) + ":" + port}
}

func (m *ProxyManager) fetch(ctx context.Context, count int) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		m.base+"/api/proxy?limit="+strconv.Itoa(count)+"&maxLatency=4000", nil)
	if err != nil {
		return
	}
	req.Header.Set("Accept", "application/json")
	resp, err := m.hc.Do(req)
	if err != nil {
		slog.Debug("leadweb: proxy manager unavailable")
		return
	}
	defer resp.Body.Close()
	var data struct {
		Success bool           `json:"success"`
		Proxies []managerProxy `json:"proxies"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&data) != nil {
		return
	}
	if data.Success && len(data.Proxies) > 0 {
		m.cache, m.idx = data.Proxies, 0
	}
}

// Get returns the next proxy URL ("socks5://host:port") or "" when the
// manager has none; protocol, when set, prefers a proxy of that protocol.
func (m *ProxyManager) Get(ctx context.Context, protocol string) string {
	if m == nil || m.base == "" {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.idx >= len(m.cache) {
		m.fetch(ctx, 20)
	}
	if len(m.cache) == 0 {
		return ""
	}
	p := m.cache[m.idx%len(m.cache)]
	m.idx++
	proto := p.Protocol
	if proto == "" {
		proto = "http"
	}
	if protocol != "" && proto != protocol {
		for _, q := range m.cache {
			if q.Protocol == protocol {
				return fmt.Sprintf("%s://%s:%d", protocol, q.Host, q.Port)
			}
		}
	}
	return fmt.Sprintf("%s://%s:%d", proto, p.Host, p.Port)
}

// Report scores the proxy behind proxyURL (non-critical; errors ignored).
func (m *ProxyManager) Report(ctx context.Context, proxyURL string, success bool) {
	if m == nil || m.base == "" {
		return
	}
	u, err := url.Parse(proxyURL)
	if err != nil || u.Hostname() == "" || u.Port() == "" {
		return
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port == 0 {
		return
	}
	status := "failure"
	if success {
		status = "success"
	}
	host, _ := json.Marshal(u.Hostname())
	body := fmt.Sprintf(`{"host": %s, "port": %d, "status": %q}`, host, port, status) // json.dumps spacing
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.base+"/api/report", strings.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if resp, err := m.hc.Do(req); err == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
	}
}
