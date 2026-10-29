package destdeliver

import (
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
)

// DefaultSlackRefPrefixes is the namespace a Slack webhook_secret_ref must also be in: the value
// of that secret is the webhook URL itself, so only dedicated Slack/destination secrets qualify
// (S-M2) even when OUTBOUND_SECRET_REF_PREFIXES is wider.
var DefaultSlackRefPrefixes = []string{"SLACK_", "DEST_"}

// CheckRef applies the outbound secret-ref policy (R2) to the reference a destination of type typ
// sends to its configured URL: the shared WP0 policy, plus the Slack namespace for slack. It
// checks the name only; the caller still resolves it workspace-only and treats a missing secret
// exactly like a refused one. slackPrefixes nil means DefaultSlackRefPrefixes.
func CheckRef(typ, ref string, p secrets.OutboundPolicy, slackPrefixes []string) error {
	if err := secrets.CheckOutboundRef(ref, p); err != nil {
		return err
	}
	if typ != "slack" {
		return nil
	}
	if len(slackPrefixes) == 0 {
		slackPrefixes = DefaultSlackRefPrefixes
	}
	for _, pre := range slackPrefixes {
		if pre != "" && strings.HasPrefix(ref, pre) {
			return nil
		}
	}
	return secrets.ErrOutboundRefNotAllowed
}
