package leaddedup

import (
	"errors"
	"math"
	"sort"
	"strings"
)

// ErrNonNumericScore is returned by MergeLeadsStrict where Python raises
// TypeError (a NULL or non-numeric score compared with max()).
var ErrNonNumericScore = errors.New("leaddedup: non-numeric score")

// truthy is Python bool(v) for JSON-shaped values.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case int:
		return x != 0
	case int32:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// isEmptyMarker is `v in ("", "N/A", "nan", "0")`-style membership: only
// strings can equal the markers.
func isEmptyMarker(v any) bool {
	s, ok := v.(string)
	return ok && (s == "" || s == "N/A" || s == "nan")
}

func isEmptyOrZeroMarker(v any) bool {
	if s, ok := v.(string); ok && s == "0" {
		return true
	}
	return isEmptyMarker(v)
}

func numeric(v any) (f float64, i int64, isInt, ok bool) {
	switch x := v.(type) {
	case int:
		return float64(x), int64(x), true, true
	case int32:
		return float64(x), int64(x), true, true
	case int64:
		return float64(x), x, true, true
	case bool:
		if x {
			return 1, 1, true, true
		}
		return 0, 0, true, true
	case float64:
		return x, 0, false, true
	}
	return 0, 0, false, false
}

// greater is Python `a > b` for numbers.
func greater(a, b any) bool {
	af, ai, aInt, _ := numeric(a)
	bf, bi, bInt, _ := numeric(b)
	if aInt && bInt {
		return ai > bi
	}
	if math.IsNaN(af) || math.IsNaN(bf) {
		return false
	}
	return af > bf
}

// ErrNonStringSource is returned by MergeLeadsStrict where Python raises
// TypeError in the source join: a present non-string (incl. NULL) master
// source, or a truthy non-string duplicate source.
var ErrNonStringSource = errors.New("leaddedup: non-string source")

// MergeLeads ports merge_leads (waterfall fill of empty fields). Non-numeric
// scores are ignored in the max (Python raises TypeError; see
// MergeLeadsStrict). Non-string sources are ignored.
func MergeLeads(master Rec, dups []Rec) Rec {
	out, _ := merge(master, dups, false)
	return out
}

// MergeLeadsStrict is MergeLeads but returns ErrNonNumericScore where Python
// would crash with TypeError (decision D2: callers answer 422).
func MergeLeadsStrict(master Rec, dups []Rec) (Rec, error) { return merge(master, dups, true) }

func merge(master Rec, dups []Rec, strict bool) (Rec, error) {
	merged := master.clone()
	for _, d := range dups {
		for _, key := range d.Keys {
			if key == "id" || key == "created_at" {
				continue
			}
			value := d.Vals[key]
			cur, ok := merged.Vals[key]
			if !ok {
				cur = ""
			}
			if (!truthy(cur) || isEmptyOrZeroMarker(cur)) && truthy(value) && !isEmptyOrZeroMarker(value) {
				merged.set(key, value)
			}
		}
	}

	sources := map[string]struct{}{}
	if mv, has := master.Vals["source"]; !has {
		sources[""] = struct{}{}
	} else if s, ok := mv.(string); ok {
		sources[s] = struct{}{}
	} else if strict {
		return Rec{}, ErrNonStringSource
	}
	for _, d := range dups {
		dv := d.Vals["source"]
		if s, ok := dv.(string); ok {
			if s != "" {
				sources[s] = struct{}{}
			}
		} else if truthy(dv) && strict {
			return Rec{}, ErrNonStringSource
		}
	}
	delete(sources, "")
	list := make([]string, 0, len(sources))
	for s := range sources {
		list = append(list, s)
	}
	sort.Strings(list)
	merged.set("source", strings.Join(list, "|"))

	all := make([]any, 0, len(dups)+1)
	all = append(all, getOr(master, "score", int64(0)))
	for _, d := range dups {
		all = append(all, getOr(d, "score", int64(0)))
	}
	var best any
	have := false
	for _, v := range all {
		if _, _, _, ok := numeric(v); !ok {
			if strict {
				return Rec{}, ErrNonNumericScore
			}
			continue
		}
		if !have || greater(v, best) {
			best, have = v, true
		}
	}
	if have {
		merged.set("score", best)
	}
	return merged, nil
}
