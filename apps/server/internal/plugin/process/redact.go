package process

import (
	"strings"
	"unicode/utf8"
)

// redactAny applies fn to every string in a JSON-like value (map keys are left
// alone) and returns a copy. It is used to keep declared secret values out of
// everything a plugin returns: fields, evidence, details and messages.
func redactAny(v any, fn func(string) string) any {
	switch t := v.(type) {
	case string:
		return fn(t)
	case map[string]any:
		return redactMap(t, fn)
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = redactAny(x, fn)
		}
		return out
	}
	return v
}

func redactMap(m map[string]any, fn func(string) string) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = redactAny(v, fn)
	}
	return out
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = s[:max]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
		s += "..."
	}
	return s
}
