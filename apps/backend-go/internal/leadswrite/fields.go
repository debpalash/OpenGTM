package leadswrite

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
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

// coerce validates v for column col and returns the pgx argument. It mirrors
// what SQLAlchemy + psycopg3 + Postgres assignment casts store for a JSON body
// value (vectors: testdata/leadwrite_vectors.json, generated from the real
// PgLeadStore); only inputs on which Python answers 500 become *FieldError (D2).
//
// Exactness needs the body decoded with json.Decoder.UseNumber: float64 cannot
// tell 1e16 (Python float) from 10000000000000000 (Python int).
func coerce(col string, t ColType, v any) (any, error) {
	if v == nil {
		if t == TText {
			return nil, &FieldError{col, "null not allowed"}
		}
		return nil, nil
	}
	if t == TInt {
		n, err := intColumn(v)
		if err != nil {
			return nil, &FieldError{col, err.Error()}
		}
		return n, nil
	}
	s, err := textColumn(v)
	if err != nil {
		return nil, &FieldError{col, err.Error()}
	}
	return s, nil
}

func hasNUL(s string) bool { return strings.IndexByte(s, 0) >= 0 }

// number classifies a numeric JSON value: an integer (decimal text) or a float.
func number(v any) (isInt bool, intText string, f float64, ok bool) {
	switch x := v.(type) {
	case json.Number:
		str := x.String()
		if !strings.ContainsAny(str, ".eE") {
			b, good := new(big.Int).SetString(str, 10)
			if !good {
				return false, "", 0, false
			}
			return true, b.String(), 0, true
		}
		fl, err := strconv.ParseFloat(str, 64) // overflow yields +-Inf like Python json
		if err != nil && !math.IsInf(fl, 0) {
			return false, "", 0, false
		}
		return false, "", fl, true
	case int:
		return true, strconv.FormatInt(int64(x), 10), 0, true
	case int32:
		return true, strconv.FormatInt(int64(x), 10), 0, true
	case int64:
		return true, strconv.FormatInt(x, 10), 0, true
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1e15 { // indistinguishable from an int
			return true, strconv.FormatInt(int64(x), 10), 0, true
		}
		return false, "", x, true
	}
	return false, "", 0, false
}

// pgFloat is float8out (shortest round-trip), which equals Go's 'e' format for
// the exponent form and 'f' otherwise: plain when -5 < exp10 < 15.
func pgFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	e := strconv.FormatFloat(f, 'e', -1, 64)
	exp, _ := strconv.Atoi(e[strings.IndexByte(e, 'e')+1:])
	if f != 0 && (exp < -4 || exp >= 15) {
		return e
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func textColumn(v any) (string, error) {
	switch x := v.(type) {
	case string:
		if hasNUL(x) {
			return "", errors.New("NUL byte not allowed")
		}
		return x, nil
	case bool:
		return strconv.FormatBool(x), nil
	case []any:
		return pgArray(x)
	}
	if isInt, it, f, ok := number(v); ok {
		if isInt {
			return it, nil
		}
		return pgFloat(f), nil
	}
	return "", errors.New("unsupported value")
}

// ---- integer column ----

func intColumn(v any) (int32, error) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case string:
		return parseInt4(x)
	}
	isInt, it, f, ok := number(v)
	if !ok {
		return 0, errors.New("integer required")
	}
	if isInt {
		n, err := strconv.ParseInt(it, 10, 32)
		if err != nil {
			return 0, errors.New("integer out of range")
		}
		return int32(n), nil
	}
	r := math.RoundToEven(f) // float8 -> int4 rounds to nearest, ties to even
	if math.IsNaN(r) || r < math.MinInt32 || r > math.MaxInt32 {
		return 0, errors.New("integer out of range")
	}
	return int32(r), nil
}

func isSpace(c byte) bool { return c == ' ' || (c >= '\t' && c <= '\r') }

// parseInt4 is Postgres 16 int4in: surrounding whitespace, sign, decimal or
// 0x/0o/0b digits with single underscores between digits.
func parseInt4(s string) (int32, error) {
	bad := errors.New("invalid integer text")
	i, end := 0, len(s)
	for i < end && isSpace(s[i]) {
		i++
	}
	for end > i && isSpace(s[end-1]) {
		end--
	}
	body := s[i:end]
	neg := false
	if body != "" && (body[0] == '+' || body[0] == '-') {
		neg = body[0] == '-'
		body = body[1:]
	}
	base := 10
	if len(body) >= 2 && body[0] == '0' {
		switch body[1] {
		case 'x', 'X':
			base, body = 16, body[2:]
		case 'o', 'O':
			base, body = 8, body[2:]
		case 'b', 'B':
			base, body = 2, body[2:]
		}
	}
	if body == "" {
		return 0, bad
	}
	var acc uint64
	sawDigit := false
	for k := 0; k < len(body); k++ {
		c := body[k]
		if c == '_' {
			// A single underscore must sit between digits (or follow a 0x/0o/0b prefix).
			prevOK := (k == 0 && base != 10) || (k > 0 && body[k-1] != '_')
			if !prevOK || k == len(body)-1 || body[k+1] == '_' {
				return 0, bad
			}
			continue
		}
		var d int
		switch {
		case c >= '0' && c <= '9':
			d = int(c - '0')
		case c >= 'a' && c <= 'f':
			d = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = int(c-'A') + 10
		default:
			return 0, bad
		}
		if d >= base {
			return 0, bad
		}
		acc = acc*uint64(base) + uint64(d)
		if acc > 1<<32 {
			return 0, errors.New("integer out of range")
		}
		sawDigit = true
	}
	if !sawDigit {
		return 0, bad
	}
	if neg {
		if acc > 1<<31 {
			return 0, errors.New("integer out of range")
		}
		return int32(-int64(acc)), nil
	}
	if acc > math.MaxInt32 {
		return 0, errors.New("integer out of range")
	}
	return int32(acc), nil
}

// ---- text column fed a JSON array: psycopg sends a Postgres array whose text
// form is stored. The element type comes from the values and must be uniform.

type leafKind int

const (
	kNone leafKind = iota
	kInt
	kFloat
	kBool
	kStr
)

func pgArray(a []any) (string, error) {
	var kind leafKind
	var b strings.Builder
	if err := arrayLevel(&b, a, &kind, map[int]int{}, 0); err != nil {
		return "", err
	}
	return b.String(), nil
}

func arrayLevel(b *strings.Builder, a []any, kind *leafKind, shape map[int]int, depth int) error {
	if n, seen := shape[depth]; seen && n != len(a) {
		return errors.New("ragged array")
	}
	shape[depth] = len(a)
	b.WriteByte('{')
	nested, flat := 0, 0
	for _, e := range a {
		if _, ok := e.([]any); ok {
			nested++
		} else {
			flat++
		}
	}
	if nested > 0 && flat > 0 {
		return errors.New("mixed array nesting")
	}
	for i, e := range a {
		if i > 0 {
			b.WriteByte(',')
		}
		if sub, ok := e.([]any); ok {
			if err := arrayLevel(b, sub, kind, shape, depth+1); err != nil {
				return err
			}
			continue
		}
		if err := arrayLeaf(b, e, kind); err != nil {
			return err
		}
	}
	b.WriteByte('}')
	return nil
}

func arrayLeaf(b *strings.Builder, e any, kind *leafKind) error {
	set := func(k leafKind) error {
		if *kind != kNone && *kind != k {
			return errors.New("mixed array element types")
		}
		*kind = k
		return nil
	}
	if e == nil {
		b.WriteString("NULL")
		return nil
	}
	switch x := e.(type) {
	case bool:
		if err := set(kBool); err != nil {
			return err
		}
		b.WriteString(map[bool]string{true: "t", false: "f"}[x])
		return nil
	case string:
		if err := set(kStr); err != nil {
			return err
		}
		if hasNUL(x) {
			return errors.New("NUL byte not allowed")
		}
		writeArrayString(b, x)
		return nil
	}
	if isInt, it, f, ok := number(e); ok {
		if isInt {
			if err := set(kInt); err != nil {
				return err
			}
			b.WriteString(it)
			return nil
		}
		if err := set(kFloat); err != nil {
			return err
		}
		b.WriteString(pgFloat(f))
		return nil
	}
	return errors.New("unsupported array element")
}

func writeArrayString(b *strings.Builder, s string) {
	quote := s == "" || strings.EqualFold(s, "NULL")
	if !quote {
		for i := 0; i < len(s); i++ {
			c := s[i]
			if c == '"' || c == '\\' || c == '{' || c == '}' || c == ',' || isSpace(c) {
				quote = true
				break
			}
		}
	}
	if !quote {
		b.WriteString(s)
		return
	}
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		if s[i] == '"' || s[i] == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	b.WriteByte('"')
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
