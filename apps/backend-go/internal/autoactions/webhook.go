// Package autoactions holds the automation action executors. Webhook is a port
// of legacy/python/apps/api/services/automations/actions.py (_act_webhook,
// _send_webhook_pinned, _resolve_webhook_headers) with the hardening listed as
// D4 in testdata/contract/README.md (webhook sender).
package autoactions

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/rowtmpl"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/settings"
)

// Result is one action outcome: Status is success|failed|skipped.
type Result struct{ Status, SkipReason, Summary, Error string }

// Column is a workbook column definition as rowtmpl consumes it.
type Column = colgraph.Col

// SecretGetter resolves a credential for a workspace (*secrets.Resolver).
type SecretGetter interface {
	Get(ctx context.Context, ws, key string) (value, source string, err error)
}

// Bounds of the webhook sender.
const (
	DefaultWebhookTimeout = 15 * time.Second
	MaxResponseBytes      = 1 << 20 // response bytes read; only 160 chars are ever surfaced
	MaxRequestBytes       = 1 << 20
	maxHeaders            = 64
	maxHeaderValue        = 8 << 10
	errCap                = 200
	bodyPrefix            = 160
)

// WebhookDeps are the sender's dependencies. Client must be an egress-guarded
// client (NewWebhookDeps builds one); Allowlist is
// AUTOMATIONS_WEBHOOK_DOMAIN_ALLOWLIST.
type WebhookDeps struct {
	Client     *http.Client
	AppEnv     string   // config AppEnv; egress test hosts only count in test/bench
	AllowHosts []string // egress test/bench allowlist (never effective in production)
	Allowlist  []string // webhook domain allowlist: exact host or ".suffix"
	Secrets    SecretGetter
	Outbound   secrets.OutboundPolicy // which refs header_secret_ref may name (zero value: default prefixes, no registry deny-list)
	// LiteralBody sends the body without {name} substitution (destination webhooks and Slack: the body is data,
	// a lead value such as "{x}" is sent as is and never fails the delivery). url and headers still resolve.
	LiteralBody bool
	Timeout     time.Duration // 0 = DefaultWebhookTimeout
	Log         *slog.Logger  // optional; never receives URLs with queries, headers or secrets
}

// NewWebhookDeps builds the production deps: the dial-time guarded client with
// the webhook hardening, the configured domain allowlist and the secret resolver.
func NewWebhookDeps(appEnv string, egressAllow []string, a config.AutomationsSettings, sec SecretGetter, log *slog.Logger) WebhookDeps {
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	// A registry that cannot load denies every ref: fail closed (secrets.NewOutboundPolicy).
	var registered func(string) bool
	if reg, err := settings.Load(); err == nil {
		registered = reg.SecretSet()
	}
	return WebhookDeps{
		Outbound: secrets.NewOutboundPolicy(config.OutboundSecretRefPrefixes(os.Getenv), registered),
		Client:   newWebhookClient(appEnv, egressAllow, net.DefaultResolver, d.DialContext),
		AppEnv:   appEnv, AllowHosts: egressAllow, Allowlist: a.WebhookDomainAllowlist,
		Secrets: sec, Log: log,
	}
}

var _ SecretGetter = (*secrets.Resolver)(nil)

// ---- extra network policy (beyond internal/egress) ---------------------------

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func isCGNAT(ip netip.Addr) bool {
	if ip.Is4In6() {
		ip = ip.Unmap()
	}
	return ip.Is4() && cgnat.Contains(ip)
}

func ipRefused(ip netip.Addr) bool { return egress.IsBlockedIP(ip) || isCGNAT(ip) }

func isLocalhostName(h string) bool {
	return h == "localhost" || strings.HasSuffix(h, ".localhost") ||
		h == "ip6-localhost" || h == "ip6-loopback" || h == "metadata" || h == "metadata.google.internal"
}

func normHost(h string) string { return strings.TrimSuffix(strings.ToLower(h), ".") }

var reNumPart = regexp.MustCompile(`^(?:0[xX][0-9a-fA-F]*|[0-9]+)$`)

// parseLooseIPv4 is inet_aton: 1-4 dotted numeric parts (decimal, 0x hex, 0
// octal), the last part filling the remaining bytes ("127.1" is 127.0.0.1).
func parseLooseIPv4(h string) (netip.Addr, bool) {
	parts := strings.Split(h, ".")
	if len(parts) == 0 || len(parts) > 4 {
		return netip.Addr{}, false
	}
	vals := make([]uint64, len(parts))
	for i, p := range parts {
		if !reNumPart.MatchString(p) {
			return netip.Addr{}, false
		}
		base, s := 10, p
		switch {
		case len(p) > 1 && (p[1] == 'x' || p[1] == 'X'):
			base, s = 16, p[2:]
			if s == "" {
				s = "0"
			}
		case len(p) > 1 && p[0] == '0':
			base, s = 8, p[1:]
		}
		n, err := strconv.ParseUint(s, base, 64)
		if err != nil {
			return netip.Addr{}, false
		}
		vals[i] = n
	}
	var v uint64
	for i := 0; i < len(vals)-1; i++ {
		if vals[i] > 255 {
			return netip.Addr{}, false
		}
		v |= vals[i] << (8 * (3 - uint(i)))
	}
	last := vals[len(vals)-1]
	if last >= 1<<(8*(4-uint(len(vals)-1))) {
		return netip.Addr{}, false
	}
	v |= last
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}), true
}

func blockedErr(format string, a ...any) error {
	return &egress.BlockedError{Reason: sprintf(format, a...)}
}

type strictResolver struct{ inner egress.Resolver }

func (s strictResolver) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	h := normHost(host)
	if isLocalhostName(h) {
		return nil, blockedErr("host '%s' is blocked", h)
	}
	if ip, ok := parseLooseIPv4(h); ok { // 127.1, 0x7f.1, 2130706433 ...
		if ipRefused(ip) {
			return nil, blockedErr("ip '%s' is private/blocked", ip)
		}
		return []netip.Addr{ip}, nil
	}
	ips, err := s.inner.LookupNetIP(ctx, network, host)
	if err != nil {
		if ctx.Err() != nil { // cancelled or timed out mid-lookup: not a blocked url
			return nil, ctx.Err()
		}
		return nil, blockedErr("cannot resolve host: %s", dnsText(err))
	}
	if len(ips) == 0 {
		return nil, blockedErr("no public address resolved for host")
	}
	for _, ip := range ips {
		if isCGNAT(ip) {
			return nil, blockedErr("host resolves to private ip %s", ip)
		}
	}
	return ips, nil
}

// newWebhookClient is the egress client plus CGNAT / *.localhost / short-form
// IPv4 refusal at resolve and dial time (the egress guard still vets every
// address and dials only the vetted IP literal).
func newWebhookClient(appEnv string, allowHosts []string, res egress.Resolver, dial egress.DialContext) *http.Client {
	strictDial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		if host, _, err := net.SplitHostPort(addr); err == nil {
			if ip, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil && isCGNAT(ip) {
				return nil, blockedErr("ip '%s' is private/blocked", ip.WithZone(""))
			}
		}
		return dial(ctx, network, addr)
	}
	return egress.NewClientWith(appEnv, allowHosts, strictResolver{res}, strictDial)
}

// ---- URL check ---------------------------------------------------------------

func allowlistMatch(host string, allow []string) bool {
	for _, e := range allow {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" || strings.ContainsAny(e, "*?[]") { // no globs
			continue
		}
		if strings.HasPrefix(e, ".") {
			if len(e) > 1 && strings.HasSuffix(host, e) && len(host) > len(e) {
				return true
			}
			continue
		}
		if host == strings.TrimSuffix(e, ".") {
			return true
		}
	}
	return false
}

// checkWebhookURL is _validate_and_pin without DNS: scheme, credentials, host,
// blocked names, domain allowlist, then literal IPs in any encoding. DNS is
// vetted at dial time. The returned error is an *egress.BlockedError.
func checkWebhookURL(d WebhookDeps, cleaned string) error {
	u, err := url.Parse(cleaned)
	if err != nil {
		return blockedErr("unparseable url")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return blockedErr("scheme '%s' not allowed", scheme)
	}
	if u.User != nil {
		if pw, _ := u.User.Password(); u.User.Username() != "" || pw != "" {
			return blockedErr("credentials in url not allowed")
		}
	}
	host := normHost(u.Hostname())
	if host == "" {
		return blockedErr("no host")
	}
	testHost := false
	if env := strings.ToLower(strings.TrimSpace(d.AppEnv)); env == "test" || env == "bench" {
		for _, h := range d.AllowHosts {
			if strings.ToLower(strings.TrimSpace(h)) == host {
				testHost = true
			}
		}
	}
	if !testHost && isLocalhostName(host) {
		return blockedErr("host '%s' is blocked", host)
	}
	if len(d.Allowlist) > 0 && !allowlistMatch(host, d.Allowlist) {
		return blockedErr("host '%s' not in webhook domain allowlist", host)
	}
	if err := egress.CheckURLFor(d.AppEnv, d.AllowHosts, cleaned, true); err != nil {
		return err
	}
	if testHost {
		return nil
	}
	ip, ok := netip.Addr{}, false
	if a, err := netip.ParseAddr(host); err == nil {
		ip, ok = a, true
	} else if a, k := parseLooseIPv4(host); k {
		ip, ok = a, true
	}
	if ok && ipRefused(ip) {
		return blockedErr("ip '%s' is private/blocked", ip.WithZone(""))
	}
	return nil
}

// ---- sender ------------------------------------------------------------------

var allowedMethods = map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "HEAD": true, "OPTIONS": true}

func truncRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

func failed(msg string) Result { return Result{Status: "failed", Error: truncRunes(msg, errCap)} }

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return rowtmpl.PyStr(v)
}

func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return t != ""
	case bool:
		return t
	}
	return true
}

// Webhook sends one webhook action (see package doc). The URL is templated from
// the row, vetted, and dialled only to a vetted IP with Host and SNI preserved;
// redirects are never followed. A cancelled ctx aborts the request and answers
// failed (callers check ctx.Err()).
func Webhook(ctx context.Context, d WebhookDeps, ws string, cfg map[string]any, leadData map[string]any, cols []Column) Result {
	rawURL, _ := cfg["url"].(string)
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return failed("webhook url not configured")
	}
	// Policy first, before anything is resolved or dialled: a stored rule that names a
	// credential outside the outbound namespace must send nothing (fail closed).
	if ref, _ := cfg["header_secret_ref"].(string); ref != "" {
		if err := secrets.CheckOutboundRef(ref, d.Outbound); err != nil {
			return failed(secrets.OutboundRefMessage)
		}
	}
	resolved, err := resolveWebhookURL(rawURL, leadData, cols)
	if err != nil {
		return failed(err.Error())
	}
	cleaned := strings.NewReplacer("\t", "", "\r", "", "\n", "").Replace(strings.TrimSpace(resolved))
	if err := checkWebhookURL(d, cleaned); err != nil {
		return blockedResult(d, err, false)
	}
	u, _ := url.Parse(cleaned)

	method := "POST"
	if m := cfg["method"]; truthy(m) {
		method = strings.ToUpper(str(m))
	}
	if !allowedMethods[method] {
		return failed("webhook method not allowed")
	}

	hdr := http.Header{}
	var redact []string
	if h := cfg["headers"]; h != nil {
		keys, vals, ok := headerPairs(h)
		if !ok {
			return failed("webhook headers must be an object")
		}
		if len(keys) > maxHeaders {
			return failed("too many webhook headers")
		}
		for i, k := range keys {
			v, err := rowtmpl.Resolve(str(vals[i]), leadData, cols)
			if err != nil {
				return failed(err.Error())
			}
			if len(v) > maxHeaderValue {
				return failed("webhook header value too large")
			}
			if strings.EqualFold(k, "host") {
				continue
			}
			hdr.Set(k, v)
			if len(v) >= 4 {
				redact = append(redact, v)
			}
		}
	}
	if ref, _ := cfg["header_secret_ref"].(string); ref != "" {
		if d.Secrets == nil {
			return failed(secrets.OutboundRefMessage)
		}
		val, src, err := d.Secrets.Get(ctx, ws, ref)
		if err != nil {
			return failed("webhook secret lookup failed")
		}
		if val != "" && src == secrets.SourceAPI {
			// Fail closed and loud: dropping the header and reporting success
			// would let an unauthenticated receiver accept the call.
			return failed("webhook secret unavailable on this worker (requires CREDENTIALS_SOURCE=local)")
		}
		if val == "" || src != secrets.SourceWorkspace { // own workspace only, never global/env; missing fails closed (R2)
			return failed(secrets.OutboundRefMessage)
		}
		redact = append(redact, val)
		name := "Authorization"
		if n := cfg["header_name"]; truthy(n) {
			name = str(n)
		}
		if _, set := hdr[http.CanonicalHeaderKey(name)]; !set && !strings.EqualFold(name, "host") {
			hdr.Set(name, val)
		}
	}

	var body []byte
	if method == "POST" || method == "PUT" || method == "PATCH" {
		var ct string
		body, ct, err = buildBody(cfg["body"], leadData, cols, d.LiteralBody)
		if err != nil {
			return failed(err.Error())
		}
		if len(body) > MaxRequestBytes {
			return failed("webhook body too large")
		}
		if ct != "" {
			if _, set := hdr["Content-Type"]; !set {
				hdr.Set("Content-Type", ct)
			}
		}
	}

	timeout := d.Timeout
	if timeout <= 0 {
		timeout = DefaultWebhookTimeout
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var rb io.Reader
	if body != nil {
		rb = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(rctx, method, cleaned, rb)
	if err != nil {
		return failed("invalid webhook request")
	}
	req.Header = hdr

	resp, err := d.Client.Do(req)
	if err != nil {
		var be *egress.BlockedError
		if errors.As(err, &be) {
			return blockedResult(d, be, true)
		}
		return failed(sanitizeErr(err, u, redact, ctx))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes))
	if d.Log != nil {
		d.Log.Debug("webhook sent", "host", u.Hostname(), "method", method, "status", resp.StatusCode)
	}
	summary := method + " " + strconv.Itoa(resp.StatusCode)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return Result{Status: "success", Summary: summary}
	}
	text := strings.ToValidUTF8(string(raw), "\uFFFD")
	for _, r := range redact { // a hostile endpoint may echo the credential back
		text = strings.ReplaceAll(text, r, "[redacted]")
	}
	text = truncRunes(text, bodyPrefix)
	return Result{Status: "failed", Summary: summary, Error: "HTTP " + strconv.Itoa(resp.StatusCode) + ": " + text}
}

// privateAddress is the stored reason for a host that resolved (or was dialed) to a blocked
// address: the address itself is logged, never stored (final review L8).
const privateAddress = "host resolves to a private address"

// blockedResult is the blocked_url failure. dialTime marks a refusal from the request itself
// (resolve or dial), whose addresses come from DNS, not from the URL the caller wrote: those are
// replaced by a fixed text so a stored result is not a DNS / internal-address oracle.
func blockedResult(d WebhookDeps, err error, dialTime bool) Result {
	var be *egress.BlockedError
	reason := err.Error()
	if errors.As(err, &be) {
		reason = be.Reason
	}
	if d.Log != nil {
		d.Log.Warn("webhook blocked", "reason", truncRunes(reason, 60))
	}
	if strings.HasPrefix(reason, "host resolves to private ip ") || (dialTime && strings.HasPrefix(reason, "ip '")) {
		reason = privateAddress
	}
	return Result{Status: "failed", SkipReason: "blocked_url", Error: truncRunes("blocked url: "+reason, errCap)}
}

// dnsText is a resolver error without the resolver's own address ("lookup x on 10.0.0.2:53: ...").
func dnsText(err error) string {
	var de *net.DNSError
	if errors.As(err, &de) {
		c := *de
		c.Server = ""
		return c.Error()
	}
	return err.Error()
}

func headerPairs(h any) (keys []string, vals []any, ok bool) {
	switch t := h.(type) {
	case map[string]any:
		for k, v := range t {
			keys, vals = append(keys, k), append(vals, v)
		}
		return keys, vals, true
	case colgraph.Col:
		for _, k := range t.Keys {
			keys, vals = append(keys, k), append(vals, t.Vals[k])
		}
		return keys, vals, true
	}
	return nil, nil, false
}

// buildBody mirrors _act_webhook: nil -> the lead row as JSON; dict/list ->
// ResolveDeep; anything else -> str(), resolved, JSON if it parses else raw text.
func buildBody(raw any, lead map[string]any, cols []Column, literal bool) ([]byte, string, error) {
	const jsonCT = "application/json"
	if literal {
		switch t := raw.(type) {
		case nil:
			b, err := rowtmpl.MarshalOrdered(rowtmpl.CanonNumbers(lead))
			return b, jsonCT, err
		case map[string]any, colgraph.Col, []any:
			b, err := rowtmpl.MarshalOrdered(rowtmpl.CanonNumbers(t))
			return b, jsonCT, err
		}
		return []byte(str(raw)), "text/plain", nil
	}
	switch t := raw.(type) {
	case nil:
		b, err := rowtmpl.MarshalOrdered(rowtmpl.CanonNumbers(lead))
		return b, jsonCT, err
	case map[string]any, colgraph.Col, []any:
		v, err := rowtmpl.ResolveDeep(t, lead, cols)
		if err != nil {
			return nil, "", err
		}
		b, err := rowtmpl.MarshalOrdered(rowtmpl.CanonNumbers(v))
		return b, jsonCT, err
	}
	resolved, err := rowtmpl.Resolve(str(raw), lead, cols)
	if err != nil {
		return nil, "", err
	}
	if v, derr := rowtmpl.DecodeOrdered([]byte(resolved)); derr == nil {
		if v == nil { // json=None: httpx sends no body
			return nil, "", nil
		}
		b, err := rowtmpl.MarshalOrdered(rowtmpl.CanonNumbers(v))
		return b, jsonCT, err
	}
	return []byte(resolved), "text/plain", nil
}

var reURLQuery = regexp.MustCompile(`(://[^\s?]*)\?\S*`)

// sanitizeErr is str(e)[:200] with Python parity for timeout/refused, the URL
// query string, every header value and the secret removed.
func sanitizeErr(err error, u *url.URL, redact []string, parent context.Context) string {
	if parent.Err() != nil && !errors.Is(parent.Err(), context.DeadlineExceeded) {
		return "context canceled"
	}
	var nerr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded),
		errors.As(err, &nerr) && nerr.Timeout():
		return "timed out"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	// A dial error names the vetted IP it dialed: keep only the cause (final review L8).
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Err != nil {
		err = oe.Err
		var de *net.DNSError
		if errors.As(err, &de) {
			err = errors.New(dnsText(de))
		}
	}
	msg := err.Error()
	for _, r := range redact {
		msg = strings.ReplaceAll(msg, r, "[redacted]")
	}
	if u != nil && u.RawQuery != "" {
		msg = strings.ReplaceAll(msg, u.RawQuery, "[redacted]")
	}
	msg = reURLQuery.ReplaceAllString(msg, "$1?[redacted]")
	return truncRunes(msg, errCap)
}

func sprintf(format string, a ...any) string { return fmt.Sprintf(format, a...) }

// resolveWebhookURL templates the URL from the row without letting row data pick the
// destination: scheme and authority must be literal, and every substituted value in the
// path, query or fragment is percent-encoded. The resolved host must equal the literal one.
func resolveWebhookURL(raw string, leadData map[string]any, cols []Column) (string, error) {
	if !strings.Contains(raw, "{") {
		return rowtmpl.Resolve(raw, leadData, cols)
	}
	head, tail := raw, ""
	if i := strings.Index(raw, "://"); i >= 0 {
		if j := strings.IndexAny(raw[i+3:], "/?#"); j >= 0 {
			head, tail = raw[:i+3+j], raw[i+3+j:]
		}
	}
	if strings.Contains(head, "{") || !strings.Contains(head, "://") {
		return "", errors.New("webhook url scheme and host must be literal")
	}
	enc := make(map[string]any, len(leadData))
	for k, v := range leadData {
		if s, ok := v.(string); ok {
			enc[k] = strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
		} else {
			enc[k] = v
		}
	}
	rt, err := rowtmpl.Resolve(tail, enc, cols)
	if err != nil {
		return "", err
	}
	out := head + rt
	a, err1 := url.Parse(head)
	b, err2 := url.Parse(strings.NewReplacer("\t", "", "\r", "", "\n", "").Replace(out))
	if err1 != nil || err2 != nil || !strings.EqualFold(a.Host, b.Host) {
		return "", errors.New("webhook url host changed by template")
	}
	return out, nil
}
