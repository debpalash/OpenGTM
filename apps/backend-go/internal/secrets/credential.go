package secrets

import (
	"context"
	"os"
	"strings"
)

// AllowGlobalCredentialFallback reports ALLOW_GLOBAL_CREDENTIAL_FALLBACK (default false). Output-integration
// credentials (CRM, outreach and spreadsheet integrations) are the workspace's own by default, so a workspace
// without its own token can never push into the operator's account (final security Low 5). A single-tenant
// self-host that configures those keys globally (app settings or the environment) opts back in with true.
func AllowGlobalCredentialFallback() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ALLOW_GLOBAL_CREDENTIAL_FALLBACK"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Credential resolves an output-integration credential for ws: the workspace secret only, or the full
// workspace > global > environment chain when AllowGlobalCredentialFallback. Unset is "".
func (r *Resolver) Credential(ctx context.Context, ws, key string) (string, error) {
	v, src, err := r.Get(ctx, ws, key)
	if err != nil {
		return "", err
	}
	if src != SourceWorkspace && !AllowGlobalCredentialFallback() {
		return "", nil
	}
	return v, nil
}

// EnvCredential is the environment-only lookup of a client built without a resolver: honoured only with
// AllowGlobalCredentialFallback (the environment is the operator's).
func EnvCredential(key string) string {
	if !AllowGlobalCredentialFallback() {
		return ""
	}
	return os.Getenv(key)
}
