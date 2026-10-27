package secrets

import (
	"errors"
	"strings"
)

// OutboundRefMessage is the single refusal text for an outbound secret ref that is
// not allowed or does not exist (post-port ruling R2): the two cases must be
// indistinguishable so the text is not an oracle for which keys exist.
const OutboundRefMessage = "header_secret_ref is not an available webhook secret"

// ErrOutboundRefNotAllowed is returned by CheckOutboundRef. Callers map it to a 422
// (with OutboundRefMessage) at create/patch, and fail the action closed at delivery.
var ErrOutboundRefNotAllowed = errors.New(OutboundRefMessage)

// DefaultOutboundRefPrefixes is the default OUTBOUND_SECRET_REF_PREFIXES.
var DefaultOutboundRefPrefixes = []string{"WEBHOOK_", "AUTOMATION_", "DEST_", "SLACK_"}

// deniedOutboundPrefixes are credential families that never leave through a
// user-chosen URL, whatever the prefix list says.
var deniedOutboundPrefixes = []string{
	"SMTP_", "IMAP_", "HUBSPOT_", "SALESFORCE_", "META_", "LINKEDIN_", "GOOGLE_ADS_",
}

// OutboundPolicy decides which secret names may be sent to a user-chosen URL.
type OutboundPolicy struct {
	// Prefixes: a ref must start with one of these (case-sensitive). Empty means
	// DefaultOutboundRefPrefixes.
	Prefixes []string
	// Registered reports a registered provider/CRM credential (settings
	// Registry.SecretSet()). May be nil.
	Registered func(name string) bool
}

// CheckOutboundRef returns nil when name may be used as an outbound secret ref, else
// ErrOutboundRefNotAllowed. It checks policy only; existence is checked by the
// caller (workspace-only lookup) and must be reported with the same error.
func CheckOutboundRef(name string, p OutboundPolicy) error {
	if name == "" || strings.TrimSpace(name) != name {
		return ErrOutboundRefNotAllowed
	}
	prefixes := p.Prefixes
	if len(prefixes) == 0 {
		prefixes = DefaultOutboundRefPrefixes
	}
	ok := false
	for _, pre := range prefixes {
		if pre != "" && strings.HasPrefix(name, pre) {
			ok = true
			break
		}
	}
	if !ok {
		return ErrOutboundRefNotAllowed
	}
	up := strings.ToUpper(name)
	for _, d := range deniedOutboundPrefixes {
		if strings.HasPrefix(up, d) {
			return ErrOutboundRefNotAllowed
		}
	}
	if p.Registered != nil && (p.Registered(name) || p.Registered(up)) {
		return ErrOutboundRefNotAllowed
	}
	return nil
}
