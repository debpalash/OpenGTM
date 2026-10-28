package intkit

import (
	"context"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

type appOnlyStore struct{ app map[string]string }

func (s appOnlyStore) AppSetting(_ context.Context, key string) (string, error) {
	return s.app[key], nil
}
func (s appOnlyStore) WorkspaceSetting(context.Context, string, string) (string, bool, error) {
	return "", false, nil
}

// Final security Low 5: integration credentials are workspace-only by default; the operator's global or
// environment key is used only with ALLOW_GLOBAL_CREDENTIAL_FALLBACK=true.
func TestResolverSecretsWorkspaceOnlyByDefault(t *testing.T) {
	env, err := secrets.NewEnvelope(secrets.Config{MasterKey: "intkit-test-master", AppEnv: "dev"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := ResolverSecrets{R: secrets.NewResolver(env, appOnlyStore{}, func(k string) string {
		if k == "INSTANTLY_API_KEY" {
			return "operator-key"
		}
		return ""
	})}
	if v, err := Key(context.Background(), r, "ws-x", "INSTANTLY_API_KEY"); err != nil || v != "" {
		t.Fatalf("operator key leaked to a workspace: %q %v", v, err)
	}
	t.Setenv("ALLOW_GLOBAL_CREDENTIAL_FALLBACK", "true")
	if v, err := Key(context.Background(), r, "ws-x", "INSTANTLY_API_KEY"); err != nil || v != "operator-key" {
		t.Fatalf("opt-in fallback: %q %v", v, err)
	}
}
