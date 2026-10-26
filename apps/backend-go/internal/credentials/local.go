package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

// ErrUnknownCredential means the env var is not named by any manifest
// auth.env_var, so it is never served (the HTTP endpoint answers 404).
var ErrUnknownCredential = errors.New("credentials: unknown credential")

// Local resolves credentials in-process (workspace secret -> settings DB ->
// environment) with the same semantics as Client. Safe for concurrent use.
type Local struct {
	r     *secrets.Resolver
	allow func(envVar string) bool
	now   func() time.Time

	mu    sync.Mutex
	cache map[cacheKey]cacheEntry
}

// NewLocal builds a Local. allow gates which env vars may be resolved; now may
// be nil (time.Now).
func NewLocal(r *secrets.Resolver, allow func(envVar string) bool, now func() time.Time) *Local {
	if now == nil {
		now = time.Now
	}
	return &Local{r: r, allow: allow, now: now, cache: make(map[cacheKey]cacheEntry)}
}

// Get returns the credential for envVar in workspaceID. ("", nil) means not
// configured and is never cached; errors (including decrypt failures) never
// carry values and are never cached.
func (l *Local) Get(ctx context.Context, workspaceID, envVar string) (string, error) {
	if workspaceID == "" || envVar == "" {
		return "", errors.New("credentials: workspace id and env var are required")
	}
	if l.allow == nil || !l.allow(envVar) {
		return "", ErrUnknownCredential
	}
	key := cacheKey{workspaceID, envVar}
	l.mu.Lock()
	if e, ok := l.cache[key]; ok {
		if l.now().Before(e.expires) {
			l.mu.Unlock()
			return e.value, nil
		}
		delete(l.cache, key)
	}
	l.mu.Unlock()

	v, _, err := l.r.Get(ctx, workspaceID, envVar)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		// The resolver's errors are value-free (fixed decrypt text or driver
		// errors); name only the env var, never wrap request-derived data.
		return "", errors.New("credentials: could not resolve " + envVar + ": " + safeErr(err))
	}
	if v != "" {
		l.mu.Lock()
		l.cache[key] = cacheEntry{value: v, expires: l.now().Add(TTL)}
		l.mu.Unlock()
	}
	return v, nil
}

// safeErr keeps the secrets decrypt message (a fixed string) and reduces
// anything else to a generic marker so no driver text can echo a value.
func safeErr(err error) string {
	if errors.Is(err, secrets.ErrWrongKey) {
		return secrets.ErrWrongKey.Error()
	}
	return "lookup failed"
}

type manifestFetcher interface {
	Manifest(ctx context.Context, name string) (json.RawMessage, string, error)
}

// ManifestAllow wraps a manifest source and records the auth.env_var of every
// manifest it serves; Allowed is the allow-list for Local. The worker fetches a
// run's manifests before executing any cell, so credentials are only ever
// requested for env vars of manifests already loaded.
type ManifestAllow struct {
	src manifestFetcher
	mu  sync.RWMutex
	set map[string]struct{}
}

func NewManifestAllow(src manifestFetcher) *ManifestAllow {
	return &ManifestAllow{src: src, set: make(map[string]struct{})}
}

// Manifest satisfies workbook.ManifestSource.
func (a *ManifestAllow) Manifest(ctx context.Context, name string) (json.RawMessage, string, error) {
	raw, sha, err := a.src.Manifest(ctx, name)
	if err != nil {
		return raw, sha, err
	}
	var m struct {
		Auth struct {
			EnvVar string `json:"env_var"`
		} `json:"auth"`
	}
	if json.Unmarshal(raw, &m) == nil && m.Auth.EnvVar != "" {
		a.mu.Lock()
		a.set[m.Auth.EnvVar] = struct{}{}
		a.mu.Unlock()
	}
	return raw, sha, nil
}

// Allowed reports whether envVar is named by a loaded manifest.
func (a *ManifestAllow) Allowed(envVar string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	_, ok := a.set[envVar]
	return ok
}
