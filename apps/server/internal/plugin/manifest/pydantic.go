package manifest

import (
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/pycompat"
)

// Issue is one field error, shaped like a pydantic ErrorDetails entry.
type Issue struct {
	Loc  []any  `json:"loc"`
	Type string `json:"type"`
	Msg  string `json:"msg"`
}

// LocString renders a location the way pydantic prints it ("request.url").
func (i Issue) LocString() string {
	parts := make([]string, len(i.Loc))
	for j, p := range i.Loc {
		if s, ok := p.(string); ok {
			parts[j] = s
		} else {
			parts[j] = pycompat.Str(p)
		}
	}
	return strings.Join(parts, ".")
}

// ModelError mirrors pydantic.ValidationError.
type ModelError struct {
	Model  string
	Issues []Issue
}

func (e *ModelError) Error() string {
	var b strings.Builder
	n := len(e.Issues)
	plural := "s"
	if n == 1 {
		plural = ""
	}
	fmt.Fprintf(&b, "%d validation error%s for %s", n, plural, e.Model)
	for _, is := range e.Issues {
		fmt.Fprintf(&b, "\n%s\n  %s [type=%s]", is.LocString(), is.Msg, is.Type)
	}
	return b.String()
}

// validator accumulates issues while walking a raw manifest.
type validator struct{ issues []Issue }

func (v *validator) add(loc []any, typ, msg string) {
	v.issues = append(v.issues, Issue{Loc: append([]any(nil), loc...), Type: typ, Msg: msg})
}

func at(loc []any, k any) []any { return append(append([]any(nil), loc...), k) }

// str mirrors a pydantic `str` field in lax mode (bytes are decoded).
func (v *validator) str(loc []any, raw any) (string, bool) {
	switch t := raw.(type) {
	case string:
		return t, true
	case pycompat.Bytes:
		if utf8.Valid(t) {
			return string(t), true
		}
		v.add(loc, "string_unicode", "Input should be a valid string, unable to parse raw data as a unicode string")
		return "", false
	}
	v.add(loc, "string_type", "Input should be a valid string")
	return "", false
}

// optStr mirrors Optional[str].
func (v *validator) optStr(loc []any, raw any) (*string, bool) {
	if raw == nil {
		return nil, true
	}
	s, ok := v.str(loc, raw)
	if !ok {
		return nil, false
	}
	return &s, true
}

var pyFloatRE = regexp.MustCompile(`^[+-]?(?:[0-9]+\.?[0-9]*(?:[eE][+-]?[0-9]+)?|\.[0-9]+(?:[eE][+-]?[0-9]+)?|(?i:inf|infinity|nan))$`)

// parseFloatStr mirrors pydantic-core's str -> float coercion.
func parseFloatStr(s string) (float64, bool) {
	s = pycompat.Strip(s)
	if strings.Contains(s, "_") {
		if strings.HasPrefix(s, "_") || strings.HasSuffix(s, "_") || strings.Contains(s, "__") {
			return 0, false
		}
		s = strings.ReplaceAll(s, "_", "")
	}
	if !pyFloatRE.MatchString(s) {
		return 0, false
	}
	l := strings.ToLower(strings.TrimLeft(s, "+-"))
	neg := strings.HasPrefix(s, "-")
	switch l {
	case "nan":
		return math.NaN(), true
	case "inf", "infinity":
		if neg {
			return math.Inf(-1), true
		}
		return math.Inf(1), true
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
			return f, true
		}
		return 0, false
	}
	return f, true
}

// float mirrors a pydantic `float` field in lax mode.
func (v *validator) float(loc []any, raw any) (float64, bool) {
	switch t := raw.(type) {
	case float64:
		return t, true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case int64:
		return float64(t), true
	case *big.Int:
		f, acc := new(big.Float).SetInt(t).Float64()
		if math.IsInf(f, 0) || (acc != big.Exact && math.IsInf(f, 0)) {
			v.add(loc, "float_type", "Input should be a valid number")
			return 0, false
		}
		return f, true
	case string:
		if f, ok := parseFloatStr(t); ok {
			return f, true
		}
		v.add(loc, "float_parsing", "Input should be a valid number, unable to parse string as a number")
		return 0, false
	case pycompat.Bytes:
		if f, ok := parseFloatStr(string(t)); ok {
			return f, true
		}
		v.add(loc, "float_parsing", "Input should be a valid number, unable to parse string as a number")
		return 0, false
	}
	v.add(loc, "float_type", "Input should be a valid number")
	return 0, false
}

// strList mirrors List[str] (list, tuple and set inputs are accepted).
func (v *validator) strList(loc []any, raw any) ([]string, bool) {
	var items []any
	switch t := raw.(type) {
	case []any:
		items = t
	case pycompat.Tuple:
		items = t
	case pycompat.Set:
		items = t
	default:
		v.add(loc, "list_type", "Input should be a valid list")
		return nil, false
	}
	out := make([]string, 0, len(items))
	ok := true
	for i, it := range items {
		s, good := v.str(at(loc, int64(i)), it)
		if !good {
			ok = false
			continue
		}
		out = append(out, s)
	}
	return out, ok
}

// strMap mirrors Dict[str, Any] (valueStr=false) or Dict[str, str].
func (v *validator) strMap(loc []any, raw any, valueStr bool) (*pycompat.Map, bool) {
	m, isMap := raw.(*pycompat.Map)
	if !isMap {
		v.add(loc, "dict_type", "Input should be a valid dictionary")
		return nil, false
	}
	out := pycompat.NewMap()
	ok := true
	for _, e := range m.Entries() {
		key, good := v.strKey(at(loc, e.Key), e.Key)
		if !good {
			ok = false
		}
		val := e.Value
		if valueStr {
			s, good := v.str(at(loc, e.Key), e.Value)
			if !good {
				ok = false
			}
			val = s
		}
		if ok {
			_ = out.Set(key, val)
		}
	}
	return out, ok
}

func (v *validator) strKey(loc []any, raw any) (string, bool) {
	switch t := raw.(type) {
	case string:
		return t, true
	case pycompat.Bytes:
		if utf8.Valid(t) {
			return string(t), true
		}
	}
	v.add(at(loc, "[key]"), "string_type", "Input should be a valid string")
	return "", false
}

// literal mirrors Literal[...] over strings.
func (v *validator) literal(loc []any, raw any, allowed ...string) (string, bool) {
	if s, ok := raw.(string); ok {
		for _, a := range allowed {
			if s == a {
				return s, true
			}
		}
	}
	quoted := make([]string, len(allowed))
	for i, a := range allowed {
		quoted[i] = "'" + a + "'"
	}
	var msg string
	switch len(quoted) {
	case 1:
		msg = "Input should be " + quoted[0]
	default:
		msg = "Input should be " + strings.Join(quoted[:len(quoted)-1], ", ") + " or " + quoted[len(quoted)-1]
	}
	v.add(loc, "literal_error", msg)
	return "", false
}

// model mirrors a nested BaseModel field: only dicts are accepted.
func (v *validator) model(loc []any, raw any, name string) (*pycompat.Map, bool) {
	m, ok := raw.(*pycompat.Map)
	if !ok {
		v.add(loc, "model_type", "Input should be a valid dictionary or instance of "+name)
		return nil, false
	}
	return m, true
}

// field fetches a string-keyed field.
func field(m *pycompat.Map, name string) (any, bool) {
	if m == nil {
		return nil, false
	}
	return m.Get(name)
}
