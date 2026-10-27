// Package rpclocal is the in-process half of POST /internal/provider-call
// (plan 15 WP-PROVIDERS, inventory A1): a caller with the pyworker.Client
// Call signature that serves the names GO_LOCAL_PROVIDERS lists in Go (the
// named RPCs of providers/scrape, then any registered Go provider) and sends
// every other call to the Python worker.
//
// It keeps the Python endpoint's semantics: the timeout clamp (default 10 s,
// at most 120 s), status ok / error / timeout, `output` as the provider
// result dict, cost only for a successful paid provider, (workspace,
// call_id) dedupe of successful calls, and scrubbing of secret values from
// everything returned.
package rpclocal

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providerrun"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers/scrape"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/pyworker"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/telemetry"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/vendorcatalog"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"

	// The ported providers register themselves in init().
	_ "github.com/debpalash/OpenGTM/apps/backend-go/internal/providers/contact"
	_ "github.com/debpalash/OpenGTM/apps/backend-go/internal/providers/firmo"
)

const (
	maxTimeout     = 120 * time.Second
	defaultTimeout = 10 * time.Second
	dedupeTTL      = 600 * time.Second
	dedupeMax      = 2048
	minSecretLen   = 6
	maxIDLen       = 200
)

// PyCaller is the Python worker RPC (pyworker.Client).
type PyCaller interface {
	Call(ctx context.Context, call pyworker.ProviderCall) (pyworker.ProviderResult, error)
}

// Secrets resolves one credential (secrets.Resolver); ws "" is global only.
type Secrets interface {
	Get(ctx context.Context, ws, key string) (value, source string, err error)
}

// Runner runs one registered Go provider the way every other Go caller does
// (providerrun.Runner: operator settings, per-workspace keys, cache, stats
// ledger, worker semaphore).
type Runner interface {
	RunLocal(ctx context.Context, workspaceID, provider string, in providers.Input, timeout time.Duration) (providers.Output, error)
}

// Deps wire a Caller.
type Deps struct {
	// Runner runs registered Go providers; nil builds a providerrun.Runner without a pool
	// (settings and keys, no cache or stats ledger).
	Runner  Runner
	Py      PyCaller          // nil: names not served here fail with pyworker.ErrUnavailable
	Local   func(string) bool // the GO_LOCAL_PROVIDERS gate; nil serves nothing here
	HTTP    *http.Client      // strict egress client for lead-supplied URLs; nil builds egress.NewClient
	Secrets Secrets           // keyed Go providers; nil resolves no key
	// Scrub lists extra secret values to remove from outputs (optional).
	Scrub func(ctx context.Context, ws string) []string
	Cost  func(provider string) float64 // default vendorcatalog.BaseCost
	Log   *slog.Logger
	Now   func() time.Time
}

// Caller routes provider calls. Safe for concurrent use.
type Caller struct {
	d     Deps
	mu    sync.Mutex
	calls map[dedupeKey]*flight
	order []dedupeKey
}

type dedupeKey struct{ ws, id string }

type flight struct {
	done chan struct{}
	at   time.Time
	res  pyworker.ProviderResult
	err  error
}

// New builds a Caller.
func New(d Deps) *Caller {
	if d.HTTP == nil {
		d.HTTP = egress.NewClient(nil)
	}
	if d.Cost == nil {
		d.Cost = vendorcatalog.BaseCost
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Runner == nil {
		var sec providerrun.Secrets = noSecrets{}
		if d.Secrets != nil {
			sec = d.Secrets
		}
		d.Runner = providerrun.New(providerrun.Deps{HTTP: d.HTTP, Secrets: sec, Log: d.Log, Cost: d.Cost})
	}
	return &Caller{d: d, calls: map[dedupeKey]*flight{}}
}

// Serves reports whether name is answered in process.
func (c *Caller) Serves(name string) bool {
	if c.d.Local == nil || !c.d.Local(name) {
		return false
	}
	if _, ok := scrape.RPCs()[name]; ok {
		return true
	}
	// Reacher needs the operator client and settings: it stays on its own paths.
	_, ok := providers.Get(name)
	return ok && name != "reacher"
}

// Call runs call in process when served here, else on the Python worker.
func (c *Caller) Call(ctx context.Context, call pyworker.ProviderCall) (pyworker.ProviderResult, error) {
	if !c.Serves(call.Provider) {
		if c.d.Py == nil {
			return pyworker.ProviderResult{}, pyworker.ErrUnavailable
		}
		return c.d.Py.Call(ctx, call)
	}
	if call.CallID == "" || call.WorkspaceID == "" || len(call.CallID) > maxIDLen || len(call.WorkspaceID) > maxIDLen {
		return pyworker.ProviderResult{Status: "error", Error: "invalid provider call", HTTPStatus: http.StatusUnprocessableEntity}, nil
	}
	f, created := c.claim(dedupeKey{call.WorkspaceID, call.CallID})
	if !created {
		select {
		case <-f.done:
			return f.res, f.err
		case <-ctx.Done():
			return pyworker.ProviderResult{}, ctx.Err()
		}
	}
	f.res, f.err = c.execute(ctx, call)
	if f.err != nil || f.res.Status != "ok" {
		c.forget(dedupeKey{call.WorkspaceID, call.CallID}, f) // a retry must re-run
	}
	close(f.done)
	return f.res, f.err
}

func (c *Caller) claim(k dedupeKey) (*flight, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.d.Now()
	for len(c.order) > 0 {
		old := c.order[0]
		f, ok := c.calls[old]
		if ok && now.Sub(f.at) <= dedupeTTL && len(c.calls) <= dedupeMax {
			break
		}
		c.order = c.order[1:]
		if ok {
			delete(c.calls, old)
		}
	}
	if f, ok := c.calls[k]; ok {
		return f, false
	}
	f := &flight{done: make(chan struct{}), at: now}
	c.calls[k] = f
	c.order = append(c.order, k)
	return f, true
}

func (c *Caller) forget(k dedupeKey, f *flight) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.calls[k] == f {
		delete(c.calls, k)
	}
}

func effectiveTimeout(t time.Duration) time.Duration {
	if t <= 0 {
		return defaultTimeout
	}
	return min(t, maxTimeout)
}

type outcome struct {
	out      *providers.Obj
	err      error
	timedOut bool // providerrun reported its own timeout
}

// noSecrets resolves nothing (no resolver configured).
type noSecrets struct{}

func (noSecrets) Get(context.Context, string, string) (string, string, error) { return "", "", nil }

// execute is _execute: one run under the clamped timeout.
func (c *Caller) execute(ctx context.Context, call pyworker.ProviderCall) (pyworker.ProviderResult, error) {
	input := call.Input
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}
	var lead map[string]any
	dec := json.NewDecoder(strings.NewReader(string(input)))
	dec.UseNumber()
	if err := dec.Decode(&lead); err != nil || lead == nil {
		return pyworker.ProviderResult{Status: "error", Error: "invalid provider call", HTTPStatus: http.StatusUnprocessableEntity}, nil
	}
	settings := callOptions(lead)
	var smu sync.Mutex
	var secrets []string
	addSecret := func(v string) {
		if len(v) >= minSecretLen {
			smu.Lock()
			secrets = append(secrets, v)
			smu.Unlock()
		}
	}
	cctx, cancel := context.WithTimeout(providers.WithWorkspace(ctx, call.WorkspaceID), effectiveTimeout(call.Timeout))
	defer cancel()
	done := make(chan outcome, 1)
	rpc, isRPC := scrape.RPCs()[call.Provider]
	go func() {
		var o outcome
		defer func() {
			if rec := recover(); rec != nil {
				o = outcome{err: fmt.Errorf("provider crashed")}
			}
			done <- o
		}()
		if isRPC {
			o.out, o.err = rpc(cctx, c.d.HTTP, input)
			return
		}
		// Registered Go providers run through providerrun, exactly like every other Go
		// caller: global operator settings, per-workspace keys, the enrichment cache, the
		// provider_stats ledger and the worker semaphore.
		p, _ := providers.Get(call.Provider)
		out, err := c.d.Runner.RunLocal(cctx, call.WorkspaceID, call.Provider, providers.Input{Lead: lead, Settings: settings}, effectiveTimeout(call.Timeout))
		if k, ok := p.(providers.Keyed); ok && c.d.Secrets != nil {
			// The key values providerrun resolved are scrubbed from everything returned.
			for _, env := range k.KeyEnv() {
				if v, _, kerr := c.d.Secrets.Get(context.WithoutCancel(cctx), call.WorkspaceID, env); kerr == nil {
					addSecret(v)
				}
			}
		}
		if err != nil {
			o.err = err
			return
		}
		if out.Error == "timeout" {
			o.timedOut = true
		}
		o.out = scrape.OutputDict(scrape.ResultProviderOf(p, out), out)
	}()
	var o outcome
	select {
	case o = <-done:
	case <-cctx.Done():
		telemetry.ProviderCallAbandoned()
		if ctx.Err() != nil {
			return pyworker.ProviderResult{}, ctx.Err()
		}
		return pyworker.ProviderResult{Status: "timeout", Error: "provider timed out"}, nil
	}
	if c.d.Scrub != nil {
		for _, s := range c.d.Scrub(ctx, call.WorkspaceID) {
			addSecret(s)
		}
	}
	if o.timedOut {
		return pyworker.ProviderResult{Status: "timeout", Error: "provider timed out"}, nil
	}
	if o.err != nil {
		if ctx.Err() != nil {
			return pyworker.ProviderResult{}, ctx.Err()
		}
		if cctx.Err() != nil {
			return pyworker.ProviderResult{Status: "timeout", Error: "provider timed out"}, nil
		}
		return pyworker.ProviderResult{Status: "error", Error: scrubString(o.err.Error(), secrets)}, nil
	}
	out := scrubObj(o.out, secrets)
	raw, err := out.MarshalJSON()
	if err != nil {
		return pyworker.ProviderResult{Status: "error", Error: "provider returned no result"}, nil
	}
	success, _ := out.Get("success")
	if ok, _ := success.(bool); ok {
		cost := 0.0
		if !isRPC && vendorcatalog.IsPaid(call.Provider) {
			cost = c.d.Cost(call.Provider)
		}
		return pyworker.ProviderResult{Status: "ok", Output: raw, CostUSD: cost}, nil
	}
	msg := "provider returned no result"
	if e, _ := out.Get("error"); e != nil {
		if s, _ := e.(string); s != "" {
			msg = s
		}
	}
	return pyworker.ProviderResult{Status: "error", Output: raw, Error: msg}, nil
}

func scrubString(s string, secrets []string) string {
	for _, sec := range secrets {
		s = strings.ReplaceAll(s, sec, "[redacted]")
	}
	return s
}

// scrubObj is _scrub: string values (not keys) anywhere in the output.
func scrubObj(o *wbcrud.Obj, secrets []string) *wbcrud.Obj {
	if len(secrets) == 0 || o == nil {
		return o
	}
	n := wbcrud.NewObj()
	for _, k := range o.Keys() {
		v, _ := o.Get(k)
		n.Set(k, scrubValue(v, secrets))
	}
	return n
}

func scrubValue(v any, secrets []string) any {
	switch t := v.(type) {
	case string:
		return scrubString(t, secrets)
	case *wbcrud.Obj:
		return scrubObj(t, secrets)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = scrubValue(e, secrets)
		}
		return out
	case []string:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = scrubString(e, secrets)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = scrubValue(e, secrets)
		}
		return out
	}
	return v
}

// callOptionKeys are the per-call tuning options a caller may put in the input's "_options"
// object (the collect stages' provider settings); providers clamp them.
var callOptionKeys = map[string]bool{"max_people": true, "max_jobs": true, "max_verify": true, "delay_ms": true, "timeout_ms": true}

// callOptions removes the reserved "_options" object from lead and returns its integer options.
func callOptions(lead map[string]any) map[string]any {
	raw, ok := lead["_options"]
	if !ok {
		return nil
	}
	delete(lead, "_options")
	m, _ := raw.(map[string]any)
	out := map[string]any{}
	for k, v := range m {
		if !callOptionKeys[k] {
			continue
		}
		if n, ok := v.(json.Number); ok {
			if i, err := n.Int64(); err == nil && i >= 0 && i <= 1<<20 {
				out[k] = int(i)
			}
		}
	}
	return out
}
