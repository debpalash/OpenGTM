package autopilot

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"unicode"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// Python value rules over decoded JSON trees. A plan is an ordered tree: *wbcrud.Obj objects, []any arrays,
// json.Number / int64 / float64 numbers, strings, bools and nil.

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int64:
		return x != 0
	case int:
		return x != 0
	case float64:
		return x != 0
	case json.Number:
		f, err := strconv.ParseFloat(x.String(), 64)
		return err != nil || f != 0
	case []any:
		return len(x) > 0
	case *wbcrud.Obj:
		return x.Len() > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
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
	s, err := canonjson.FloatRepr(f)
	if err != nil {
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	return s
}

// str is Python str() of a JSON value.
func str(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case string:
		return x
	case bool:
		if x {
			return "True"
		}
		return "False"
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return strconv.Itoa(x)
	case float64:
		return pyFloat(x)
	case json.Number:
		s := x.String()
		if strings.ContainsAny(s, ".eE") {
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				return pyFloat(f)
			}
			return s
		}
		if s == "-0" {
			return "0"
		}
		return s
	}
	return repr(v)
}

// repr is Python repr() of a JSON value.
func repr(v any) string {
	switch x := v.(type) {
	case string:
		q := "'"
		if strings.Contains(x, "'") && !strings.Contains(x, `"`) {
			q = `"`
		}
		s := strings.NewReplacer(`\`, `\\`, "\n", `\n`, "\r", `\r`, "\t", `\t`).Replace(x)
		if q == "'" {
			s = strings.ReplaceAll(s, "'", `\'`)
		}
		return q + s + q
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = repr(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case *wbcrud.Obj:
		keys := x.Keys()
		parts := make([]string, len(keys))
		for i, k := range keys {
			e, _ := x.Get(k)
			parts[i] = repr(k) + ": " + repr(e)
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return str(v)
}

// typeName is type(v).__name__ for a JSON value.
func typeName(v any) string {
	switch x := v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case string:
		return "str"
	case int64, int:
		return "int"
	case float64:
		return "float"
	case json.Number:
		if strings.ContainsAny(x.String(), ".eE") {
			return "float"
		}
		return "int"
	case []any:
		return "list"
	}
	return "dict"
}

func isPySpace(r rune) bool {
	return unicode.IsSpace(r) || r == '\x1c' || r == '\x1d' || r == '\x1e' || r == '\x1f'
}

func strip(s string) string { return strings.TrimFunc(s, isPySpace) }

// IntOf is Python int(v) for a JSON value; err is the ValueError/TypeError text Python raises.
func IntOf(v any) (int64, string) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, ""
		}
		return 0, ""
	case int64:
		return x, ""
	case int:
		return int64(x), ""
	case float64:
		return floatInt(x)
	case json.Number:
		s := x.String()
		if strings.ContainsAny(s, ".eE") {
			f, err := strconv.ParseFloat(s, 64)
			if err != nil {
				return 0, "cannot convert float infinity to integer"
			}
			return floatInt(f)
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, "integer out of range"
		}
		return n, ""
	case string:
		if n, ok := parsePyIntLiteral(strip(x)); ok {
			return n, ""
		}
		return 0, "invalid literal for int() with base 10: " + repr(x)
	case nil:
		return 0, "int() argument must be a string, a bytes-like object or a real number, not 'NoneType'"
	}
	return 0, "int() argument must be a string, a bytes-like object or a real number, not '" + typeName(v) + "'"
}

func floatInt(f float64) (int64, string) {
	switch {
	case math.IsNaN(f):
		return 0, "cannot convert float NaN to integer"
	case math.IsInf(f, 0):
		return 0, "cannot convert float infinity to integer"
	case f >= 9.223372036854775807e18 || f < -9.223372036854775808e18:
		return 0, "integer out of range"
	}
	return int64(f), ""
}

// parsePyIntLiteral accepts Python's base-10 int() literal: optional sign, digits, single underscores
// between digits.
func parsePyIntLiteral(s string) (int64, bool) {
	body := s
	if strings.HasPrefix(body, "+") || strings.HasPrefix(body, "-") {
		body = body[1:]
	}
	if body == "" || body[0] == '_' || body[len(body)-1] == '_' || strings.Contains(body, "__") {
		return 0, false
	}
	for _, r := range body {
		if r != '_' && (r < '0' || r > '9') {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(strings.ReplaceAll(s, "_", ""), 10, 64)
	return n, err == nil
}

// get reads key from an object value; ok=false when v is not an object.
func get(v any, key string) (any, bool) {
	o, isObj := v.(*wbcrud.Obj)
	if !isObj {
		return nil, false
	}
	x, _ := o.Get(key)
	return x, true
}

// lookup is o.get(key, dflt) on an object.
func lookup(o *wbcrud.Obj, key string, dflt any) any {
	if v, ok := o.Get(key); ok {
		return v
	}
	return dflt
}
