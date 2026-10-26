// Package audiencevalid ports the audience filter validation, account-key and
// accounts-rollup helpers of apps/api/routers/audiences.py.
package audiencevalid

import (
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
)

// FilterKeys are the audience filter keys Python accepts (FILTER_KEYS).
var FilterKeys = []string{
	"city", "state", "score_tier", "status", "source", "company_size",
	"specialization", "has_email", "has_phone", "has_website",
	"min_score", "max_score", "search", "job_ids",
}

var filterKeySet = func() map[string]bool {
	m := make(map[string]bool, len(FilterKeys))
	for _, k := range FilterKeys {
		m[k] = true
	}
	return m
}()

// ValueError is Python's ValueError raised by _validate_filters; the router
// wraps its text as the 422 detail "Value error, <Msg>".
type ValueError struct{ Msg string }

func (e *ValueError) Error() string { return e.Msg }

func valueErr(format string, a ...any) error { return &ValueError{Msg: fmt.Sprintf(format, a...)} }

// ValidateFilters is _validate_filters: unknown keys (sorted), then the three
// has_* booleans, then the two score integers, then min/max order. Integers
// must be JSON ints: decode with json.Decoder.UseNumber so 5.0 stays a float;
// a float64 value is a Python float and is rejected. Booleans are never
// integers. It returns v itself like Python.
func ValidateFilters(v map[string]any) (map[string]any, error) {
	var unknown []string
	for k := range v {
		if !filterKeySet[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, valueErr("unsupported audience filters: %s", strings.Join(unknown, ", "))
	}
	for _, k := range []string{"has_email", "has_phone", "has_website"} {
		if x, ok := v[k]; ok {
			if _, isBool := x.(bool); !isBool {
				return nil, valueErr("%s must be a boolean", k)
			}
		}
	}
	ints := map[string]*big.Int{}
	for _, k := range []string{"min_score", "max_score"} {
		if x, ok := v[k]; ok {
			b, isInt := toBig(x)
			if !isInt {
				return nil, valueErr("%s must be an integer", k)
			}
			ints[k] = b
		}
	}
	if lo, hi := ints["min_score"], ints["max_score"]; lo != nil && hi != nil && lo.Cmp(hi) > 0 {
		return nil, valueErr("min_score cannot exceed max_score")
	}
	return v, nil
}

// ToLeadFilter maps a validated audience filter to the leadsread.Filter with
// the semantics of query_leads_page: string filters apply when truthy,
// has_* when a bool, min/max when present, job_ids when non-empty (a bare
// string iterates as characters like Python's list("ab")), search and
// specialization are str()-converted. A truthy value of an unusable type is
// an error here where Python would fail inside the query.
func ToLeadFilter(f map[string]any) (leadsread.Filter, error) {
	var out leadsread.Filter
	for _, p := range []struct {
		key string
		dst *string
	}{
		{"city", &out.City}, {"state", &out.State}, {"score_tier", &out.ScoreTier},
		{"status", &out.Status}, {"source", &out.Source}, {"company_size", &out.CompanySize},
	} {
		x := f[p.key]
		if !truthy(x) {
			continue
		}
		s, ok := x.(string)
		if !ok {
			return out, fmt.Errorf("audience filter %s must be a string", p.key)
		}
		*p.dst = s
	}
	if x := f["specialization"]; truthy(x) {
		out.Specialization = pyStr(x)
	}
	if x := f["search"]; truthy(x) {
		out.Search = pyStr(x)
	}
	if x := f["job_ids"]; truthy(x) {
		switch j := x.(type) {
		case string:
			for _, r := range j {
				out.JobIDs = append(out.JobIDs, string(r))
			}
		case []any:
			for _, e := range j {
				s, ok := e.(string)
				if !ok {
					return out, fmt.Errorf("audience filter job_ids entries must be strings")
				}
				out.JobIDs = append(out.JobIDs, s)
			}
		default:
			return out, fmt.Errorf("audience filter job_ids must be a list")
		}
	}
	for _, p := range []struct {
		key string
		dst **bool
	}{{"has_email", &out.HasEmail}, {"has_phone", &out.HasPhone}, {"has_website", &out.HasWebsite}} {
		if b, ok := f[p.key].(bool); ok {
			v := b
			*p.dst = &v
		}
	}
	for _, p := range []struct {
		key string
		dst **int
	}{{"min_score", &out.MinScore}, {"max_score", &out.MaxScore}} {
		x, present := f[p.key]
		if !present || x == nil {
			continue
		}
		b, ok := toBig(x)
		if !ok {
			return out, fmt.Errorf("audience filter %s must be an integer", p.key)
		}
		var n int
		switch {
		case b.IsInt64():
			n = int(b.Int64())
		case b.Sign() > 0:
			n = math.MaxInt64
		default:
			n = math.MinInt64
		}
		*p.dst = &n
	}
	return out, nil
}
