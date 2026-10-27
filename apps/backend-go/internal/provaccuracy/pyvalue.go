package provaccuracy

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// pyFloatRepr is repr(float) as json.dumps writes it.
func pyFloatRepr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	if f == 0 {
		if math.Signbit(f) {
			return "-0.0"
		}
		return "0.0"
	}
	e := strconv.FormatFloat(f, 'e', -1, 64) // d.ddde+XX
	mant, expS, _ := strings.Cut(e, "e")
	exp, _ := strconv.Atoi(expS)
	if exp < -4 || exp >= 16 {
		sign := "+"
		if exp < 0 {
			sign = "-"
			exp = -exp
		}
		es := strconv.Itoa(exp)
		if len(es) < 2 {
			es = "0" + es
		}
		return mant + "e" + sign + es
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

// pyStr is str(v) for a JSON-decoded value (OMap and []any print as a Python repr).
func pyStr(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		s := string(x)
		if !strings.ContainsAny(s, ".eE") {
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				return strconv.FormatInt(n, 10)
			}
			return s
		}
		f, _ := strconv.ParseFloat(s, 64)
		return pyFloatRepr(f)
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
		q := "'"
		if strings.Contains(x, "'") && !strings.Contains(x, `"`) {
			q = `"`
		}
		r := strings.NewReplacer(`\`, `\\`, "\n", `\n`, "\r", `\r`, "\t", `\t`, q, `\`+q)
		return q + r.Replace(x) + q
	case json.Number:
		return pyStr(x)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = pyRepr(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case *OMap:
		parts := make([]string, 0, x.Len())
		for _, k := range x.Keys() {
			val, _ := x.Get(k)
			parts = append(parts, pyRepr(k)+": "+pyRepr(val))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return ""
}
