// Package sourcewb is the source_workbook job and the source preview: ports of
// services/workbook/source_engine.py (materialize_source, handle_source_workbook,
// preview_source), crm_import.py and people_search.py. A workbook `source` column
// materializes new rows from a collection query (the leadgen collector), a CRM
// contact import, or LinkedIn people discovery, publishing row_added and
// source_done events. The collector, the lead quality gate, the account-fit
// evaluator and the people finder are interfaces: their implementations live in
// internal/collect and internal/leadpeople (plan 15 wave 3).
package sourcewb

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"unicode"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// plain converts decoded workbook JSON (ordered *wbcrud.Obj, json.Number) into
// plain Go values (map[string]any, int64/float64) for the Python-semantics helpers.
func plain(v any) any {
	switch x := v.(type) {
	case *wbcrud.Obj:
		if x == nil {
			return nil
		}
		m := make(map[string]any, x.Len())
		for _, k := range x.Keys() {
			e, _ := x.Get(k)
			m[k] = plain(e)
		}
		return m
	case wbcrud.Column:
		m := map[string]any{}
		for _, k := range x.Keys() {
			m[k] = plain(x.Get(k))
		}
		return m
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = plain(e)
		}
		return out
	case json.Number:
		s := string(x)
		if !strings.ContainsAny(s, ".eE") {
			if i, err := x.Int64(); err == nil {
				return i
			}
		}
		f, _ := x.Float64()
		return f
	}
	return v
}

func pyStr(v any) string    { return manifest.PyStr(plain(v)) }
func truthy(v any) bool     { return manifest.PyTruthy(plain(v)) }
func strip(s string) string { return manifest.PyStrip(s) }

func isSpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

// orStr is `str(v or "")`.
func orStr(v any) string {
	if !truthy(v) {
		return ""
	}
	return pyStr(v)
}

// iterate is Python iteration over a JSON value: list items, string characters.
func iterate(v any) []any {
	switch x := plain(v).(type) {
	case []any:
		return x
	case string:
		out := make([]any, 0, len(x))
		for _, r := range x {
			out = append(out, string(r))
		}
		return out
	case map[string]any: // dict iteration yields keys (order is not recoverable here)
		out := make([]any, 0, len(x))
		for k := range x {
			out = append(out, k)
		}
		return out
	}
	return nil
}

// pyInt is int(v); ok is false where Python raises.
func pyInt(v any) (int64, bool) {
	switch x := plain(v).(type) {
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case int64:
		return x, true
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return 0, false
		}
		return int64(math.Trunc(x)), true
	case string:
		s := strings.ReplaceAll(strip(x), "_", "")
		i, err := strconv.ParseInt(s, 10, 64)
		return i, err == nil
	}
	return 0, false
}

// get is dict.get on a column / nested object.
func get(o interface{ Get(string) (any, bool) }, k string) any {
	if o == nil {
		return nil
	}
	v, _ := o.Get(k)
	return v
}

func asObj(v any) (*wbcrud.Obj, bool) {
	o, ok := v.(*wbcrud.Obj)
	return o, ok && o != nil
}

// obj builds an ordered object from key/value pairs.
func obj(kv ...any) *wbcrud.Obj {
	o := wbcrud.NewObj()
	for i := 0; i+1 < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}
