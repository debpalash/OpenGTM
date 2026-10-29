package dsnparse

import (
	"regexp"
	"strings"
)

// hardPhrases are rejection phrases; they count only on a diagnostic line (see diagnosticLines).
var hardPhrases = []string{
	"mailbox unavailable", "user unknown", "no such user", "recipient rejected",
	"does not exist", "mailbox not found", "address rejected", "unknown user",
}

var (
	// smtpReplyRE is a permanent-failure reply code at a token boundary followed by a space, a hyphen or the end.
	smtpReplyRE = regexp.MustCompile(`(?:^|[^0-9.])(?:550|551|553|554)(?:[ \-]|$)`)
	// enhancedStatusRE is an enhanced status code for "no such mailbox" style failures at a token boundary.
	enhancedStatusRE = regexp.MustCompile(`(?:^|[^0-9.])5\.(?:1\.1|1\.2|1\.10|4\.1)(?:[^0-9.]|$)`)
	// diagLineRE selects the lines that carry an SMTP verdict: Diagnostic-Code and Status fields, and lines that
	// begin with a reply code or an enhanced status (optionally after "<addr>:" or "host x said:").
	diagLineRE = regexp.MustCompile(`(?i)^(?:diagnostic-code|status)[ \t]*:|^(?:<[^>]*>:?[ \t]*)?(?:host [^ ]+ said:[ \t]*)?(?:[45][0-9]{2}[ \-]|[45]\.[0-9]{1,3}\.[0-9]{1,3}[ \t])`)
)

// htmlTagRE strips tags so an HTML-only bounce is read as its text.
var htmlTagRE = regexp.MustCompile(`<[^<>@]*>`)

// phraseLedRE is a line that begins with a rejection phrase ("Mailbox unavailable", "<a@b>: user unknown").
var phraseLedRE = regexp.MustCompile(`(?i)^(?:<[^>]*>:?[ \t]*|(?:delivery (?:has )?failed|delivery failure|failed|error|reason|undeliverable|permanent (?:failure|error))[ \t]*[:\-][ \t]*)?(?:mailbox unavailable|user unknown|no such user|recipient rejected|mailbox not found|address rejected|unknown user)`)

// diagnosticLines are the lines of the first text part that can state a delivery verdict: the Diagnostic-Code
// and Status fields, lines that start with an SMTP reply or an enhanced status, and lines that start with a
// rejection phrase. A number or phrase inside running text ("order #5501", "save up to $550", "that product does
// not exist") is not one.
func diagnosticLines(body string) []string {
	var out []string
	for _, l := range strings.Split(body, "\n") {
		l = strings.TrimSpace(htmlTagRE.ReplaceAllString(l, ""))
		if l != "" && (diagLineRE.MatchString(l) || phraseLedRE.MatchString(l)) {
			out = append(out, pyLower(l))
		}
	}
	return out
}

func hardDiagnostic(lines []string) bool {
	for _, l := range lines {
		if smtpReplyRE.MatchString(l) || enhancedStatusRE.MatchString(l) || containsAny(l, hardPhrases) {
			return true
		}
	}
	return false
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
// the first text part. Hard needs a reply code, enhanced status or phrase on one of the part's diagnostic lines
// (diagnosticLines), not anywhere in the text.
func parseHeuristic(root *part) ([]Record, error) {
	subject := ""
	if v, ok := root.getRaw("subject"); ok {
		subject = strValue(v)
	}
	body := firstText(root)
	blob := pyLower(subject + "\n" + body)
	hasHard := hardDiagnostic(diagnosticLines(body))
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
