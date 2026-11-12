package collect

import (
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/emailverify"
)

// Email confidence tagging at discovery. Before this, leads harvested by the
// discovery strategies carried an email with an EMPTY email_confidence, which
// made harvested addresses indistinguishable from provider-verified ones
// downstream (and the pipeline wrote them to the DB that way). tagHarvestedEmails
// fills every still-untagged email with its honest class:
//
//	role  — role/catch-all mailbox (info@, sales@, security@, ...)
//	free  — free consumer provider (gmail.com, ...)
//	found — harvested from the page, unverified
//
// Only leads whose confidence is empty are touched, so the verification stages
// (verified, pattern, smtp_verified, risky) always win. The helper is silent on
// the ledger by design: the recorded job vectors pin the stage sequence.
func tagHarvestedEmails(leads []*Lead) {
	for _, l := range leads {
		if l.Email == "" || l.EmailConfidence != "" || !strings.Contains(l.Email, "@") {
			continue
		}
		l.EmailConfidence = classifyHarvestedEmail(l.Email)
	}
}

// classifyHarvestedEmail is the honest class of a harvested address: role, free,
// or found. It NEVER returns verified/smtp_verified — evidence-based verdicts
// belong to the verification stages.
func classifyHarvestedEmail(email string) string {
	c := emailverify.Classify(email)
	switch {
	case c.IsRole:
		return "role"
	case c.IsFree:
		return "free"
	}
	return "found"
}
