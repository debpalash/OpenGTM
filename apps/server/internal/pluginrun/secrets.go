package pluginrun

import (
	"context"
	"fmt"
	"os"

	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/declarative"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/manifest"
)

// SecretStore supplies per-workspace secret values to the Runner. found is
// false when the workspace has no value for name (the Runner then falls back
// to the environment); an error means the lookup could not be answered
// reliably (database down, undecryptable value) and must fail the run rather
// than silently use another credential. Implementations must never put a
// secret value in an error.
type SecretStore interface {
	WorkspaceSecret(ctx context.Context, workspaceID, name string) (value string, found bool, err error)
}

type workspaceKey struct{}

// WithWorkspace tells the Runner which workspace a run belongs to, so its
// declared secrets resolve from that workspace's store. Without it only the
// environment is consulted.
func WithWorkspace(ctx context.Context, workspaceID string) context.Context {
	return context.WithValue(ctx, workspaceKey{}, workspaceID)
}

func workspaceFrom(ctx context.Context) string {
	id, _ := ctx.Value(workspaceKey{}).(string)
	return id
}

// SetSecretStore enables workspace secrets. Call it before the Runner is
// used; a nil store means environment-only resolution.
func (r *Runner) SetSecretStore(s SecretStore) { r.secretStore = s }

// secretsFor builds the resolver for one run.
func (r *Runner) secretsFor(ctx context.Context, p *manifest.Plugin) declarative.SecretResolver {
	if r.secretStore == nil {
		return envSecrets{p}
	}
	return layeredSecrets{p: p, store: r.secretStore, workspace: workspaceFrom(ctx)}
}

// layeredSecrets resolves a plugin's declared secrets: the workspace's own
// secret first, then the process environment. Undeclared names never
// resolve, from either source.
type layeredSecrets struct {
	p         *manifest.Plugin
	store     SecretStore
	workspace string
}

func (s layeredSecrets) Secret(ctx context.Context, name string) (string, error) {
	if !s.p.HasSecret(name) {
		return "", nil
	}
	if s.workspace != "" {
		v, found, err := s.store.WorkspaceSecret(ctx, s.workspace, name)
		if err != nil {
			return "", fmt.Errorf("workspace secret %s is unavailable: %w", name, err)
		}
		if found {
			return v, nil
		}
	}
	return os.Getenv(name), nil
}
