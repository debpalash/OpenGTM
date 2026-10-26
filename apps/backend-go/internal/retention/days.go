// Package retention is the retention policy core: day normalization, the eight
// per-category targets, the non-destructive preview, the policy store and the
// next-run scheduling (port of governance/retention.py). Enforcement (the
// deleting job) lives elsewhere and must use Targets/Cutoff/NormalizeDays so
// preview and enforce can never disagree.
package retention

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Categories is the retention category order (Python DEFAULT_DAYS insertion order).
var Categories = []string{"audit", "llm_usage", "signals", "activation", "audience_history", "agent_results", "outreach_history"}

// DefaultDays are the days applied to a category the policy does not set.
var DefaultDays = map[string]int{"audit": 365, "llm_usage": 365, "signals": 365, "activation": 180, "audience_history": 365, "agent_results": 180, "outreach_history": 365}

// MinDays are the per-category lower bounds; MaxDays is the shared upper bound.
var MinDays = map[string]int{"audit": 90, "llm_usage": 30, "signals": 30, "activation": 30, "audience_history": 30, "agent_results": 30, "outreach_history": 30}

// MaxDays is the largest accepted retention (10 years).
const MaxDays = 3650

// Days maps category to retention days. It marshals in Categories order
// (Python dict(DEFAULT_DAYS) overlay order), then any other keys sorted.
type Days map[string]int

// Counts maps category to a row count; it marshals in Categories order.
type Counts map[string]int64

func (d Days) MarshalJSON() ([]byte, error) {
	return marshalOrdered(len(d), func(k string) (any, bool) { v, ok := d[k]; return v, ok }, keysOf(d))
}

func (c Counts) MarshalJSON() ([]byte, error) {
	return marshalOrdered(len(c), func(k string) (any, bool) { v, ok := c[k]; return v, ok }, keysOf(c))
}

func keysOf[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

func marshalOrdered(n int, get func(string) (any, bool), keys []string) ([]byte, error) {
	seen := map[string]bool{}
	ordered := make([]string, 0, n)
	for _, k := range Categories {
		if _, ok := get(k); ok {
			ordered = append(ordered, k)
			seen[k] = true
		}
	}
	var rest []string
	for _, k := range keys {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	ordered = append(ordered, rest...)
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range ordered {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		v, _ := get(k)
		vb, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		b.Write(kb)
		b.WriteByte(':')
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// NormalizeDays is normalized_days: unknown categories are rejected (sorted, one
// error), omitted ones take DefaultDays, every given value must be an integer in
// MinDays[key]..3650. Error text is byte-equal to Python's. When several values
// violate the range, the first in Categories order is reported (Python reports
// request order; use NormalizeDaysOrdered to reproduce that).
func NormalizeDays(in map[string]any) (Days, error) { return NormalizeDaysOrdered(in, nil) }

// NormalizeDaysOrdered is NormalizeDays reporting range violations in the given
// key order (the request body's order); keys missing from order follow in
// Categories order.
func NormalizeDaysOrdered(in map[string]any, order []string) (Days, error) {
	var unknown []string
	for k := range in {
		if _, ok := DefaultDays[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("unsupported retention categories: %s", strings.Join(unknown, ", "))
	}
	out := Days{}
	for k, v := range DefaultDays {
		out[k] = v
	}
	seq := append(append([]string{}, order...), Categories...)
	done := map[string]bool{}
	for _, k := range seq {
		raw, ok := in[k]
		if !ok || done[k] {
			continue
		}
		done[k] = true
		days, ok := asInt(raw)
		if !ok {
			return nil, fmt.Errorf("%s retention must be an integer", k)
		}
		if days < int64(MinDays[k]) || days > MaxDays {
			return nil, fmt.Errorf("%s retention must be %d..%d days", k, MinDays[k], MaxDays)
		}
		out[k] = int(days)
	}
	return out, nil
}

// asInt accepts integers and integral floats (JSON numbers decode to float64);
// everything else (bool, string, nil, fractions, NaN, huge values) is refused
// instead of being coerced like Python's int().
func asInt(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int8:
		return int64(n), true
	case int16:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case uint8:
		return int64(n), true
	case uint16:
		return int64(n), true
	case uint32:
		return int64(n), true
	case float32:
		return asInt(float64(n))
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) || n != math.Trunc(n) || math.Abs(n) > 1<<53 {
			return 0, false
		}
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}

// normalizeInts is NormalizeDays for an int map (Preview/UpsertPolicy callers).
func normalizeInts(in map[string]int) (Days, error) {
	m := make(map[string]any, len(in))
	for k, v := range in {
		m[k] = v
	}
	return NormalizeDays(m)
}
