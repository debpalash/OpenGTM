package enrich

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/egress"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/declarative"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/manifest"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/pycompat"
)

// DefaultDomainRPS is the per-registrable-domain request rate applied to v1
// connectors. The Python path applies none (it only bounds in-flight calls),
// and a v1 manifest has no limit field, so the plugin default of 1 request per
// second would turn a 1,000-row run into 17 minutes. 1,000/s keeps the shared
// limiter's bookkeeping without throttling; lower it with
// OPENGTM_ENRICH_DOMAIN_RPS when a vendor needs a ceiling.
const DefaultDomainRPS = 1000.0

// errTimeout marks a provider call that exceeded provider_timeout, the
// counterpart of asyncio.TimeoutError from the killable worker pool.
var errTimeout = errors.New("provider timeout")

// connectorSecrets resolves a connector's declared secrets from the process
// environment. Python also consults the settings database, which still lives
// in the legacy SQLite control plane (RFC milestone M8); the Python route only
// sends a run here when the two resolve identically (see connector_run.py).
type connectorSecrets struct{ p *manifest.Plugin }

func (s connectorSecrets) Secret(_ context.Context, name string) (string, error) {
	if !s.p.HasSecret(name) {
		return "", nil
	}
	return os.Getenv(name), nil
}

// providerCaller runs connectors through the shared egress client with the
// run's bounded provider concurrency and per-call deadline.
type providerCaller struct {
	client  *egress.Client
	sem     chan struct{}
	timeout time.Duration
	rps     float64
}

// call is one provider.enrich(lead): inputs from the row, a hard deadline, and
// the Python result dictionary. It returns errTimeout when the deadline passed,
// ctx's error when the run was cancelled, and any other error as an exception
// raised inside the provider (Python's `except Exception`).
func (c *providerCaller) call(ctx context.Context, name string, p *manifest.Plugin, lead *pycompat.Map) (response, *declarative.ProviderResult, error) {
	inputs, err := providerInputs(lead)
	if err != nil {
		return response{}, nil, err
	}
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return response{}, nil, ctx.Err()
	}
	cctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	plan := p
	if p.V1 != nil && c.rps > 0 {
		cp := *p
		cp.Limits.RequestsPerSecondPerDomain = c.rps
		plan = &cp
	}
	res, err := declarative.RunProvider(cctx, plan, plainInputs(inputs), connectorSecrets{p}, c.client)
	if err != nil {
		switch {
		case ctx.Err() != nil:
			return response{}, nil, ctx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			return response{}, nil, errTimeout
		}
		return response{}, nil, err
	}
	// provider_runner._provider_job: res.provider is the registered name and
	// the declared license defaults to "unknown".
	return response{
		Provider: name, Success: res.Success, Fields: res.Fields,
		Confidence: res.Confidence, Error: res.Error, License: "unknown",
	}, &res, nil
}

// plainInputs adapts the pycompat input values for RunProvider, which expects
// encoding/json-style values (it converts them back with FromPlain).
func plainInputs(in map[string]any) map[string]any { return in }

var rateLimitMarkers = []string{"429", "rate limit", "too many requests", "quota", "throttle"}

// looksRateLimited is planner.looks_rate_limited.
func looksRateLimited(msg string) bool {
	m := strings.ToLower(msg)
	for _, s := range rateLimitMarkers {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}
