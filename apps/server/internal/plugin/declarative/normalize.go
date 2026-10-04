package declarative

import (
	"strings"
	"unicode"

	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/pycompat"
)

// e164MaxDigits mirrors normalize.py: "+" then up to 15 digits.
const e164MaxDigits = 15

// NormalizePhone ports normalize.normalize_phone: E.164 when the input
// carries an international prefix ("+" or "00"), separator-stripping
// otherwise; non-strings and non-phone-shaped strings pass through.
func NormalizePhone(value any) any {
	s, ok := value.(string)
	if !ok {
		return value
	}
	t := pycompat.Strip(s)
	if t == "" {
		return value
	}
	hadPlus := strings.HasPrefix(t, "+")
	var digits strings.Builder
	for _, r := range t {
		if unicode.IsDigit(r) { // Python's \d is Unicode-aware
			digits.WriteRune(r)
		}
	}
	d := []rune(digits.String())
	if len(d) == 0 || len(d) > e164MaxDigits+2 {
		return value
	}
	if hadPlus {
		return "+" + string(d[:min(len(d), e164MaxDigits)])
	}
	if strings.HasPrefix(string(d), "00") && len(d) > 8 {
		end := min(len(d), 2+e164MaxDigits)
		return "+" + string(d[2:end])
	}
	return string(d)
}

// fieldNormalizers mirrors FIELD_NORMALIZERS.
var fieldNormalizers = map[string]func(any) any{
	"phone":        NormalizePhone,
	"mobile_phone": NormalizePhone,
}

// NormalizeFields ports normalize.normalize_fields.
func NormalizeFields(fields *pycompat.Map) *pycompat.Map {
	if fields.Len() == 0 {
		return fields
	}
	out := fields.Copy()
	for _, name := range []string{"phone", "mobile_phone"} {
		if v, ok := out.Get(name); ok {
			_ = out.Set(name, fieldNormalizers[name](v))
		}
	}
	return out
}
