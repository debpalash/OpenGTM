package audiencerefresh

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/debpalash/OpenGTM/apps/server/internal/jobs/jobkit"
)

// leadFilter is the WHERE clause (over alias l, after the tenant predicate)
// and its arguments that PgLeadStore.query_leads_page builds from an
// audience's filter_criteria.
type leadFilter struct {
	conds []string
	args  []any
}

func (f *leadFilter) arg(v any) string {
	f.args = append(f.args, v)
	return fmt.Sprintf("$%d", len(f.args))
}

// simpleKeys are the equality filters, applied when truthy.
var simpleKeys = []string{"city", "state", "score_tier", "status", "source", "company_size"}

// searchColumns feed the ILIKE fallback of the search filter.
var searchColumns = []string{"company", "city", "specialization", "notes", "description"}

// buildLeadFilter ports the filter handling of PgLeadStore.query_leads_page
// for the decoded `audience.filters or {}`. firstParam is the number of
// parameters the caller already uses ($1 is the workspace).
//
// Values the Python code would hand to the database with the wrong type
// (a number for a text column, a string for a boolean ...) make PostgreSQL
// reject the query there; here they are rejected up front, which fails the
// refresh the same way.
func buildLeadFilter(filters any, firstParam int) (*leadFilter, error) {
	f := &leadFilter{args: make([]any, firstParam)}
	if !jobkit.Truthy(filters) {
		return f, nil // `audience.filters or {}`
	}
	fc, ok := filters.(*jobkit.Object)
	if !ok {
		return nil, jobkit.TypedErrorf("AttributeError", "'%s' object has no attribute 'get'", jobkit.TypeName(filters))
	}
	get := func(key string) any { v, _ := fc.Get(key); return v }

	if ids := get("lead_ids"); ids != nil {
		list, err := idList(ids)
		if err != nil {
			return nil, err
		}
		f.conds = append(f.conds, fmt.Sprintf("l.id = ANY(%s::bigint[])", f.arg(list)))
	}
	for _, key := range simpleKeys {
		if v := get(key); jobkit.Truthy(v) {
			s, ok := v.(string)
			if !ok {
				return nil, typeMismatch("character varying", v)
			}
			f.conds = append(f.conds, fmt.Sprintf("l.%s = %s", key, f.arg(s)))
		}
	}
	if v := get("job_ids"); jobkit.Truthy(v) {
		items, err := pyList(v)
		if err != nil {
			return nil, err
		}
		ids := make([]string, 0, len(items))
		sources := make([]string, 0, len(items))
		for _, it := range items {
			s, ok := it.(string)
			if !ok {
				return nil, typeMismatch("character varying", it)
			}
			ids = append(ids, s)
			sources = append(sources, "job:"+s)
		}
		f.conds = append(f.conds, fmt.Sprintf("(l.collection_job_id = ANY(%s::text[]) OR l.source = ANY(%s::text[]))",
			f.arg(ids), f.arg(sources)))
	}
	if v := get("specialization"); jobkit.Truthy(v) {
		s, err := pyFormat(v)
		if err != nil {
			return nil, err
		}
		f.conds = append(f.conds, fmt.Sprintf("l.specialization ILIKE %s", f.arg("%"+s+"%")))
	}
	for _, col := range []string{"email", "phone", "website"} {
		switch v := get("has_" + col).(type) {
		case bool:
			if v {
				f.conds = append(f.conds, fmt.Sprintf("(l.%[1]s IS NOT NULL AND l.%[1]s <> '')", col))
			} else {
				f.conds = append(f.conds, fmt.Sprintf("(l.%[1]s IS NULL OR l.%[1]s = '')", col))
			}
		}
	}
	for _, c := range []struct{ key, op string }{{"min_score", ">="}, {"max_score", "<="}} {
		v := get(c.key)
		if v == nil {
			continue
		}
		text, err := numericText(v)
		if err != nil {
			return nil, err
		}
		f.conds = append(f.conds, fmt.Sprintf("l.score %s %s::text::numeric", c.op, f.arg(text)))
	}
	if v := get("search"); jobkit.Truthy(v) {
		s, err := pyFormat(v)
		if err != nil {
			return nil, err
		}
		tsv := f.arg(s)
		like := f.arg("%" + s + "%")
		parts := []string{fmt.Sprintf("l.search_tsv @@ websearch_to_tsquery('english', %s)", tsv)}
		for _, c := range searchColumns {
			parts = append(parts, fmt.Sprintf("l.%s ILIKE %s", c, like))
		}
		f.conds = append(f.conds, "("+strings.Join(parts, " OR ")+")")
	}
	return f, nil
}

func (f *leadFilter) where() string {
	if len(f.conds) == 0 {
		return ""
	}
	return " AND " + strings.Join(f.conds, " AND ")
}

// idList converts lead_ids (`id IN (...)`) to integers.
func idList(v any) ([]int64, error) {
	items, ok := v.([]any)
	if !ok {
		return nil, jobkit.TypedErrorf("ArgumentError",
			"IN expression list, SELECT construct, or bound parameter object expected, got %s", jobkit.Repr(jobkit.TypeName(v)))
	}
	out := make([]int64, 0, len(items))
	for _, it := range items {
		n, ok := it.(json.Number)
		if !ok || jobkit.IsFloatLiteral(n) {
			return nil, typeMismatch("integer", it)
		}
		i, err := n.Int64()
		if err != nil {
			return nil, jobkit.TypedErrorf("DataError", "value %s is out of range for type bigint", string(n))
		}
		out = append(out, i)
	}
	return out, nil
}

// pyList is list(v) for a decoded JSON value.
func pyList(v any) ([]any, error) {
	switch t := v.(type) {
	case []any:
		return t, nil
	case string:
		var out []any
		for _, r := range t {
			out = append(out, string(r))
		}
		return out, nil
	case *jobkit.Object:
		out := make([]any, 0, len(t.Keys))
		for _, k := range t.Keys {
			out = append(out, k)
		}
		return out, nil
	}
	return nil, jobkit.TypedErrorf("TypeError", "'%s' object is not iterable", jobkit.TypeName(v))
}

// pyFormat is str(v) for the scalar values that can reach an f-string.
func pyFormat(v any) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case json.Number:
		return string(t), nil
	case bool:
		if t {
			return "True", nil
		}
		return "False", nil
	}
	return "", jobkit.TypedErrorf("TypeError", "unsupported filter value of type %s", jobkit.TypeName(v))
}

// numericText renders a min_score/max_score bound for `score >= $n::numeric`.
func numericText(v any) (string, error) {
	switch t := v.(type) {
	case json.Number:
		return string(t), nil
	case string:
		return t, nil // PostgreSQL parses it, or rejects it as it would in Python
	}
	return "", typeMismatch("integer", v)
}

func typeMismatch(column string, v any) error {
	return jobkit.TypedErrorf("ProgrammingError", "operator does not exist: %s = %s", column, jobkit.TypeName(v))
}
