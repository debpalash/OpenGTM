package rowtmpl

import (
	"encoding/json"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
)

// CanonNumbers returns v with every json.Number leaf rewritten to the text Python's json.dumps
// writes for the parsed value (1.50 -> 1.5, 1e2 -> 100.0, -0 -> 0; integers of any size keep their
// digits). Ordered objects, lists and plain maps are rebuilt; v is not modified. A number Go cannot
// parse as a finite float is left as written.
func CanonNumbers(v any) any {
	switch t := v.(type) {
	case json.Number:
		s := numberStr(t)
		if s == "inf" || s == "-inf" || s == "nan" {
			return t
		}
		return json.Number(s)
	case colgraph.Col:
		vals := make(map[string]any, len(t.Vals))
		for k, e := range t.Vals {
			vals[k] = CanonNumbers(e)
		}
		return colgraph.Col{Keys: t.Keys, Vals: vals}
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = CanonNumbers(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = CanonNumbers(e)
		}
		return out
	}
	return v
}
