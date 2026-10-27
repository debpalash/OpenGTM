package workbook

import (
	"context"
	"errors"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/provcall"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// ProviderRunner runs one provider call (providerrun.Runner). The runner owns
// the provider_stats ledger: the worker never writes provider attempts of its
// own for calls that go through it. Vendor failures are an Output with Error
// set ("timeout" for a deadline); a returned error is infrastructure.
type ProviderRunner interface {
	Run(ctx context.Context, workspaceID, provider string, in providers.Input, timeout time.Duration) (providers.Output, error)
}

// configurer is implemented by runners that can tell, without any network
// call, whether a provider's credentials exist (providerrun.Runner).
type configurer interface {
	Configured(ctx context.Context, workspaceID, provider string) (bool, error)
}

// manifestRunner is the runner used when Deps.Runner is nil: it executes the
// signature-verified declarative manifests of the payload (the original
// executor behaviour) and writes the ledger rows itself, in the same format.
type manifestRunner struct{ rs *runState }

func (r manifestRunner) manifest(name string) (manifest.Manifest, bool) {
	m, ok := r.rs.p.Manifests[name]
	return m, ok
}

func (r manifestRunner) Run(ctx context.Context, _ string, provider string, in providers.Input, timeout time.Duration) (providers.Output, error) {
	rs := r.rs
	m, ok := r.manifest(provider)
	if !ok {
		return providers.Output{}, provcall.ErrUnknownProvider
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	res := manifest.ExecuteChecked(pctx, rs.deps.Client, m, leadStrings(in.Lead), rs.credFunc(pctx), rs.deps.URLCheck)
	if ctx.Err() != nil {
		return providers.Output{}, context.Cause(ctx)
	}
	timedOut := errors.Is(pctx.Err(), context.DeadlineExceeded)
	out := providers.Output{Confidence: res.Confidence, Error: res.Error}
	if timedOut {
		out = providers.Output{Error: "timeout"}
	} else if len(res.Fields) > 0 {
		out.Extra = providers.NewObj()
		for k, v := range res.Fields {
			out.Extra.Set(k, v)
		}
		out.Value = res.Fields[in.Field]
	}
	rec := attemptRecord{provider: provider, field: in.Field, latencyMS: float64(time.Since(start)) / float64(time.Millisecond), timedOut: timedOut}
	if !timedOut {
		rec.success = res.Success && len(res.Fields) > 0
		rec.confidence = res.Confidence
		if rec.confidence == 0 {
			rec.confidence = m.DefaultConfidence
		}
	}
	rs.pl.recordAttempts(ctx, rs.deps.Pool, rs.p.WorkspaceID, []attemptRecord{rec}, rs.log)
	return out, nil
}

// precheck runs manifest.Prepare (render, credential, URL guard; no network to
// the provider) and returns its refusal text, or "".
func (r manifestRunner) precheck(ctx context.Context, name string, lead map[string]any) string {
	m, ok := r.manifest(name)
	if !ok {
		return ""
	}
	rs := r.rs
	pctx, cancel := context.WithTimeout(ctx, time.Duration(rs.p.ProviderTimeout*float64(time.Second)))
	defer cancel()
	_, perr := manifest.Prepare(m, leadStrings(lead), rs.credFunc(pctx), rs.deps.URLCheck)
	return perr
}

// precheck refuses a call the runner can already tell will not reach the network.
func (rs *runState) precheck(ctx context.Context, name string, lead map[string]any) string {
	if pc, ok := rs.runner.(prechecker); ok {
		return pc.precheck(ctx, name, lead)
	}
	if cr, ok := rs.runner.(configurer); ok {
		okc, err := cr.Configured(ctx, rs.p.WorkspaceID, name)
		if err != nil {
			return "credential lookup failed" // fail closed: no hold, no call
		}
		if !okc {
			return "not configured"
		}
	}
	return ""
}
