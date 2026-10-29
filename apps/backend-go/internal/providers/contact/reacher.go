package contact

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// reacher ports providers/reacher_verify.py (ReacherVerifier): the strongest
// tier of the email-verify cascade. It is not an EnrichmentProvider in Python;
// here Enrich(lead.email) returns
//
//	Value      = status ("valid" | "invalid" | "catch_all" | "unknown")
//	Confidence = the status confidence (0.95 / 0.9 / 0.5 / 0)
//	Extra      = {email, status, source, confidence, detail, is_available}
//
// Output.Error is always empty: verify never fails, any error becomes an
// unknown status that also feeds the circuit breaker.
//
// Settings: REACHER_URL (global, never per-workspace), REACHER_ENABLED,
// REACHER_TIMEOUT. The optional REACHER_API_KEY comes through the key callback
// and is only fetched once the URL is valid and the breaker is closed.
type reacher struct{ br *breaker }

func newReacher() *reacher {
	return &reacher{br: &breaker{threshold: 3, cooldown: 60 * time.Second, now: time.Now}}
}

func (*reacher) Name() string     { return "reacher" }
func (*reacher) KeyEnv() []string { return []string{"REACHER_API_KEY"} }

// breaker is the process-global circuit breaker: after threshold consecutive
// failures it reports open for cooldown (Python defaults 3 and 60s).
type breaker struct {
	mu        sync.Mutex
	threshold int
	cooldown  time.Duration
	now       func() time.Time
	failures  int
	openUntil time.Time
}

func (b *breaker) isOpen() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.openUntil.IsZero() && b.now().Before(b.openUntil)
}

func (b *breaker) success() {
	b.mu.Lock()
	b.failures, b.openUntil = 0, time.Time{}
	b.mu.Unlock()
}

func (b *breaker) failure() {
	b.mu.Lock()
	b.failures++
	if b.failures >= b.threshold {
		b.openUntil = b.now().Add(b.cooldown)
	}
	b.mu.Unlock()
}

func validateReacherURL(u string) bool {
	if u == "" {
		return false
	}
	p, err := url.Parse(u)
	if err != nil || (p.Scheme != "http" && p.Scheme != "https") {
		return false
	}
	host := strings.ToLower(p.Hostname())
	if host == "" || host == "169.254.169.254" || host == "metadata.google.internal" || host == "metadata" ||
		strings.HasPrefix(host, "169.254.") {
		return false
	}
	return true
}

func setting(in providers.Input, k string) string {
	v, ok := in.Settings[k]
	if !ok || v == nil {
		return ""
	}
	if s, isStr := v.(string); isStr {
		return providers.PyStrip(s)
	}
	return providers.PyStrip(providers.Str(v))
}

func truthyFlag(s string) bool {
	switch strings.ToLower(providers.PyStrip(s)) {
	case "", "0", "false", "no", "off":
		return false
	}
	return true
}

var statusConfidence = map[string]float64{"valid": 0.95, "invalid": 0.9, "catch_all": 0.5, "unknown": 0}

func (r *reacher) available(in providers.Input) bool {
	if !truthyFlag(setting(in, "REACHER_ENABLED")) {
		return false
	}
	if !validateReacherURL(setting(in, "REACHER_URL")) {
		return false
	}
	return !r.br.isOpen()
}

func (r *reacher) Enrich(ctx context.Context, c *http.Client, key func() (string, error), in providers.Input) (providers.Output, error) {
	email := lead(in, "email")
	status, detail, err := r.verify(ctx, c, key, in, email)
	if err != nil {
		return providers.Output{}, err
	}
	f := newFields()
	f.Set("email", email)
	f.Set("status", status)
	f.Set("source", "reacher")
	f.Set("confidence", statusConfidence[status])
	f.Set("detail", detail)
	f.Set("is_available", r.available(in))
	return providers.Output{Value: status, Confidence: statusConfidence[status], Extra: f}, nil
}

// verify never fails except for a cancelled context or a key-lookup failure.
func (r *reacher) verify(ctx context.Context, c *http.Client, key func() (string, error), in providers.Input, email string) (status, detail string, err error) {
	if r.br.isOpen() {
		return "unknown", "breaker_open", nil
	}
	base := setting(in, "REACHER_URL")
	if !validateReacherURL(base) {
		return "unknown", "unconfigured", nil
	}
	apiKey, err := key()
	if err != nil {
		return "", "", err
	}
	apiKey = providers.PyStrip(apiKey)
	hdr := [][2]string{{"Content-Type", "application/json"}}
	if apiKey != "" {
		hdr = append(hdr, [2]string{"Authorization", "Bearer " + apiKey})
	}
	timeout := 12 * time.Second
	if t := setting(in, "REACHER_TIMEOUT"); t != "" {
		if f, perr := strconv.ParseFloat(t, 64); perr == nil && f > 0 {
			timeout = time.Duration(f * float64(time.Second))
		}
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resp, derr := providers.Do(cctx, c, providers.Req{
		Method: http.MethodPost, URL: strings.TrimRight(base, "/") + "/v0/check_email",
		Header: hdr, Body: marshal(jo{{"to_email", email}}),
	}, apiKey)
	var payload any
	if derr == nil && (resp.Status < 200 || resp.Status > 299) { // raise_for_status
		derr = &providers.PyException{Msg: "status " + strconv.Itoa(resp.Status)}
	}
	if derr == nil {
		payload, derr = providers.ParseJSON(resp.Body)
	}
	if derr == nil {
		if _, isMap := payload.(map[string]any); !isMap {
			derr = &providers.PyException{Msg: "unexpected payload"}
		}
	}
	if derr != nil {
		if cerr := ctx.Err(); cerr != nil {
			return "", "", cerr
		}
		r.br.failure()
		if providers.IsTimeout(derr) {
			return "unknown", "timeout", nil
		}
		return "unknown", "error", nil
	}
	r.br.success()
	status, detail = mapReacher(payload.(map[string]any))
	return status, detail, nil
}

// mapReacher is map_reacher_status: safe->valid, invalid->invalid, risky->
// catch_all when the domain is catch-all (else unknown), disposable->invalid.
func mapReacher(p map[string]any) (status, detail string) {
	reach := ""
	if s, ok := p["is_reachable"].(string); ok {
		reach = strings.ToLower(s)
	}
	sub := func(k string) map[string]any {
		m, _ := p[k].(map[string]any)
		return m
	}
	catchAll := providers.Truthy(sub("smtp")["is_catch_all"])
	misc := sub("misc")
	if providers.Truthy(misc["is_disposable"]) {
		return "invalid", "disposable"
	}
	var bits []string
	if providers.Truthy(misc["is_role_account"]) {
		bits = append(bits, "role")
	}
	if catchAll {
		bits = append(bits, "catch_all")
	}
	switch reach {
	case "safe":
		status = "valid"
	case "invalid":
		status = "invalid"
	case "risky":
		status = "unknown"
		if catchAll {
			status = "catch_all"
		}
	default:
		status = "unknown"
	}
	switch {
	case len(bits) > 0:
		detail = strings.Join(bits, ",")
	case reach != "":
		detail = reach
	default:
		detail = "unknown"
	}
	return status, detail
}
