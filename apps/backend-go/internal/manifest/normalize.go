package manifest

import (
	"strings"
	"unicode"
)

const e164MaxDigits = 15

// normalizePhone is the best-effort phone normalizer from normalize.py: E.164
// when the input carries "+" or "00", separator-stripped otherwise; non-strings
// and unparseable input pass through unchanged. Python's \d is any Unicode
// decimal digit (unicode.IsDigit == category Nd).
func normalizePhone(value any) any {
	str, ok := value.(string)
	if !ok {
		return value
	}
	s := pyStrip(str)
	if s == "" {
		return value
	}
	hadPlus := strings.HasPrefix(s, "+")
	digits := []rune(strings.Map(func(r rune) rune {
		if unicode.IsDigit(r) {
			return r
		}
		return -1
	}, s))
	if len(digits) == 0 || len(digits) > e164MaxDigits+2 {
		return value
	}
	if hadPlus {
		if len(digits) <= e164MaxDigits {
			return "+" + string(digits)
		}
		return value
	}
	if len(digits) > 8 && digits[0] == '0' && digits[1] == '0' {
		rest := digits[2:]
		if len(rest) > e164MaxDigits {
			rest = rest[:e164MaxDigits]
		}
		return "+" + string(rest)
	}
	return string(digits)
}

var fieldNormalizers = []struct {
	name string
	fn   func(any) any
}{{"phone", normalizePhone}, {"mobile_phone", normalizePhone}}

// Normalize applies per-field normalizers; unknown fields pass through. The
// input map is not mutated.
func Normalize(fields map[string]any) map[string]any {
	if len(fields) == 0 {
		return fields
	}
	out := make(map[string]any, len(fields))
	for k, v := range fields {
		out[k] = v
	}
	for _, n := range fieldNormalizers {
		if v, ok := out[n.name]; ok {
			out[n.name] = n.fn(v)
		}
	}
	return out
}
