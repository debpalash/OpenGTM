package dsnparse

import (
	"regexp"
	"strings"
)

var hardTokens = []string{
	"mailbox unavailable", "user unknown", "no such user", "recipient rejected",
	"does not exist", "mailbox not found", "address rejected", "unknown user",
	"550", "551", "553", "554", "5.1.1", "5.1.2", "5.1.10", "5.4.1",
}

var bounceMarkers = []string{
	"delivery status notification", "undeliverable", "mail delivery failed",
	"returned mail", "delivery failure", "failure notice", "delivery has failed",
	"could not be delivered", "mail delivery subsystem",
}

// uniWS is Python's \s for str patterns (str.isspace).
const uniWS = `\t\n\v\f\r\x1c-\x1f \x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}`

var (
	msgIDRE       = regexp.MustCompile(`<[^>@` + uniWS + `]+@[^>` + uniWS + `]+>`)
	msgIDHeaderRE = regexp.MustCompile(`(?i)message-id:[` + uniWS + `]*(<[^>]+>)`)
)

// pyLower is str.lower() for the token comparison (U+0130 lowers to two code points).
func pyLower(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "İ", "i̇"))
}

// parseHeuristic is _parse_heuristic: conservative token match over subject and
// the first text part. "550" or "does not exist" anywhere counts as hard (kept).
func parseHeuristic(root *part) ([]Record, error) {
	subject := ""
	if v, ok := root.getRaw("subject"); ok {
		subject = strValue(v)
	}
	body := firstText(root)
	blob := pyLower(subject + "\n" + body)
	hasHard := containsAny(blob, hardTokens)
	looksBounce := containsAny(blob, bounceMarkers)
	if !hasHard && !looksBounce {
		return nil, nil
	}
	kind := "unknown"
	if hasHard {
		kind = "hard"
	}
	return []Record{{
		Recipient:         strings.ToLower(emailRE.FindString(body)),
		Kind:              kind,
		Diagnostic:        "heuristic plain-text bounce",
		OriginalMessageID: findMessageID(body),
	}}, nil
}

func containsAny(s string, tokens []string) bool {
	for _, t := range tokens {
		if strings.Contains(s, t) {
			return true
		}
	}
	return false
}

// firstText is _first_text: the first non-empty decoded text/plain part, else
// the decoded payload of a non-multipart message.
func firstText(root *part) string {
	text := ""
	found := false
	walk(root, func(p *part) {
		if found || p.contentType() != "text/plain" {
			return
		}
		if raw := payloadBytes(p); len(raw) > 0 {
			text, found = decodeUTF8Replace(raw), true
		}
	})
	if found {
		return text
	}
	if !root.isList {
		if raw := payloadBytes(root); len(raw) > 0 {
			return decodeUTF8Replace(raw)
		}
	}
	return ""
}

func findMessageID(body string) string {
	if body == "" {
		return ""
	}
	if m := msgIDHeaderRE.FindStringSubmatch(body); m != nil {
		return strings.TrimSpace(m[1])
	}
	return strings.TrimSpace(msgIDRE.FindString(body))
}
