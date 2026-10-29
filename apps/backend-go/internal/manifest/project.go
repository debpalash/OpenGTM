package manifest

import (
	"regexp"
	"strconv"
	"strings"
)

// Python's `$` also matches before a single trailing newline, hence \n?.
var tokenRE = regexp.MustCompile(`^([A-Za-z0-9_]*)(?:\[([0-9]*)\])?\n?$`)

// walkPath walks a JSONPath-lite path (no leading "$."): `key`, `key[]`
// (fan-out, first non-empty), `key[2]` and `[2]`.
func walkPath(data any, path string) any {
	cur := data
	parts := strings.Split(path, ".")
	for position, raw := range parts {
		if cur == nil {
			return nil
		}
		m := tokenRE.FindStringSubmatchIndex(raw)
		if m == nil {
			return nil
		}
		if key := raw[m[2]:m[3]]; key != "" {
			obj, ok := cur.(map[string]any)
			if !ok {
				return nil
			}
			cur = obj[key]
		}
		if m[4] < 0 { // no [..] group
			continue
		}
		list, ok := cur.([]any)
		if !ok {
			return nil
		}
		idx := raw[m[4]:m[5]]
		if idx == "" {
			remainder := strings.Join(parts[position+1:], ".")
			for _, item := range list {
				v := item
				if remainder != "" {
					v = walkPath(item, remainder)
				}
				if s, isStr := v.(string); v != nil && !(isStr && s == "") {
					return v
				}
			}
			return nil
		}
		i, err := strconv.Atoi(idx)
		if err != nil || i < 0 || i >= len(list) {
			cur = nil
		} else {
			cur = list[i]
		}
	}
	return cur
}

// ProjectValue resolves one mapping expression against the response JSON.
// An expression without "$." is a static literal; text before "$." is a
// literal prefix (str() of the value is appended). Returns nil when absent.
func ProjectValue(data any, expr string) any {
	idx := strings.Index(expr, "$.")
	if idx == -1 {
		return expr
	}
	prefix, path := expr[:idx], expr[idx+2:]
	val := data
	if path != "" {
		val = walkPath(data, path)
	}
	if val == nil {
		return nil
	}
	if prefix != "" {
		return prefix + pyStr(val)
	}
	return val
}

// Project projects the response onto {output_field: value}; nil and "" are dropped.
func Project(data any, mappings map[string]string) map[string]any {
	out := make(map[string]any, len(mappings))
	for field, expr := range mappings {
		val := ProjectValue(data, expr)
		if s, ok := val.(string); val == nil || (ok && s == "") {
			continue
		}
		out[field] = val
	}
	return out
}
