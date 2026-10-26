// Package colgraph ports the workbook column dependency graph
// (apps/api/services/workbook/column_deps.py) and the "only run if" condition
// engine (conditions.py). Behaviour, including the odd corners, follows the
// Python reference vectors in testdata/.
package colgraph

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
)

// Col is one column config object. Keys keeps JSON key order (columns_config is
// a JSON, not JSONB, column) so a column can be written back unchanged.
type Col struct {
	Keys []string
	Vals map[string]any
}

// MakeCol builds a Col from alternating key, value arguments.
func MakeCol(kv ...any) Col {
	c := Col{Vals: map[string]any{}}
	for i := 0; i+1 < len(kv); i += 2 {
		k := kv[i].(string)
		c.Set(k, kv[i+1])
	}
	return c
}

// Set stores v under k; an existing key keeps its position.
func (c *Col) Set(k string, v any) {
	if c.Vals == nil {
		c.Vals = map[string]any{}
	}
	if _, ok := c.Vals[k]; !ok {
		c.Keys = append(c.Keys, k)
	}
	c.Vals[k] = v
}

// Get returns the value for k, nil when absent (dict.get).
func (c Col) Get(k string) any { return c.Vals[k] }

// Lookup returns the value and whether the key is present.
func (c Col) Lookup(k string) (any, bool) { v, ok := c.Vals[k]; return v, ok }

// ParseCol decodes a JSON object keeping key order. Numbers stay json.Number
// so ints and floats keep their Python types.
func ParseCol(raw []byte) (Col, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return Col{}, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return Col{}, errors.New("colgraph: column is not a JSON object")
	}
	c := Col{Vals: map[string]any{}}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return Col{}, err
		}
		k, _ := kt.(string)
		var v any
		if err := dec.Decode(&v); err != nil {
			return Col{}, err
		}
		c.Set(k, v)
	}
	if _, err := dec.Token(); err != nil && err != io.EOF {
		return Col{}, err
	}
	return c, nil
}

// ParseCols decodes a JSON array of column objects.
func ParseCols(raw []byte) ([]Col, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	out := make([]Col, len(items))
	for i, it := range items {
		c, err := ParseCol(it)
		if err != nil {
			return nil, fmt.Errorf("column %d: %w", i, err)
		}
		out[i] = c
	}
	return out, nil
}

// without returns a shallow copy minus one key.
func (c Col) without(skip string) Col {
	out := Col{Vals: make(map[string]any, len(c.Vals))}
	for _, k := range c.Keys {
		if k != skip {
			out.Keys = append(out.Keys, k)
			out.Vals[k] = c.Vals[k]
		}
	}
	return out
}

// --- Python value helpers -------------------------------------------------

func pyIsSpace(r rune) bool {
	switch {
	case r >= 0x09 && r <= 0x0d, r >= 0x1c && r <= 0x20, r == 0x85, r == 0xa0, r == 0x1680,
		r >= 0x2000 && r <= 0x200a, r == 0x2028, r == 0x2029, r == 0x202f, r == 0x205f, r == 0x3000:
		return true
	}
	return false
}

// pyStrip is str.strip().
func pyStrip(s string) string { return strings.TrimFunc(s, pyIsSpace) }

func isCased(r rune) bool { return unicode.IsUpper(r) || unicode.IsLower(r) || unicode.IsTitle(r) }

func isCaseIgnorable(r rune) bool {
	if unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf, unicode.Lm, unicode.Sk) {
		return true
	}
	return strings.ContainsRune("'.:^`·‘’․‧﹒＇．：", r)
}

// pyLower is str.lower(): full case mapping for U+0130 and the final-sigma rule.
func pyLower(s string) string {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return strings.ToLower(s)
	}
	rs := []rune(s)
	var b strings.Builder
	for i, r := range rs {
		switch r {
		case 0x130:
			b.WriteString("i̇")
		case 0x3a3:
			before := false
			for j := i - 1; j >= 0; j-- {
				if isCaseIgnorable(rs[j]) {
					continue
				}
				before = isCased(rs[j])
				break
			}
			after := false
			for j := i + 1; j < len(rs); j++ {
				if isCaseIgnorable(rs[j]) {
					continue
				}
				after = isCased(rs[j])
				break
			}
			if before && !after {
				b.WriteRune('ς')
			} else {
				b.WriteRune('σ')
			}
		default:
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// pyTruthy mirrors Python truthiness for decoded JSON values.
func pyTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return t != ""
	case bool:
		return t
	case []any:
		return len(t) > 0
	case []string:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	case map[string]string:
		return len(t) > 0
	case Col:
		return len(t.Vals) > 0
	case json.Number:
		f, ok := parseNumberFloat(t)
		return !ok || f != 0
	case int:
		return t != 0
	case int64:
		return t != 0
	case float64:
		return t != 0
	}
	return true
}

func toMap(v any) (map[string]any, bool) {
	switch t := v.(type) {
	case map[string]any:
		return t, true
	case Col:
		return t.Vals, true
	case map[string]string:
		m := make(map[string]any, len(t))
		for k, e := range t {
			m[k] = e
		}
		return m, true
	}
	return nil, false
}

func toList(v any) ([]any, bool) {
	switch t := v.(type) {
	case []any:
		return t, true
	case []string:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = e
		}
		return out, true
	}
	return nil, false
}

// sameValue is Python ==/!= for decoded config values (None == None).
func sameValue(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if sa, ok := a.(string); ok {
		sb, ok := b.(string)
		return ok && sa == sb
	}
	return pyStrValue(a) == pyStrValue(b) && fmt.Sprintf("%T", a) == fmt.Sprintf("%T", b)
}

func idString(c Col) (string, bool) {
	v, ok := c.Lookup("id")
	if !ok || v == nil {
		return "", false
	}
	return pyStrValue(v), true
}
