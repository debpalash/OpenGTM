package rowtmpl

import (
	"errors"
	"regexp"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
)

// Bounds (Python is unbounded; documented deviation D4).
const (
	MaxTemplateBytes = 64 << 10
	MaxPlaceholders  = 256
	MaxOutputBytes   = 1 << 20
)

// ErrTooLarge is returned when a template or its output exceeds the bounds.
var ErrTooLarge = errors.New("template too large")

// placeholder is re.compile(r'\{([^}]+)\}'). RE2 is leftmost-first like
// Python's engine and the pattern has no alternation or nesting, so matches
// are identical.
var placeholder = regexp.MustCompile(`\{([^}]+)\}`)

func checkTemplate(tmpl string) ([][]int, error) {
	if len(tmpl) > MaxTemplateBytes {
		return nil, ErrTooLarge
	}
	ms := placeholder.FindAllStringSubmatchIndex(tmpl, MaxPlaceholders+1)
	if len(ms) > MaxPlaceholders {
		return nil, ErrTooLarge
	}
	return ms, nil
}

// ValidateTemplateReferences is validate_template_references: an ambiguous
// display reference is rejected before the flat map loses identity.
func ValidateTemplateReferences(tmpl string, cols []colgraph.Col) error {
	ms, err := checkTemplate(tmpl)
	if err != nil {
		return err
	}
	for _, m := range ms {
		ref := pyStrip(tmpl[m[2]:m[3]])
		if len(colgraph.ReferenceIndexes(ref, cols)) > 1 {
			return errors.New("Ambiguous column reference: " + ref + ". Use an exact column ID.")
		}
	}
	return nil
}

func ambiguous(key string) error {
	return errors.New("Ambiguous column reference: " + key + ". Use an exact column ID.")
}

// ResolvePrompt is _resolve_prompt: exact key, then case-insensitive, then
// case-insensitive with underscores as spaces; several distinct candidate
// texts are ambiguous.
func ResolvePrompt(tmpl string, values map[string]string, strict bool) (string, error) {
	ms, err := checkTemplate(tmpl)
	if err != nil {
		return "", err
	}
	if len(ms) == 0 {
		return tmpl, nil
	}
	var lowered, normalized map[string]string
	var loweredAmb, normAmb map[string]bool
	build := func() {
		if lowered != nil {
			return
		}
		lowered, normalized = map[string]string{}, map[string]string{}
		loweredAmb, normAmb = map[string]bool{}, map[string]bool{}
		add := func(m map[string]string, amb map[string]bool, k, v string) {
			if old, ok := m[k]; ok && old != v {
				amb[k] = true
			}
			m[k] = v
		}
		for k, v := range values {
			l := pyLower(k)
			add(lowered, loweredAmb, l, v)
			add(normalized, normAmb, strings.ReplaceAll(l, "_", " "), v)
		}
	}
	var out strings.Builder
	last := 0
	for _, m := range ms {
		out.WriteString(tmpl[last:m[0]])
		last = m[1]
		key := pyStrip(tmpl[m[2]:m[3]])
		var rep string
		if v, ok := values[key]; ok {
			rep = v
		} else {
			build()
			kl := pyLower(key)
			if loweredAmb[kl] {
				return "", ambiguous(key)
			}
			if v, ok := lowered[kl]; ok {
				rep = v
			} else {
				kn := strings.ReplaceAll(kl, "_", " ")
				if normAmb[kn] {
					return "", ambiguous(key)
				}
				if v, ok := normalized[kn]; ok {
					rep = v
				} else if strict {
					return "", errors.New("Unknown column reference: " + key + ". Fix the template before sending.")
				} else {
					rep = "[" + key + ": not found]"
				}
			}
		}
		out.WriteString(rep)
		if out.Len() > MaxOutputBytes {
			return "", ErrTooLarge
		}
	}
	out.WriteString(tmpl[last:])
	if out.Len() > MaxOutputBytes {
		return "", ErrTooLarge
	}
	return out.String(), nil
}

// Resolve is output._resolve: validate references, build lead values, resolve
// strictly.
func Resolve(tmpl string, leadData map[string]any, cols []colgraph.Col) (string, error) {
	if err := ValidateTemplateReferences(tmpl, cols); err != nil {
		return "", err
	}
	return ResolvePrompt(tmpl, LeadValues(leadData, cols), true)
}

// ResolveDeep is output._resolve_deep: resolves placeholders in the string
// values of a dict/list/str (keys untouched). colgraph.Col dicts keep key order
// and stay Col; map[string]any stays a map; other values pass through.
func ResolveDeep(v any, leadData map[string]any, cols []colgraph.Col) (any, error) {
	switch t := v.(type) {
	case string:
		return Resolve(t, leadData, cols)
	case colgraph.Col:
		out := colgraph.Col{Keys: append([]string(nil), t.Keys...), Vals: make(map[string]any, len(t.Vals))}
		for _, k := range t.Keys {
			r, err := ResolveDeep(t.Vals[k], leadData, cols)
			if err != nil {
				return nil, err
			}
			out.Vals[k] = r
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			r, err := ResolveDeep(e, leadData, cols)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			r, err := ResolveDeep(e, leadData, cols)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	}
	return v, nil
}
