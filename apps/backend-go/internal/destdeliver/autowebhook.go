package destdeliver

import (
	"context"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autoactions"
)

// AutoWebhook is the WebhookSender over the automation webhook sender (S-M3): the same
// egress-pinned dialer, domain allowlist, literal-host rule and outbound secret-ref policy as an
// automation action, so webhook and Slack destinations deliver in Go. header_secret_ref resolves
// workspace-only through Deps.Secrets; a ref outside the policy or missing fails the delivery
// without sending. The request Payload is the template context (the field_map-mapped data).
type AutoWebhook struct{ Deps autoactions.WebhookDeps }

// Send posts one destination webhook or Slack message.
func (a AutoWebhook) Send(ctx context.Context, r WebhookRequest) (Result, error) {
	deps := a.Deps
	deps.LiteralBody = true // the envelope carries lead data: never re-templated
	res := autoactions.Webhook(ctx, deps, r.WorkspaceID, r.Config, r.Payload, nil)
	if res.Status == "success" {
		return Result{Success: true, Summary: res.Summary}, nil
	}
	return Result{Summary: res.Summary, Error: res.Error}, nil
}
