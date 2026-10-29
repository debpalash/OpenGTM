// Package ambitionimport ports services/workbook/ambitionbox_import.py: the
// durable, resumable AmbitionBox company import (start, page loop, failure
// reconcile).
package ambitionimport

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// columnsJSON is AMBITIONBOX_COLUMNS verbatim (Python json.dumps separators).
const columnsJSON = `[{"id": "company", "name": "Company", "type": "lead_field", "lead_field": "company", "width": 220}, ` +
	`{"id": "industry", "name": "Industry", "type": "lead_field", "lead_field": "industry", "width": 180}, ` +
	`{"id": "ambitionbox_rating", "name": "Rating", "type": "lead_field", "lead_field": "ambitionbox_rating", "width": 90}, ` +
	`{"id": "review_count", "name": "Reviews", "type": "lead_field", "lead_field": "review_count", "width": 100}, ` +
	`{"id": "company_size", "name": "Employees", "type": "lead_field", "lead_field": "company_size", "width": 130}, ` +
	`{"id": "city", "name": "Top location", "type": "lead_field", "lead_field": "city", "width": 150}, ` +
	`{"id": "jobs_count", "name": "Jobs", "type": "lead_field", "lead_field": "jobs_count", "width": 80}, ` +
	`{"id": "company_type", "name": "Company type", "type": "lead_field", "lead_field": "company_type", "width": 150}, ` +
	`{"id": "is_verified", "name": "Verified", "type": "lead_field", "lead_field": "is_verified", "width": 90}, ` +
	`{"id": "ambitionbox_url", "name": "AmbitionBox", "type": "lead_field", "lead_field": "ambitionbox_url", "width": 260}]`

// Columns returns AMBITIONBOX_COLUMNS as ordered objects.
func Columns() []*wbcrud.Obj {
	var raw []json.RawMessage
	if err := json.Unmarshal([]byte(columnsJSON), &raw); err != nil {
		panic(err)
	}
	out := make([]*wbcrud.Obj, len(raw))
	for i, r := range raw {
		o, err := wbcrud.ParseObject(r)
		if err != nil {
			panic(err)
		}
		out[i] = o
	}
	return out
}

// pyTruthy is Python truthiness for decoded JSON values.
func pyTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int:
		return x != 0
	case int32:
		return x != 0
	case int64:
		return x != 0
	case float32:
		return x != 0
	case float64:
		return x != 0
	case json.Number:
		f, err := strconv.ParseFloat(string(x), 64)
		return err != nil || f != 0 || math.IsNaN(f)
	case []any:
		return len(x) > 0
	case *wbcrud.Obj:
		return x.Len() > 0
	}
	return true
}

// or is Python `a or b`.
func or(v, def any) any {
	if pyTruthy(v) {
		return v
	}
	return def
}

func get(o *wbcrud.Obj, k string) any {
	v, _ := o.Get(k)
	return v
}

// pyStr is str(v) for the scalar JSON values a company card carries.
func pyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case string:
		return x
	case bool:
		if x {
			return "True"
		}
		return "False"
	case json.Number:
		return string(x)
	case float64:
		s := strconv.FormatFloat(x, 'f', -1, 64)
		if !strings.ContainsAny(s, ".") {
			s += ".0"
		}
		return s
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// CompanyToRow is company_to_row: an AmbitionBox card mapped to workbook
// fields in Python's dict order, without inventing a website.
func CompanyToRow(c *wbcrud.Obj) *wbcrud.Obj {
	name := ""
	if v := get(c, "name"); pyTruthy(v) {
		name = strings.TrimSpace(pyStr(v))
	}
	var list = func(k string) any {
		if v := get(c, k); pyTruthy(v) {
			return v
		}
		return []any{}
	}
	o := wbcrud.NewObj()
	o.Set("company", name)
	o.Set("industry", or(get(c, "industry"), ""))
	o.Set("specialization", or(get(c, "industry"), ""))
	o.Set("industry_tags", or(get(c, "industry"), ""))
	o.Set("company_size", or(get(c, "employee_count"), ""))
	o.Set("city", or(get(c, "top_location"), ""))
	o.Set("source", "ambitionbox")
	o.Set("ambitionbox_company_id", get(c, "company_id"))
	o.Set("ambitionbox_rating", get(c, "rating"))
	o.Set("review_count", get(c, "review_count"))
	o.Set("jobs_count", or(get(c, "jobs_count"), 0))
	o.Set("salaries_count", or(get(c, "salaries_count"), 0))
	o.Set("interviews_count", or(get(c, "interviews_count"), 0))
	o.Set("total_locations", or(get(c, "total_locations"), 0))
	o.Set("company_type", or(get(c, "company_type"), ""))
	o.Set("is_verified", pyTruthy(get(c, "is_verified")))
	o.Set("ambitionbox_url", or(get(c, "profile_url"), ""))
	o.Set("logo_url", or(get(c, "logo_url"), ""))
	o.Set("highly_rated_for", list("highly_rated_for"))
	o.Set("critically_rated_for", list("critically_rated_for"))
	return o
}
