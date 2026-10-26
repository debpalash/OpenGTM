package leadswrite

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// ColType is the Postgres storage class of a leads column.
type ColType int

const (
	TText     ColType = iota // varchar/text NOT NULL
	TTextNull                // varchar/text, nullable
	TInt                     // integer, nullable (int4)
)

// FieldError reports a value Postgres would reject for the column (Python
// answered 500; decision D2 answers 422). Nothing is written when it occurs.
type FieldError struct {
	Field  string
	Reason string
}

func (e *FieldError) Error() string { return "invalid value for " + e.Field + ": " + e.Reason }

// MutableColumns is every leads column except id, workspace_id, created_at,
// updated_at and search_tsv (update_lead_fields' allow-list in store.py).
var MutableColumns = map[string]ColType{
	"company":              TText,
	"website":              TTextNull,
	"email":                TTextNull,
	"email_confidence":     TTextNull,
	"email_provider":       TTextNull,
	"phone":                TTextNull,
	"phone_provider":       TTextNull,
	"contact_person":       TTextNull,
	"contact_title":        TTextNull,
	"city":                 TTextNull,
	"state":                TTextNull,
	"address":              TTextNull,
	"specialization":       TTextNull,
	"company_size":         TTextNull,
	"employee_count_exact": TInt,
	"description":          TTextNull,
	"revenue_range":        TTextNull,
	"founded_year":         TTextNull,
	"founding_year":        TTextNull,
	"industry_tags":        TTextNull,
	"technologies":         TTextNull,
	"funding_stage":        TTextNull,
	"linkedin_url":         TTextNull,
	"twitter_url":          TTextNull,
	"facebook_url":         TTextNull,
	"secondary_emails":     TTextNull,
	"secondary_phones":     TTextNull,
	"decision_makers":      TTextNull,
	"glassdoor_rating":     TTextNull,
	"hiring_signals":       TTextNull,
	"enrichment_attempts":  TInt,
	"enrichment_waterfall": TTextNull,
	"last_funding_amount":  TTextNull,
	"investors":            TTextNull,
	"recent_news":          TTextNull,
	"google_rating":        TTextNull,
	"revenue_estimate":     TTextNull,
	"email_verify":         TTextNull,
	"email_presence":       TTextNull,
	"source":               TTextNull,
	"score":                TInt,
	"score_tier":           TTextNull,
	"status":               TTextNull,
	"yupcha_value_prop":    TTextNull,
	"company_need":         TTextNull,
	"notes":                TTextNull,
	"last_enriched_at":     TTextNull,
	"company_size_basis":   TTextNull,
	"field_provenance":     TTextNull,
	"technographics":       TTextNull,
	"source_url":           TTextNull,
	"collection_job_id":    TTextNull,
}

// coerce validates v for column col and returns the pgx argument.
func coerce(col string, t ColType, v any) (any, error) {
	if v == nil {
		if t == TText {
			return nil, &FieldError{col, "null not allowed"}
		}
		return nil, nil
	}
	switch t {
	case TInt:
		var n int64
		switch x := v.(type) {
		case int:
			n = int64(x)
		case int32:
			n = int64(x)
		case int64:
			n = x
		case float64:
			if x != math.Trunc(x) || math.IsInf(x, 0) || math.Abs(x) > 1<<53 {
				return nil, &FieldError{col, "integer required"}
			}
			n = int64(x)
		case json.Number:
			i, err := x.Int64()
			if err != nil {
				return nil, &FieldError{col, "integer required"}
			}
			n = i
		default:
			return nil, &FieldError{col, "integer required"}
		}
		if n < math.MinInt32 || n > math.MaxInt32 {
			return nil, &FieldError{col, "integer out of range"}
		}
		return int32(n), nil
	default:
		s, ok := v.(string)
		if !ok {
			return nil, &FieldError{col, "string required"}
		}
		if strings.IndexByte(s, 0) >= 0 {
			return nil, &FieldError{col, "NUL byte not allowed"}
		}
		return s, nil
	}
}

// buildUpdate filters fields to the allow-list, validates every value and
// returns the SET clause (columns sorted for determinism) and its arguments.
// An empty result means nothing to write. Argument $1 is reserved for the id,
// $2 for updated_at.
func buildUpdate(fields map[string]any) (set string, args []any, err error) {
	names := make([]string, 0, len(fields))
	for k := range fields {
		if _, ok := MutableColumns[k]; ok {
			names = append(names, k)
		}
	}
	if len(names) == 0 {
		return "", nil, nil
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names)+1)
	for i, k := range names {
		arg, err := coerce(k, MutableColumns[k], fields[k])
		if err != nil {
			return "", nil, err
		}
		args = append(args, arg)
		parts = append(parts, fmt.Sprintf("%s = $%d", k, i+3))
	}
	parts = append(parts, "updated_at = $2")
	return strings.Join(parts, ", "), args, nil
}
