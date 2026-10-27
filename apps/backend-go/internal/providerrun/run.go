// Package providerrun runs one enrichment provider call: a Go-native provider
// in a bounded, abandonable goroutine, or any other provider on the Python
// worker through pyworker. It resolves credentials per workspace, fills
// provider settings from global operator config only, reads and fills the
// workspace-scoped enrichment cache, and writes the provider_stats ledger the
// way Python's planner.record_attempt does.
package providerrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/enrichcache"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers/contact"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/pyworker"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/telemetry"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/vendorcatalog"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"

	// Providers register themselves in init().
	_ "github.com/debpalash/OpenGTM/apps/backend-go/internal/providers/firmo"
)

// Sentinels. ErrUnavailable is not a cell error: the worker hands the job back
// to the Python executor. ErrUnknownProvider and ErrUnauthorized come from the
// Python RPC (provider not on Python / bad token or disabled internal API).
var (
	ErrUnavailable     = pyworker.ErrUnavailable
	ErrUnknownProvider = pyworker.ErrUnknownProvider
	ErrUnauthorized    = pyworker.ErrUnauthorized
)

const (
	maxTimeout     = 120 * time.Second
	defaultTimeout = 10 * time.Second
	defaultWorkers = 16

	// Python planner.COOLDOWN_SECONDS / TIMEOUT_COOLDOWN_SECONDS.
	cooldownSeconds        = 300
	timeoutCooldownSeconds = 180

	ledgerTimeout = 5 * time.Second
)

// Secrets resolves one credential or setting (secrets.Resolver). ws == ""
// resolves global operator config only.
type Secrets interface {
	Get(ctx context.Context, ws, key string) (value, source string, err error)
}

// PyCaller is the Python worker RPC (pyworker.Client).
type PyCaller interface {
	Call(ctx context.Context, call pyworker.ProviderCall) (pyworker.ProviderResult, error)
}

// Deps wires a Runner.
type Deps struct {
	Pool    *pgxpool.Pool // nil disables the ledger and the cache (unit tests)
	HTTP    *http.Client  // egress-guarded client; nil builds egress.NewClient
	Secrets Secrets
	Py      PyCaller
	// OperatorAllow is EGRESS_ALLOW_HOSTS: exact host[:port] entries the
	// Reacher client may reach even when private.
	OperatorAllow []string
	Workers       int // provider_workers: goroutines running Go providers at once
	Log           *slog.Logger
	Cost          func(provider string) float64 // default vendorcatalog.BaseCost
	Now           func() time.Time
}

// Runner runs provider calls. Safe for concurrent use.
type Runner struct {
	d        Deps
	http     *http.Client
	operator *http.Client
	sem      chan struct{}
}

// New builds a Runner.
func New(d Deps) *Runner {
	if d.Workers <= 0 {
		d.Workers = defaultWorkers
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Cost == nil {
		d.Cost = vendorcatalog.BaseCost
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	r := &Runner{d: d, http: d.HTTP, sem: make(chan struct{}, d.Workers)}
	if r.http == nil {
		r.http = egress.NewClient(nil)
	}
	r.operator = r.http
	if len(d.OperatorAllow) > 0 {
		r.operator = egress.NewOperatorClient(d.OperatorAllow)
	}
	return r
}

// reacherErrorDetails is ERROR_DETAILS of the Python verifier: an unknown
// verdict that is an infrastructure failure, not an answer.
var reacherErrorDetails = map[string]bool{"error": true, "timeout": true, "breaker_open": true, "http_error": true, "unconfigured": true}

// providerTTL overrides the per-field cache TTL (tech_stack caches 90 days).
var providerTTL = map[string]time.Duration{"tech_stack": 90 * 24 * time.Hour}

// Run executes provider for one lead. Vendor failures are an Output with Error
// set (and a miss in the ledger). A returned error is infrastructure: the
// caller's context ended, a credential or setting lookup failed (never
// downgraded to "not configured"), or the Python path answered ErrUnavailable,
// ErrUnknownProvider or ErrUnauthorized. A call that outlives timeout (hard cap
// 120 s) reports Output{Error: "timeout"}.
func (r *Runner) Run(ctx context.Context, workspaceID, provider string, in providers.Input, timeout time.Duration) (providers.Output, error) {
	if workspaceID == "" || provider == "" {
		return providers.Output{}, errors.New("providerrun: workspace and provider are required")
	}
	if err := ctx.Err(); err != nil {
		return providers.Output{}, err
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	if timeout > maxTimeout {
		timeout = maxTimeout
	}
	p, native := providers.Get(provider)
	// A gated provider (providers/scrape) runs in Go only when GO_LOCAL_PROVIDERS lists it.
	native = native && providers.InProcess(p)

	if out, ok := r.cacheGet(ctx, workspaceID, provider, in); ok {
		return out, nil
	}

	start := time.Now()
	var (
		out         providers.Output
		rateLimited bool
		timedOut    bool
		err         error
	)
	if native {
		out, timedOut, err = r.runGo(ctx, workspaceID, p, in, timeout)
	} else {
		out, rateLimited, timedOut, err = r.runPython(ctx, workspaceID, provider, in, timeout)
	}
	if err != nil {
		return providers.Output{}, err
	}
	success := out.Error == "" && out.Extra != nil && out.Extra.Len() > 0
	r.record(ctx, workspaceID, provider, in.Field, success, out.Confidence,
		float64(time.Since(start))/float64(time.Millisecond), rateLimited, timedOut)
	if out.Error == "" {
		r.cachePut(ctx, workspaceID, provider, in, out)
	}
	return out, nil
}

// ---- Go path ----------------------------------------------------------------

type goResult struct {
	out providers.Output
	err error
}

func (r *Runner) runGo(ctx context.Context, ws string, p providers.Provider, in providers.Input, timeout time.Duration) (providers.Output, bool, error) {
	name := p.Name()
	in.Settings = map[string]any{}
	for _, k := range globalSettingKeys(name) {
		v, _, err := r.d.Secrets.Get(ctx, "", k)
		if err != nil {
			return providers.Output{}, false, fmt.Errorf("providerrun: setting %s: %w", k, err)
		}
		if v != "" {
			in.Settings[k] = v
		}
	}
	envs := []string(nil)
	if k, ok := p.(providers.Keyed); ok {
		envs = k.KeyEnv()
	}
	client := r.http
	if name == "reacher" {
		client = r.operator
	}

	cctx, cancel := context.WithTimeout(providers.WithWorkspace(ctx, ws), timeout)
	defer cancel()
	select {
	case r.sem <- struct{}{}:
	case <-cctx.Done():
		if ctx.Err() != nil {
			return providers.Output{}, false, ctx.Err()
		}
		return providers.Output{Error: "provider_busy"}, false, nil
	}

	key := func() (string, error) {
		if len(envs) == 0 {
			return "", nil
		}
		vals := make([]string, 0, len(envs))
		for _, e := range envs {
			v, _, err := r.d.Secrets.Get(cctx, ws, e)
			if err != nil {
				return "", fmt.Errorf("providerrun: credential lookup failed: %w", err)
			}
			vals = append(vals, v)
		}
		if len(vals) == 1 {
			return vals[0], nil
		}
		return providers.JoinKeys(vals...), nil
	}

	done := make(chan goResult, 1)
	var state atomic.Int32 // 0 running, 1 abandoned by the caller, 2 finished
	go func() {
		defer func() { <-r.sem }()
		var res goResult
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					res = goResult{out: providers.Output{Error: "provider crashed"}}
				}
			}()
			res.out, res.err = p.Enrich(cctx, client, key, in)
		}()
		if !state.CompareAndSwap(0, 2) {
			telemetry.ProviderCallReturned()
		}
		done <- res
	}()

	var res goResult
	select {
	case res = <-done:
	case <-cctx.Done():
		if state.CompareAndSwap(0, 1) {
			telemetry.ProviderCallAbandoned()
			if ctx.Err() != nil {
				return providers.Output{}, false, ctx.Err()
			}
			return providers.Output{Error: "timeout"}, true, nil
		}
		res = <-done // finished at the deadline: use its answer
	}
	if res.err != nil {
		if ctx.Err() != nil {
			return providers.Output{}, false, ctx.Err()
		}
		if errors.Is(cctx.Err(), context.DeadlineExceeded) && errors.Is(res.err, context.DeadlineExceeded) {
			return providers.Output{Error: "timeout"}, true, nil
		}
		return providers.Output{}, false, res.err
	}
	out := res.out
	if name == "reacher" && out.Error == "" && out.Extra != nil {
		if v, ok := out.Extra.Get("detail"); ok {
			if d, _ := v.(string); reacherErrorDetails[d] {
				out.Error = d
			}
		}
	}
	return out, false, nil
}

// ---- Python path ------------------------------------------------------------

func (r *Runner) runPython(ctx context.Context, ws, provider string, in providers.Input, timeout time.Duration) (out providers.Output, rateLimited, timedOut bool, err error) {
	if r.d.Py == nil {
		return providers.Output{}, false, false, ErrUnavailable
	}
	lead := in.Lead
	if lead == nil {
		lead = map[string]any{}
	}
	raw, merr := json.Marshal(lead)
	if merr != nil {
		return providers.Output{Error: "invalid provider call input"}, false, false, nil
	}
	// A fresh call_id per attempt: Python dedupes on (workspace, call_id) and a
	// retried paid call must never be replayed as the old one.
	res, cerr := r.d.Py.Call(ctx, pyworker.ProviderCall{
		CallID: newCallID(), Provider: provider, WorkspaceID: ws, Input: raw, Timeout: timeout,
	})
	if cerr != nil {
		return providers.Output{}, false, false, cerr
	}
	if res.Err != nil {
		return providers.Output{}, false, false, res.Err
	}
	switch res.Status {
	case "timeout":
		return providers.Output{Error: "timeout"}, false, true, nil
	case "ok", "error":
	default:
		return providers.Output{Error: "provider returned no result"}, false, false, nil
	}
	if o, ok := parsePythonOutput(res.Output, in.Field); ok {
		o.CostUSD = res.CostUSD
		if res.Status == "error" && o.Error == "" {
			o.Error = firstNonEmpty(res.Error, "provider returned no result")
		}
		return o, false, false, nil
	}
	msg := firstNonEmpty(res.Error, "provider returned no result")
	// No result dict: the Python provider raised, like Python's except branch.
	return providers.Output{Error: msg}, looksRateLimited(msg), false, nil
}

func parsePythonOutput(raw json.RawMessage, field string) (providers.Output, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return providers.Output{}, false
	}
	obj, err := wbcrud.ParseObject(raw)
	if err != nil || obj == nil {
		return providers.Output{}, false
	}
	var out providers.Output
	if f, ok := obj.Get("fields"); ok {
		if fo, ok := f.(*wbcrud.Obj); ok {
			out.Extra = fo
			if v, ok := fo.Get(field); ok {
				out.Value = v
			}
		}
	}
	if c, ok := obj.Get("confidence"); ok {
		out.Confidence = toFloat(c)
	}
	if e, ok := obj.Get("error"); ok {
		out.Error, _ = e.(string)
	}
	if s, ok := obj.Get("success"); ok {
		if b, _ := s.(bool); !b && out.Error == "" {
			out.Error = "provider returned no result"
		}
	}
	return out, true
}

func toFloat(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case json.Number:
		f, _ := t.Float64()
		return f
	}
	return 0
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// looksRateLimited ports planner.looks_rate_limited.
func looksRateLimited(err string) bool {
	e := strings.ToLower(err)
	for _, s := range []string{"429", "rate limit", "too many requests", "quota", "throttle"} {
		if strings.Contains(e, s) {
			return true
		}
	}
	return false
}

// ---- ledger -----------------------------------------------------------------

// record is the planner.record_attempt upsert in its own short transaction. It
// must outlive a cancelled call context, and a failure is logged and swallowed
// so telemetry can never fail a call.
func (r *Runner) record(ctx context.Context, ws, provider, field string, success bool, conf, latencyMS float64, rateLimited, timedOut bool) {
	if r.d.Pool == nil {
		return
	}
	cd := int32(0)
	if rateLimited {
		cd = cooldownSeconds
	} else if timedOut {
		cd = timeoutCooldownSeconds
	}
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ledgerTimeout)
	defer cancel()
	err := tenancy.InWorkspace(lctx, r.d.Pool, ws, func(tx pgx.Tx) error {
		return gen.New(tx).RecordProviderAttempt(lctx, gen.RecordProviderAttemptParams{
			Provider: provider, Field: field, Success: success, Confidence: conf,
			LatencyMs: latencyMS, CostUsd: r.d.Cost(provider), CooldownSeconds: cd,
		})
	})
	if err != nil {
		r.d.Log.Warn("provider stats write failed", "provider", provider, "err", err)
	}
}

// ---- cache ------------------------------------------------------------------

type cacheMode int

const (
	cacheNone cacheMode = iota
	cacheField
	cacheEmail
)

func leadStr(lead map[string]any, k string) string {
	s, _ := lead[k].(string)
	return s
}

func (r *Runner) mode(ctx context.Context, provider string, in providers.Input) (cacheMode, string, bool) {
	if r.d.Pool == nil || in.Operation != "" || in.Field == "" {
		return cacheNone, "", false
	}
	if provider == "reacher" {
		email := leadStr(in.Lead, "email")
		if email == "" {
			return cacheNone, "", false
		}
		// Engaged only when Reacher would actually run, as in Python's cascade.
		if in2, ok := r.reacherInput(ctx, in); ok && contact.ReacherAvailable(in2) {
			return cacheEmail, email, true
		}
		return cacheNone, "", false
	}
	key, ok := enrichcache.CanonicalKey(in.Field, enrichcache.Lead{
		Website: leadStr(in.Lead, "website"), Company: leadStr(in.Lead, "company"),
		ContactPerson: leadStr(in.Lead, "contact_person"),
	})
	if !ok {
		return cacheNone, "", false
	}
	return cacheField, key, true
}

// reacherInput resolves the operator Reacher settings for the availability
// probe; a lookup failure disables the cache (the call itself fails closed).
func (r *Runner) reacherInput(ctx context.Context, in providers.Input) (providers.Input, bool) {
	in.Settings = map[string]any{}
	for _, k := range globalSettingKeys("reacher") {
		v, _, err := r.d.Secrets.Get(ctx, "", k)
		if err != nil {
			return in, false
		}
		if v != "" {
			in.Settings[k] = v
		}
	}
	return in, true
}

func (r *Runner) cacheGet(ctx context.Context, ws, provider string, in providers.Input) (providers.Output, bool) {
	m, key, ok := r.mode(ctx, provider, in)
	if !ok {
		return providers.Output{}, false
	}
	var out providers.Output
	var hit bool
	err := tenancy.InWorkspace(ctx, r.d.Pool, ws, func(tx pgx.Tx) error {
		switch m {
		case cacheEmail:
			raw, found, err := enrichcache.GetEmail(ctx, tx, ws, key, "reacher", r.d.Now())
			if err != nil || !found {
				return err
			}
			var v struct {
				Status     string  `json:"status"`
				Source     string  `json:"source"`
				Detail     string  `json:"detail"`
				Confidence float64 `json:"confidence"`
			}
			if json.Unmarshal(raw, &v) != nil || v.Status == "" {
				return nil
			}
			x := providers.NewObj()
			x.Set("email", key)
			x.Set("status", v.Status)
			x.Set("source", v.Source)
			x.Set("confidence", v.Confidence)
			x.Set("detail", v.Detail)
			out, hit = providers.Output{Value: v.Status, Confidence: v.Confidence, Extra: x}, true
		case cacheField:
			h, found, err := enrichcache.Get(ctx, tx, ws, key, in.Field, r.d.Now())
			if err != nil || !found {
				return err
			}
			x := providers.NewObj()
			x.Set(in.Field, h.Value)
			out, hit = providers.Output{Value: h.Value, Confidence: h.Confidence, Extra: x}, true
		}
		return nil
	})
	if err != nil {
		if ctx.Err() == nil {
			r.d.Log.Warn("provider cache read failed", "provider", provider, "err", err)
		}
		return providers.Output{}, false
	}
	return out, hit
}

func (r *Runner) cachePut(ctx context.Context, ws, provider string, in providers.Input, out providers.Output) {
	m, key, ok := r.mode(ctx, provider, in)
	if !ok {
		return
	}
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ledgerTimeout)
	defer cancel()
	err := tenancy.InWorkspace(lctx, r.d.Pool, ws, func(tx pgx.Tx) error {
		switch m {
		case cacheEmail:
			status, _ := out.Value.(string)
			if status != "valid" && status != "invalid" && status != "catch_all" {
				return nil
			}
			detail := ""
			if out.Extra != nil {
				if v, ok := out.Extra.Get("detail"); ok {
					detail, _ = v.(string)
				}
			}
			body, err := json.Marshal(map[string]any{"status": status, "source": "reacher", "detail": detail, "confidence": out.Confidence})
			if err != nil {
				return err
			}
			return enrichcache.PutEmail(lctx, tx, ws, key, "reacher", body, enrichcache.EmailTTL(status))
		case cacheField:
			v, _ := out.Value.(string)
			if v == "" || v == "N/A" {
				return nil
			}
			ttl := enrichcache.TTLFor(in.Field)
			if t, ok := providerTTL[provider]; ok {
				ttl = t
			}
			return enrichcache.Put(lctx, tx, ws, key, in.Field, enrichcache.Hit{Value: v, Confidence: out.Confidence, Provider: provider}, ttl)
		}
		return nil
	})
	if err != nil {
		r.d.Log.Warn("provider cache write failed", "provider", provider, "err", err)
	}
}

// Configured reports whether a Go-native keyed provider has every credential it
// needs in this workspace, so a paid call can be refused before any spend hold
// (Python never reserved for a missing key). Providers that are not Go-native
// or need no key are always configured; a lookup failure is returned, never
// downgraded to "configured".
func (r *Runner) Configured(ctx context.Context, workspaceID, provider string) (bool, error) {
	p, native := providers.Get(provider)
	if !native || !providers.InProcess(p) {
		return true, nil
	}
	k, ok := p.(providers.Keyed)
	if !ok || r.d.Secrets == nil {
		return true, nil
	}
	for _, e := range k.KeyEnv() {
		v, _, err := r.d.Secrets.Get(ctx, workspaceID, e)
		if err != nil {
			return false, fmt.Errorf("providerrun: credential lookup failed: %w", err)
		}
		if v == "" {
			return false, nil
		}
	}
	return true, nil
}
