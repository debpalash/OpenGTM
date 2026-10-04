// Package pycompat reproduces the small slice of Python behavior that plugin
// manifests depend on, so the Go host makes byte-for-byte the same decisions as
// the legacy Python connector SDK:
//
//   - LoadYAML mirrors PyYAML's yaml.safe_load (YAML 1.1 implicit typing:
//     yes/no/on/off booleans, 0-prefixed octal, sexagesimal numbers, dates,
//     merge keys, duplicate keys where the last value wins).
//   - Dumps mirrors json.dumps (sort_keys, separators, ensure_ascii, indent,
//     float repr, NaN/Infinity, TypeError on non-JSON values).
//   - LoadJSON mirrors json.loads (NaN/Infinity, arbitrary-precision ints).
//   - Str and Repr mirror str()/repr() for JSON-like values, which the template
//     renderer needs when it interpolates non-string values.
//
// Values use these Go types:
//
//	nil, bool, int64, *big.Int (ints outside int64), float64, string,
//	[]any (list/tuple), *Map (dict, insertion ordered), Set, Bytes, Timestamp.
package pycompat

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"time"
)

// Map is an insertion-ordered dict with Python key-equality semantics
// (1 == 1.0 == True share one slot; the first key object is kept).
type Map struct {
	keys  []any
	vals  []any
	index map[string]int
}

// NewMap returns an empty Map.
func NewMap() *Map { return &Map{index: map[string]int{}} }

// Set inserts or replaces a value. Keys must be hashable scalars.
func (m *Map) Set(k, v any) error {
	h, err := hashKey(k)
	if err != nil {
		return err
	}
	if i, ok := m.index[h]; ok {
		m.vals[i] = v
		return nil
	}
	m.index[h] = len(m.keys)
	m.keys = append(m.keys, k)
	m.vals = append(m.vals, v)
	return nil
}

// Get looks a key up with Python equality semantics.
func (m *Map) Get(k any) (any, bool) {
	if m == nil {
		return nil, false
	}
	h, err := hashKey(k)
	if err != nil {
		return nil, false
	}
	i, ok := m.index[h]
	if !ok {
		return nil, false
	}
	return m.vals[i], true
}

// GetString is Get for a string key.
func (m *Map) GetString(k string) (any, bool) { return m.Get(k) }

// Has reports whether k is present.
func (m *Map) Has(k any) bool { _, ok := m.Get(k); return ok }

// Delete removes k, preserving the order of the remaining keys.
func (m *Map) Delete(k any) {
	h, err := hashKey(k)
	if err != nil {
		return
	}
	i, ok := m.index[h]
	if !ok {
		return
	}
	m.keys = append(m.keys[:i:i], m.keys[i+1:]...)
	m.vals = append(m.vals[:i:i], m.vals[i+1:]...)
	delete(m.index, h)
	for j := i; j < len(m.keys); j++ {
		hk, _ := hashKey(m.keys[j])
		m.index[hk] = j
	}
}

// Len returns the number of entries.
func (m *Map) Len() int {
	if m == nil {
		return 0
	}
	return len(m.keys)
}

// Keys returns the keys in insertion order.
func (m *Map) Keys() []any { return append([]any(nil), m.keys...) }

// Entry is one key/value pair.
type Entry struct {
	Key, Value any
}

// Entries returns the pairs in insertion order.
func (m *Map) Entries() []Entry {
	if m == nil {
		return nil
	}
	out := make([]Entry, len(m.keys))
	for i := range m.keys {
		out[i] = Entry{m.keys[i], m.vals[i]}
	}
	return out
}

// Copy returns a shallow copy.
func (m *Map) Copy() *Map {
	n := NewMap()
	for i := range m.keys {
		_ = n.Set(m.keys[i], m.vals[i])
	}
	return n
}

// Tuple is a Python tuple (YAML !!omap/!!pairs items). JSON encodes it as a list.
type Tuple []any

// Set is a Python set built from a YAML !!set (keys only, insertion ordered).
type Set []any

// Bytes is a Python bytes value (YAML !!binary).
type Bytes []byte

// Timestamp is a PyYAML date (DateOnly) or datetime value.
type Timestamp struct {
	Time     time.Time
	DateOnly bool
	Aware    bool
}

// unhashableError mirrors Python's TypeError for list/dict/set keys.
type unhashableError struct{ typ string }

func (e unhashableError) Error() string { return "unhashable type: '" + e.typ + "'" }

var nanCounter uint64

func hashKey(k any) (string, error) {
	switch v := k.(type) {
	case nil:
		return "N", nil
	case bool:
		if v {
			return "i1", nil
		}
		return "i0", nil
	case int64:
		return "i" + strconv.FormatInt(v, 10), nil
	case int:
		return "i" + strconv.Itoa(v), nil
	case *big.Int:
		return "i" + v.String(), nil
	case float64:
		if math.IsNaN(v) {
			nanCounter++ // NaN never equals another NaN: give it its own slot.
			return fmt.Sprintf("nan%d", nanCounter), nil
		}
		if !math.IsInf(v, 0) && v == math.Trunc(v) {
			bf := new(big.Float).SetFloat64(v)
			bi, _ := bf.Int(nil)
			return "i" + bi.String(), nil
		}
		return "f" + strconv.FormatUint(math.Float64bits(v), 16), nil
	case string:
		return "s" + v, nil
	case Bytes:
		return "b" + string(v), nil
	case Timestamp:
		return "t" + v.Time.String() + strconv.FormatBool(v.DateOnly), nil
	default:
		return "", unhashableError{TypeName(k)}
	}
}

// TypeName returns the Python type name of a value.
func TypeName(v any) string {
	switch t := v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case int64, int, *big.Int:
		return "int"
	case float64:
		return "float"
	case string:
		return "str"
	case []any:
		return "list"
	case Tuple:
		return "tuple"
	case *Map:
		return "dict"
	case Set:
		return "set"
	case Bytes:
		return "bytes"
	case Timestamp:
		if t.DateOnly {
			return "date"
		}
		return "datetime"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// Truthy mirrors Python truthiness.
func Truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case int64:
		return t != 0
	case int:
		return t != 0
	case *big.Int:
		return t.Sign() != 0
	case float64:
		return t != 0
	case string:
		return t != ""
	case []any:
		return len(t) > 0
	case Tuple:
		return len(t) > 0
	case *Map:
		return t.Len() > 0
	case Set:
		return len(t) > 0
	case Bytes:
		return len(t) > 0
	default:
		return true
	}
}

// ToPlain converts a value tree into encoding/json-friendly Go values
// (map[string]any, []any, string, bool, nil, float64, int64, json.Number for
// big ints). It fails on non-string keys and non-JSON types.
func ToPlain(v any) (any, error) {
	switch t := v.(type) {
	case nil, bool, int64, float64, string:
		return t, nil
	case int:
		return int64(t), nil
	case *big.Int:
		return t.String(), nil
	case Tuple:
		return ToPlain([]any(t))
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			p, err := ToPlain(e)
			if err != nil {
				return nil, err
			}
			out[i] = p
		}
		return out, nil
	case *Map:
		out := make(map[string]any, t.Len())
		for _, e := range t.Entries() {
			ks, ok := e.Key.(string)
			if !ok {
				return nil, fmt.Errorf("mapping key %s must be a string", Repr(e.Key))
			}
			p, err := ToPlain(e.Value)
			if err != nil {
				return nil, err
			}
			out[ks] = p
		}
		return out, nil
	default:
		return nil, fmt.Errorf("value of type %s is not JSON-compatible", TypeName(v))
	}
}

// FromPlain converts encoding/json-style values (map[string]any etc.) into the
// pycompat model. Map keys are inserted in sorted order.
func FromPlain(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := NewMap()
		for _, k := range sortedKeys(t) {
			_ = m.Set(k, FromPlain(t[k]))
		}
		return m
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = FromPlain(e)
		}
		return out
	case []string:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = e
		}
		return out
	case map[string]string:
		m := NewMap()
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sortStrings(keys)
		for _, k := range keys {
			_ = m.Set(k, t[k])
		}
		return m
	case int:
		return int64(t)
	case float32:
		return float64(t)
	case interface{ String() string }:
		if n, ok := v.(interface {
			String() string
			Int64() (int64, error)
		}); ok { // json.Number
			return NumberFromLiteral(n.String())
		}
		return t
	default:
		return v
	}
}

// NumberFromLiteral parses a JSON number literal the way json.loads does:
// integers stay exact, anything with '.', 'e' or 'E' becomes a float.
func NumberFromLiteral(s string) any {
	for i := 0; i < len(s); i++ {
		if s[i] == '.' || s[i] == 'e' || s[i] == 'E' {
			f, err := strconv.ParseFloat(s, 64)
			if err != nil && !isRangeErr(err) {
				return s
			}
			return f
		}
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	b, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return s
	}
	return b
}

func isRangeErr(err error) bool {
	ne, ok := err.(*strconv.NumError)
	return ok && ne.Err == strconv.ErrRange
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortStrings(keys)
	return keys
}
