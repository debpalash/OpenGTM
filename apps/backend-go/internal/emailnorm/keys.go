package emailnorm

import "strings"

func gmailCollapsed(canonical string) string {
	at := strings.LastIndex(canonical, "@")
	if at < 0 {
		return ""
	}
	local, domain := canonical[:at], canonical[at+1:]
	if domain != "gmail.com" && domain != "googlemail.com" {
		return ""
	}
	if i := strings.Index(local, "+"); i >= 0 {
		local = local[:i]
	}
	local = strings.ReplaceAll(local, ".", "")
	if local == "" {
		return ""
	}
	return local + "@" + domain
}

// MatchKeys is suppression_match_keys: the canonical form, plus the
// dot/plus-collapsed form for gmail.com / googlemail.com only.
func MatchKeys(email string) []string {
	canonical := Normalize(email)
	if canonical == "" {
		return []string{}
	}
	keys := []string{canonical}
	if c := gmailCollapsed(canonical); c != "" && c != canonical {
		keys = append(keys, c)
	}
	return keys
}
