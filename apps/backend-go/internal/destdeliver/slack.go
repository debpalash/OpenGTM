package destdeliver

import (
	"context"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/destvalidate"
)

const maxSlackChars = 3000

// slack renders the message and posts it to the webhook URL held in the workspace secret named
// by webhook_secret_ref. Template syntax beyond {name} fields is refused (plan 10 D6) instead
// of being evaluated as Python str.format would.
func (d *Deliverer) slack(ctx context.Context, dest Destination, leadID int, snapshot, mapped map[string]any, idem string, sec *secretSet) Result {
	const notFound = "Slack webhook secret not found"
	ref := strOr(dest.Config["webhook_secret_ref"], "")
	if ref == "" || d.ports.Secrets == nil {
		return Result{Error: notFound}
	}
	webhookURL, src, err := d.ports.Secrets.Get(ctx, dest.WorkspaceID, ref)
	if err != nil {
		return Result{Error: "Slack webhook secret lookup failed"}
	}
	// The reference names a workspace secret only: a global or env value never stands in (it would be sent to a
	// destination-configured URL).
	if webhookURL == "" || src != "workspace" {
		return Result{Error: notFound}
	}
	sec.add(webhookURL)

	template := strOr(dest.Config["message_template"], destvalidate.DefaultSlackTemplate)
	text, err := destvalidate.RenderSlackTemplate(template, mapped)
	if err != nil {
		return Result{Error: "invalid slack message_template"}
	}
	res, err := d.ports.Webhook.Send(ctx, WebhookRequest{
		WorkspaceID: dest.WorkspaceID, DestinationType: "slack", Payload: snapshot,
		LeadID: int64(leadID), IdempotencyKey: idem,
		Config: map[string]any{
			"url":     webhookURL,
			"method":  "POST",
			"headers": map[string]any{"Idempotency-Key": idem},
			"body": map[string]any{
				"text": truncChars(text, maxSlackChars),
				"metadata": map[string]any{
					"event_type": "opengtm_audience_member",
					"event_payload": map[string]any{
						"destination_id": dest.ID,
						"lead_id":        int64(leadID),
					},
				},
			},
		},
	})
	return finish(res, err, sec)
}
