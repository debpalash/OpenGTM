package smtpmail

import (
	"encoding/json"
	"fmt"
	"html"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
)

var placeholderRe = regexp.MustCompile(`\{\{([^}]+)\}\}`)

// Render replaces {{name}} with str(value). The key is not trimmed, missing
// keys and nil values render empty, and the result is trimmed (Python strip).
func Render(template string, vars map[string]any) string {
	out := placeholderRe.ReplaceAllStringFunc(template, func(m string) string {
		return pyStr(vars[m[2:len(m)-2]])
	})
	return pyStrip(out)
}

// RenderHTML is Render for the HTML part: each substituted value is HTML-escaped (the template itself
// is trusted markup), so scraped lead fields cannot inject tags or attributes. Deliberate deviation:
// Python substitutes raw (plan 09 final security M3). The text part is rendered with Render.
func RenderHTML(template string, vars map[string]any) string {
	out := placeholderRe.ReplaceAllStringFunc(template, func(m string) string {
		return html.EscapeString(pyStr(vars[m[2:len(m)-2]]))
	})
	return pyStrip(out)
}

// LeadVariables mirrors build_lead_variables. Nil columns stay nil (Python
// getattr returns None) and render empty.
func LeadVariables(l leadsread.Lead) map[string]any {
	sp := func(p *string) any {
		if p == nil {
			return nil
		}
		return *p
	}
	var score any
	if l.Score != nil {
		score = *l.Score
	}
	return map[string]any{
		"company":        l.Company,
		"email":          sp(l.Email),
		"name":           sp(l.ContactPerson),
		"contact_person": sp(l.ContactPerson),
		"city":           sp(l.City),
		"phone":          sp(l.Phone),
		"website":        sp(l.Website),
		"specialization": sp(l.Specialization),
		"company_size":   sp(l.CompanySize),
		"score":          score,
		"title":          sp(l.ContactTitle),
	}
}

// pyStr is Python str(v) for JSON-shaped values; nil is the empty string
// (the renderer maps None to "").
func pyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	}
	return pyRepr(v)
}

func pyRepr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case string:
		return pyQuote(x)
	case json.Number:
		return x.String()
	case int:
		return strconv.Itoa(x)
	case int32:
		return strconv.FormatInt(int64(x), 10)
	case int64:
		return strconv.FormatInt(x, 10)
	case float32:
		return pyFloat(float64(x))
	case float64:
		return pyFloat(x)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = pyRepr(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys) // Go maps carry no insertion order
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = pyQuote(k) + ": " + pyRepr(x[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprint(v)
}

func pyFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	e := strconv.FormatFloat(f, 'e', -1, 64)
	i := strings.IndexByte(e, 'e')
	exp, _ := strconv.Atoi(e[i+1:])
	if exp < -4 || exp >= 16 {
		return e
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

func pyQuote(s string) string {
	q := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		q = '"'
	}
	var b strings.Builder
	b.WriteByte(q)
	for _, r := range s {
		switch {
		case r == rune(q) || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 32 || r == 127:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(q)
	return b.String()
}
