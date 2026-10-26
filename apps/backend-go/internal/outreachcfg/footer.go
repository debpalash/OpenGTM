package outreachcfg

import (
	"context"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

// Footer returns the workspace OUTREACH_FOOTER (physical address), trimmed;
// empty when unset or unresolvable.
func Footer(ctx context.Context, r *secrets.Resolver, workspaceID string) string {
	v, _, err := r.Get(ctx, workspaceID, "OUTREACH_FOOTER")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(v)
}
