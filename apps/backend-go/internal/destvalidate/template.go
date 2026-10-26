package destvalidate

import (
	"errors"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
)

// DefaultSlackTemplate is used when a slack destination has no template.
const DefaultSlackTemplate = "New audience member: {company} {contact_person} {email}"

// ErrInvalidTemplate is returned for a template that uses anything beyond
// {name} fields and {{ / }} escapes.
var ErrInvalidTemplate = errors.New("invalid slack message_template")

// Python formatted the template with str.format_map, so attribute access
// ({company.__class__}), indexing ({company[0]}), conversions ({company!r}) and
// format specs ({company:>10}) reached into the values. That is a template
// injection surface (D6), so only plain {name} fields and brace escapes are
// supported here; the rest is rejected rather than guessed at.
type tplPart struct {
	text  string
	field bool
}

func parseSlackTemplate(tpl string) ([]tplPart, error) {
	var parts []tplPart
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			parts = append(parts, tplPart{text: lit.String()})
			lit.Reset()
		}
	}
	for i := 0; i < len(tpl); i++ {
		c := tpl[i]
		switch c {
		case '{':
			if i+1 < len(tpl) && tpl[i+1] == '{' {
				lit.WriteByte('{')
				i++
				continue
			}
			end := strings.IndexByte(tpl[i+1:], '}')
			if end < 0 {
				return nil, ErrInvalidTemplate
			}
			name := tpl[i+1 : i+1+end]
			if !plainFieldName(name) {
				return nil, ErrInvalidTemplate
			}
			flush()
			parts = append(parts, tplPart{text: name, field: true})
			i += end + 1
		case '}':
			if i+1 < len(tpl) && tpl[i+1] == '}' {
				lit.WriteByte('}')
				i++
				continue
			}
			return nil, ErrInvalidTemplate
		default:
			lit.WriteByte(c)
		}
	}
	flush()
	return parts, nil
}

// plainFieldName accepts what Python treats as a plain mapping key: not empty,
// not all ASCII digits (positional), and no syntax characters.
func plainFieldName(name string) bool {
	if name == "" || strings.ContainsAny(name, "{}.[]!:") {
		return false
	}
	for i := 0; i < len(name); i++ {
		if name[i] < '0' || name[i] > '9' {
			return true
		}
	}
	return false
}

// CheckSlackTemplate reports ErrInvalidTemplate when the template uses
// anything but {name} fields and brace escapes.
func CheckSlackTemplate(tpl string) error {
	_, err := parseSlackTemplate(tpl)
	return err
}

// RenderSlackTemplate expands {name} fields from fields (a missing or nil key
// renders empty, like Python's __missing__) and returns the text stripped of
// surrounding whitespace. An empty template means DefaultSlackTemplate.
// Values are rendered with Python's str(); the caller truncates the message to
// 3000 characters. Unsupported syntax yields ErrInvalidTemplate.
func RenderSlackTemplate(template string, fields map[string]any) (string, error) {
	if template == "" {
		template = DefaultSlackTemplate
	}
	parts, err := parseSlackTemplate(template)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	for _, p := range parts {
		if !p.field {
			out.WriteString(p.text)
			continue
		}
		v, ok := fields[p.text]
		if !ok || v == nil {
			continue
		}
		out.WriteString(colgraph.PyStr(v))
	}
	return colgraph.PyStrip(out.String()), nil
}
