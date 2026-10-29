package config

import "strings"

// OutboundSecretRefPrefixes reads OUTBOUND_SECRET_REF_PREFIXES (comma list; default
// WEBHOOK_,AUTOMATION_,DEST_,SLACK_): the namespaces a secret ref sent to a
// user-chosen URL must start with. Entries are trimmed; empties are dropped.
func OutboundSecretRefPrefixes(get func(string) string) []string {
	var out []string
	for _, p := range strings.Split(get("OUTBOUND_SECRET_REF_PREFIXES"), ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{"WEBHOOK_", "AUTOMATION_", "DEST_", "SLACK_"}
	}
	return out
}
