// Package outurl is the destination rule of an output webhook URL template (plan 15 review M3):
// scheme, userinfo, host and port must be literal in the template, and every value a row
// substitutes into the path, query or fragment is percent-encoded, so row data (often scraped)
// can never choose where the row and the configured headers are sent. It is the same rule as the
// automation webhook's (autoactions resolveWebhookURL). Column-config validation and delivery
// both call it, so an unsafe template is refused when saved and again when delivered.
package outurl

import (
	"errors"
	"net/url"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/rowtmpl"
)

// ErrNotLiteral is returned for a template whose scheme or authority holds a placeholder.
var ErrNotLiteral = errors.New("webhook url scheme and host must be literal")

// ErrHostChanged is returned when the rendered URL does not keep the template's host.
var ErrHostChanged = errors.New("webhook url host changed by template")

// split returns the literal head (scheme and authority) and the rest of the template.
func split(raw string) (head, tail string, ok bool) {
	i := strings.Index(raw, "://")
	if i < 0 {
		return raw, "", false
	}
	if j := strings.IndexAny(raw[i+3:], "/?#"); j >= 0 {
		return raw[:i+3+j], raw[i+3+j:], true
	}
	return raw, "", true
}

// ValidateTemplate refuses a template with a placeholder in its scheme, userinfo, host or port.
// A template without placeholders is always accepted here (the egress policy vets it).
func ValidateTemplate(raw string) error {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "{") {
		return nil
	}
	head, _, ok := split(raw)
	if !ok || strings.Contains(head, "{") {
		return ErrNotLiteral
	}
	return nil
}

// Resolve renders the template with resolve (strict row substitution), encoding substituted
// values, and verifies the host is the template's.
func Resolve(raw string, row map[string]any, resolve func(tmpl string, row map[string]any) (string, error)) (string, error) {
	if err := ValidateTemplate(raw); err != nil {
		return "", err
	}
	if !strings.Contains(raw, "{") {
		return resolve(raw, row)
	}
	head, tail, _ := split(raw)
	enc := make(map[string]any, len(row))
	for k, v := range row {
		switch t := v.(type) {
		case string:
			enc[k] = escape(t)
		case nil, bool, int, int32, int64, float64:
			enc[k] = v
		default: // numbers decoded as json.Number and structured values: render, then encode
			s := rowtmpl.PyStr(v)
			if _, isNum := v.(interface{ Int64() (int64, error) }); isNum {
				enc[k] = v
			} else {
				enc[k] = escape(s)
			}
		}
	}
	rt, err := resolve(tail, enc)
	if err != nil {
		return "", err
	}
	out := head + rt
	a, err1 := url.Parse(head)
	b, err2 := url.Parse(strings.NewReplacer("\t", "", "\r", "", "\n", "").Replace(out))
	if err1 != nil || err2 != nil || !strings.EqualFold(a.Host, b.Host) {
		return "", ErrHostChanged
	}
	return out, nil
}

func escape(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "+", "%20") }
