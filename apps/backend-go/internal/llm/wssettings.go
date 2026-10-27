package llm

import (
	"context"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

// WorkspaceSettings resolves provider keys, URLs and models for one workspace:
// the workspace's own secret first, then the global setting, then the
// environment (secrets.Resolver order). Python read only the global setting, so
// this is a superset: with no workspace-level secret the result is identical.
type WorkspaceSettings struct {
	Resolver    *secrets.Resolver
	WorkspaceID string
}

// Get implements Settings.
func (w WorkspaceSettings) Get(ctx context.Context, key, def string) (string, error) {
	v, _, err := w.Resolver.Get(ctx, w.WorkspaceID, key)
	if err != nil {
		return "", err
	}
	if v == "" {
		return def, nil
	}
	return v, nil
}
