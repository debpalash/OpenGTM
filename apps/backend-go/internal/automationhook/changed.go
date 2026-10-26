package automationhook

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// Change is one changed cell: events.on_rows_changed's {row_id, field, old, new, cell_version}.
type Change struct {
	RowID       int64
	Field       string
	Old, New    any
	CellVersion string
}

func (h *hook) RowsChanged(ctx context.Context, tx pgx.Tx, workspaceID, workbookID string, changes []Change) (int, error) {
	if !h.enabled || len(changes) == 0 {
		return 0, nil
	}
	return h.guarded(ctx, tx, "rows-changed", workspaceID, func(sp pgx.Tx) (int, error) {
		q := gen.New(sp)
		rules, err := q.ListEnabledTriggersWithConfig(ctx, gen.ListEnabledTriggersWithConfigParams{
			WorkspaceID: workspaceID, TriggerType: "on_row_changed"})
		if err != nil {
			return 0, err
		}
		n := 0
		for _, rule := range rules {
			if !covers(rule.ScopeWorkbookIds, workbookID) {
				continue
			}
			watch, ok := configSet(rule.TriggerConfig, "watch_fields")
			if !ok { // malformed config: fail closed (Python's emit aborts on the TypeError)
				h.log.Warn("automation rule skipped: malformed watch_fields", "workspace_id", workspaceID, "trigger_id", rule.ID)
				continue
			}
			for _, ch := range changes {
				if len(watch) > 0 && !watch[ch.Field] {
					continue
				}
				rid := strconv.FormatInt(ch.RowID, 10)
				key := "rowchg:" + rid + ":" + ch.Field + ":" + sha8(ch.Old) + "->" + sha8(ch.New) + ":" + ch.CellVersion
				if err := h.enqueue(ctx, q, workspaceID, rule.ID, []target{{WorkbookID: workbookID, RowID: rid}}, "row_changed", key); err != nil {
					return 0, err
				}
				n++
			}
		}
		return n, nil
	}), nil
}

// configSet is set((cfg or {}).get(key) or []) for a JSON trigger_config. ok is false when
// Python would raise (non-object config, non-list truthy value, unhashable element) or the
// value is a truthy string/number (a char-set / TypeError in Python that can never be a
// meaningful filter): callers skip the rule, i.e. fail closed. Falsy values mean "no filter".
func configSet(cfg []byte, key string) (set map[string]bool, ok bool) {
	set = map[string]bool{}
	var c any
	if len(bytes.TrimSpace(cfg)) == 0 {
		return set, true
	}
	if json.Unmarshal(cfg, &c) != nil {
		return nil, false
	}
	if jsonFalsy(c) {
		return set, true
	}
	obj, isObj := c.(map[string]any)
	if !isObj {
		return nil, false
	}
	v := obj[key]
	if jsonFalsy(v) {
		return set, true
	}
	list, isList := v.([]any)
	if !isList {
		return nil, false
	}
	for _, e := range list {
		switch x := e.(type) {
		case string:
			set[x] = true
		case float64, bool, nil:
			// hashable in Python but never equal to a string id: kept out of the set, so a list of
			// only these yields a non-empty "wanted" that matches nothing.
			set["\x00non-string"] = true
		default:
			return nil, false // list/dict element: TypeError in Python
		}
	}
	return set, true
}

func jsonFalsy(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case float64:
		return x == 0
	case bool:
		return !x
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

// sha8 is events._sha8: first 8 hex chars of sha1(str(v if v is not None else "")).
func sha8(v any) string {
	sum := sha1.Sum([]byte(pyStr(v)))
	return hex.EncodeToString(sum[:])[:8]
}

// pyStr renders a decoded JSON value like Python's str() of the same value (None -> "").
// Decode with UseNumber so ints and floats stay distinguishable. Dict key order is sorted
// (Go maps carry no insertion order).
func pyStr(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
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
		return pyQuote(x)
	case int:
		return strconv.FormatInt(int64(x), 10)
	case int32:
		return strconv.FormatInt(int64(x), 10)
	case int64:
		return strconv.FormatInt(x, 10)
	case float32:
		return pyFloat(float64(x))
	case float64:
		return pyFloat(x)
	case json.Number:
		s := x.String()
		if strings.ContainsAny(s, ".eE") {
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				return pyFloat(f)
			}
		}
		return s
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = pyRepr(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = pyQuote(k) + ": " + pyRepr(x[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmtFallback(v)
}

func fmtFallback(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// pyFloat is Python's repr(float): shortest round-trip digits, fixed notation when
// -4 < decimal-point-position <= 16, else d[.ddd]e±XX.
func pyFloat(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	case math.IsNaN(f):
		return "nan"
	}
	s := strconv.FormatFloat(f, 'e', -1, 64) // [-]d[.ddd]e±XX
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	mant, expS, _ := strings.Cut(s, "e")
	exp, _ := strconv.Atoi(expS)
	digits := strings.Replace(mant, ".", "", 1)
	decpt := exp + 1
	var out string
	switch {
	case -4 < decpt && decpt <= 16:
		switch {
		case decpt <= 0:
			out = "0." + strings.Repeat("0", -decpt) + digits
		case decpt >= len(digits):
			out = digits + strings.Repeat("0", decpt-len(digits)) + ".0"
		default:
			out = digits[:decpt] + "." + digits[decpt:]
		}
	default:
		out = digits[:1]
		if len(digits) > 1 {
			out += "." + digits[1:]
		}
		sign := "+"
		if exp < 0 {
			sign, exp = "-", -exp
		}
		e := strconv.Itoa(exp)
		if len(e) < 2 {
			e = "0" + e
		}
		out += "e" + sign + e
	}
	if neg {
		out = "-" + out
	}
	return out
}

// pyQuote is Python's repr(str).
func pyQuote(s string) string {
	q := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		q = '"'
	}
	var b strings.Builder
	b.WriteByte(q)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		switch {
		case r == rune(q) || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			b.WriteString(`\x` + hex2(int(r)))
		case r == utf8.RuneError && size == 1:
			b.WriteString(`�`)
		case unicode.IsPrint(r):
			b.WriteRune(r)
		case r <= 0xff:
			b.WriteString(`\x` + hex2(int(r)))
		case r <= 0xffff:
			b.WriteString(`\u` + strings.Repeat("0", 4-len(strconv.FormatInt(int64(r), 16))) + strconv.FormatInt(int64(r), 16))
		default:
			h := strconv.FormatInt(int64(r), 16)
			b.WriteString(`\U` + strings.Repeat("0", 8-len(h)) + h)
		}
	}
	b.WriteByte(q)
	return b.String()
}

func hex2(n int) string {
	h := strconv.FormatInt(int64(n), 16)
	if len(h) < 2 {
		h = "0" + h
	}
	return h
}
