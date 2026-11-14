package providerrun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/credentials"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// processConnectorIndex is the signature-verified connector index of this process (built-in
// embedded manifests, or OPENGTM_CONNECTOR_MANIFESTS_DIR / _TRUST_STORE under
// CONNECTOR_SIGNATURE_POLICY), shared by every Runner built without Deps.Manifests.
var processConnectorIndex = credentials.ConnectorIndex(os.LookupEnv, nil)

// declarative returns the verified manifest serving provider when no Go provider does.
func (r *Runner) declarative(provider string) (manifest.Manifest, bool) {
	if r.d.Manifests == nil {
		return manifest.Manifest{}, false
	}
	e, ok := r.d.Manifests()[provider]
	if !ok || e.Manifest.Name != provider {
		return manifest.Manifest{}, false
	}
	return e.Manifest, true
}

// compliance reports a declarative provider of the compliance capability (a per-recipient legal
// verdict with a receipt), which the provider cache must never serve.
func (r *Runner) compliance(provider string) bool {
	if _, native := providers.Get(provider); native {
		return false
	}
	m, ok := r.declarative(provider)
	if !ok {
		return false
	}
	if m.Capability == "compliance" {
		return true
	}
	for _, c := range m.Capabilities {
		if c == "compliance" {
			return true
		}
	}
	return false
}

// nativeFor is the Go provider Run would execute for provider (a gated one only when served in process).
func nativeFor(provider string, local bool) (providers.Provider, bool) {
	p, ok := providers.Get(provider)
	return p, ok && (local || providers.InProcess(p))
}

// Serves reports whether Run executes provider in this process: a Go provider, or else a
// signature-verified declarative manifest. Anything else is ErrUnavailable, before any I/O.
func (r *Runner) Serves(provider string) bool {
	if p, ok := providers.Get(provider); ok {
		return providers.InProcess(p) // a Go provider name never falls through to a manifest
	}
	_, ok := r.declarative(provider)
	return ok
}

// ManifestEnv lists the credential names a declarative provider may read (to scrub their values
// from what a caller returns); nil for anything else.
func (r *Runner) ManifestEnv(provider string) []string {
	if _, ok := providers.Get(provider); ok {
		return nil
	}
	m, ok := r.declarative(provider)
	if !ok {
		return nil
	}
	var out []string
	for n := range manifest.AllowedEnvNames(m) {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// manifestCreds resolves every credential the manifest may read, in the calling workspace (the
// resolver's workspace -> operator -> environment order, as for Go providers). A lookup failure
// is returned, never downgraded to a missing key, and happens before any network I/O.
func (r *Runner) manifestCreds(ctx context.Context, ws string, m manifest.Manifest) (func(string) (string, error), []string, error) {
	vals := map[string]string{}
	var secrets []string
	for n := range manifest.AllowedEnvNames(m) {
		v, _, err := r.d.Secrets.Get(ctx, ws, n)
		if err != nil {
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			return nil, nil, fmt.Errorf("providerrun: credential lookup failed: %w", err)
		}
		vals[n] = v
		if v != "" {
			secrets = append(secrets, v)
		}
	}
	return func(n string) (string, error) { return vals[n], nil }, secrets, nil
}

// Precheck refuses a call that cannot reach the network, without any network I/O: for a manifest,
// manifest.Prepare (security policy, rendering, credential, URL guard); for a keyed Go provider, a
// missing key ("not configured"). "" means the call may go out. An error is a credential lookup
// failure (the caller fails closed).
func (r *Runner) Precheck(ctx context.Context, workspaceID, provider string, lead map[string]any) (string, error) {
	if _, native := providers.Get(provider); !native {
		if m, ok := r.declarative(provider); ok {
			cred, _, err := r.manifestCreds(ctx, workspaceID, m)
			if err != nil {
				return "", err
			}
			_, refusal := manifest.Prepare(m, manifest.LeadFromRow(lead), cred, r.d.URLCheck)
			return refusal, nil
		}
	}
	ok, err := r.Configured(ctx, workspaceID, provider)
	if err != nil {
		return "", err
	}
	if !ok {
		return "not configured", nil
	}
	return "", nil
}

// manifestConfigured is Configured for a declarative provider: its auth credential is set.
func (r *Runner) manifestConfigured(ctx context.Context, ws string, m manifest.Manifest) (bool, error) {
	if m.Auth.Type == "" || m.Auth.Type == "none" || m.Auth.EnvVar == "" || r.d.Secrets == nil {
		return true, nil
	}
	v, _, err := r.d.Secrets.Get(ctx, ws, m.Auth.EnvVar)
	if err != nil {
		return false, fmt.Errorf("providerrun: credential lookup failed: %w", err)
	}
	return v != "", nil
}

// runManifest executes a declarative provider (manifest.ExecuteChecked: the signature-verified
// manifest, workspace credentials, the URL guard and the egress-guarded client) under the worker
// semaphore and the caller's timeout. Like runGo, a vendor failure is Output.Error and a returned
// error is infrastructure.
func (r *Runner) runManifest(ctx context.Context, ws string, m manifest.Manifest, in providers.Input, timeout time.Duration) (providers.Output, bool, error) {
	cctx, cancel := context.WithTimeout(providers.WithWorkspace(ctx, ws), timeout)
	defer cancel()
	cred, _, err := r.manifestCreds(cctx, ws, m)
	if err != nil {
		if ctx.Err() != nil {
			return providers.Output{}, false, ctx.Err()
		}
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			return providers.Output{Error: "timeout"}, true, nil
		}
		return providers.Output{}, false, err
	}
	select {
	case r.sem <- struct{}{}:
	case <-cctx.Done():
		if ctx.Err() != nil {
			return providers.Output{}, false, ctx.Err()
		}
		return providers.Output{Error: "provider_busy"}, false, nil
	}
	defer func() { <-r.sem }()
	res := manifest.ExecuteChecked(cctx, r.manifestHTTP, m, manifest.LeadFromRow(in.Lead), cred, r.d.URLCheck)
	if ctx.Err() != nil {
		return providers.Output{}, false, ctx.Err()
	}
	if errors.Is(cctx.Err(), context.DeadlineExceeded) {
		return providers.Output{Error: "timeout"}, true, nil
	}
	out := providers.Output{Confidence: res.Confidence, Error: res.Error}
	if len(res.Fields) > 0 {
		out.Extra = providers.NewObj()
		keys := make([]string, 0, len(res.Fields))
		for k := range res.Fields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out.Extra.Set(k, res.Fields[k])
		}
		out.Value = res.Fields[in.Field]
	}
	if out.Error == "" && out.Confidence == 0 {
		out.Confidence = m.DefaultConfidence
	}
	return out, false, nil
}
