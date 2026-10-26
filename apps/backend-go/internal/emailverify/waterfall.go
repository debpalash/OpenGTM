package emailverify

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// usableRE is _EMAIL_RE.fullmatch; Python's \w is Unicode, so letters, digits
// and underscore of any script.
var usableRE = regexp.MustCompile(`^[\p{L}\p{N}_.\-+]+@[\p{L}\p{N}_.\-]+\.[\p{L}\p{N}_]{2,}$`)

var (
	junkLocals  = map[string]bool{"example": true, "test": true, "noreply": true, "no-reply": true, "donotreply": true}
	junkDomains = map[string]bool{
		"example.com": true, "domain.com": true, "email.com": true, "sentry.io": true, "wixpress.com": true,
		"w3.org": true, "schema.org": true, "googleapis.com": true, "gravatar.com": true, "wordpress.org": true,
		"cloudflare.com": true, "hunter.io": true, "snov.io": true,
	}
	imageExts = []string{".png", ".jpg", ".svg", ".gif", ".webp"}
)

// IsUsableEmail is _is_usable_email: a cheap gate so obvious junk (image names,
// vendor and placeholder domains, noreply locals) is never stored as a discovered email.
func IsUsableEmail(email string) bool {
	if email == "" || !strings.Contains(email, "@") {
		return false
	}
	email = strings.ToLower(providers.PyStrip(email))
	if !usableRE.MatchString(email) {
		return false
	}
	local, domain, _ := strings.Cut(email, "@")
	if junkLocals[local] || junkDomains[domain] {
		return false
	}
	for _, ext := range imageExts {
		if strings.HasSuffix(email, ext) {
			return false
		}
	}
	return true
}

// DomainFromURL is _domain_from_url: lower-case, default to https://, take the
// netloc (as urllib.parse.urlparse reads it) and drop a leading "www.".
func DomainFromURL(raw string) string {
	if raw == "" {
		return ""
	}
	u := strings.ToLower(providers.PyStrip(raw))
	if !strings.HasPrefix(u, "http") {
		u = "https://" + u
	}
	netloc := ""
	if i := strings.IndexByte(u, ':'); i > 0 && validScheme(u[:i]) {
		if rest := u[i+1:]; strings.HasPrefix(rest, "//") {
			netloc = rest[2:]
			if j := strings.IndexAny(netloc, "/?#"); j >= 0 {
				netloc = netloc[:j]
			}
		}
	}
	return strings.TrimPrefix(netloc, "www.")
}

func validScheme(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case i > 0 && (c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.'):
		default:
			return false
		}
	}
	return s != ""
}

// Finder is one email-finder provider in the waterfall. Available is a
// no-network gate (credentials present). Find returns (email, bonus contact
// fields) or ("", nil); an error is recorded as a failed step and the chain
// continues.
type Finder interface {
	Name() string
	Available(ctx context.Context, workspaceID string) bool
	Find(ctx context.Context, workspaceID string, lead map[string]any, domain string) (email string, extra map[string]string, err error)
}

// Step is WaterfallStep.to_dict.
type Step struct {
	Provider string `json:"provider"`
	Success  bool   `json:"success"`
	Email    string `json:"email,omitempty"`
	Skipped  bool   `json:"skipped,omitempty"` // provider unavailable: not counted as a try
	Error    string `json:"error,omitempty"`   // first 120 characters
}

// WaterfallResult is WaterfallOutcome.
type WaterfallResult struct {
	Email    string `json:"email"`
	Provider string `json:"provider"`
	Found    bool   `json:"found"`
	Attempts int    `json:"attempts"` // providers actually invoked (skipped ones excluded)
	Steps    []Step `json:"steps"`
}

// Waterfall is find_email_waterfall: for a lead without an email, try each
// finder in order and stop at the first usable address. It MUTATES lead like
// Python: email and email_provider only on a hit, contact_person/contact_title
// only when the lead has none, and always enrichment_waterfall (appended
// "email_finder" entry) and enrichment_attempts. A lead that already has an
// email is a no-op. The error is non-nil only when ctx ends.
func Waterfall(ctx context.Context, d Deps, workspaceID string, lead map[string]any) (WaterfallResult, error) {
	out := WaterfallResult{Steps: []Step{}}
	if hasEmail(lead) {
		return out, nil
	}
	website, _ := lead["website"].(string)
	domain := DomainFromURL(website)
	for _, f := range d.Finders {
		if err := ctx.Err(); err != nil {
			return WaterfallResult{}, err
		}
		if !f.Available(ctx, workspaceID) {
			out.Steps = append(out.Steps, Step{Provider: f.Name(), Skipped: true})
			continue
		}
		step := Step{Provider: f.Name()}
		email, extra, err := f.Find(ctx, workspaceID, lead, domain)
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return WaterfallResult{}, cerr
			}
			step.Error = truncRunes(err.Error(), 120)
			out.Steps = append(out.Steps, step)
			continue
		}
		if email != "" && IsUsableEmail(email) {
			step.Success, step.Email = true, email
			out.Steps = append(out.Steps, step)
			out.Email, out.Provider = email, f.Name()
			lead["email"], lead["email_provider"] = email, f.Name()
			for _, k := range []string{"contact_person", "contact_title"} {
				if v := extra[k]; v != "" {
					if cur, _ := lead[k].(string); cur == "" {
						lead[k] = v
					}
				}
			}
			break
		}
		out.Steps = append(out.Steps, step)
	}
	out.Found = out.Email != ""
	for _, s := range out.Steps {
		if !s.Skipped {
			out.Attempts++
		}
	}
	recordProvenance(d, lead, out)
	return out, nil
}

func recordProvenance(d Deps, lead map[string]any, out WaterfallResult) {
	entry := pyEntry(out)
	var existing []json.RawMessage
	if s, _ := lead["enrichment_waterfall"].(string); s != "" {
		var v any
		if json.Unmarshal([]byte(s), &v) == nil {
			if _, isList := v.([]any); isList {
				_ = json.Unmarshal([]byte(s), &existing)
			} else {
				existing = []json.RawMessage{json.RawMessage(s)}
			}
		}
	}
	var b strings.Builder
	b.WriteByte('[')
	for _, e := range existing {
		b.Write(e)
		b.WriteString(", ")
	}
	b.WriteString(entry)
	b.WriteByte(']')
	lead["enrichment_waterfall"] = b.String()
	lead["enrichment_attempts"] = toInt(lead["enrichment_attempts"]) + out.Attempts
	if d.Provenance != nil && out.Email != "" && out.Provider != "" {
		d.Provenance(lead, out.Provider)
	}
}

func toInt(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case json.Number:
		n, _ := x.Int64()
		return int(n)
	}
	return 0
}

// pyEntry renders the "email_finder" provenance entry the way Python's
// json.dumps does (", " and ": " separators, ensure_ascii).
func pyEntry(out WaterfallResult) string {
	var b strings.Builder
	b.WriteString(`{"field": "email_finder", "winner": `)
	b.WriteString(pyString(out.Provider))
	b.WriteString(`, "value": `)
	if out.Email == "" {
		b.WriteString("null")
	} else {
		b.WriteString(pyString(out.Email))
	}
	b.WriteString(`, "attempts": [`)
	for i, s := range out.Steps {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(`{"provider": ` + pyString(s.Provider) + `, "success": `)
		if s.Success {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
		if s.Email != "" {
			b.WriteString(`, "email": ` + pyString(s.Email))
		}
		if s.Skipped {
			b.WriteString(`, "skipped": true`)
		}
		if s.Error != "" {
			b.WriteString(`, "error": ` + pyString(s.Error))
		}
		b.WriteByte('}')
	}
	b.WriteString("]}")
	return b.String()
}

func truncRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// pyString is json.dumps for a str with ensure_ascii=True.
func pyString(s string) string {
	const hex = "0123456789abcdef"
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r < 0x20 || (r > 0x7f && r < 0x10000):
			u := func(c rune) {
				b.WriteString(`\u` + string([]byte{hex[c>>12&15], hex[c>>8&15], hex[c>>4&15], hex[c&15]}))
			}
			u(r)
		case r >= 0x10000:
			r -= 0x10000
			hi, lo := 0xd800+(r>>10), 0xdc00+(r&0x3ff)
			for _, c := range []rune{hi, lo} {
				b.WriteString(`\u` + string([]byte{hex[c>>12&15], hex[c>>8&15], hex[c>>4&15], hex[c&15]}))
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
