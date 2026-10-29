package wbcrud

import (
	"encoding/json"
	"math"
	"math/big"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// Request parsing for the telemetry routes: pydantic's lax int/float string
// grammar (vectors in testdata/wbcrud/telemetry_vectors.json come from the real
// pydantic) and the BudgetRequest body.

// BeforeIDMax and LeadIDMax are the int4 range limits FastAPI never validates:
// a larger value reaches Postgres as an out-of-range integer and answers 500.
// Go answers 422 less_than_equal instead (documented deviation).
var (
	BeforeIDMax int64 = math.MaxInt32
	LeadIDMax   int64 = math.MaxInt32
	LeadIDMin   int64 = math.MinInt32
)

var (
	intRE     = regexp.MustCompile(`^[+-]?[0-9]+(_[0-9]+)*(\.0+)?$`)
	floatRE   = regexp.MustCompile(`^[+-]?([0-9]+\.?[0-9]*|\.[0-9]+)([eE][+-]?[0-9]+)?$`)
	nonFinite = regexp.MustCompile(`(?i)^[+-]?(inf|infinity|nan)$`)
)

// ParseIntParam parses a query/path string like pydantic's lax int: surrounding
// whitespace ignored, an optional sign, digits with single underscores between
// them, and a zero fraction ("2.0"). Anything else (exponents, "1.5") is invalid.
func ParseIntParam(s string) (*big.Int, bool) {
	s = strings.TrimSpace(s)
	if !intRE.MatchString(s) {
		return nil, false
	}
	if i := strings.IndexByte(s, '.'); i >= 0 {
		s = s[:i]
	}
	n, ok := new(big.Int).SetString(strings.ReplaceAll(s, "_", ""), 10)
	return n, ok
}

// QueryInt reads an integer query parameter in FastAPI's order of checks:
// absent -> def (0 and no error when def is nil: the caller tests q.Has), the
// last repeated value wins, int_parsing, then ge / le. Bounds are nil when absent.
func QueryInt(q url.Values, name string, def *int64, ge, le *int64) (int64, *httpx.FieldError) {
	vals, present := q[name]
	if !present {
		if def != nil {
			return *def, nil
		}
		return 0, nil
	}
	return IntValue(vals[len(vals)-1], []any{"query", name}, ge, le)
}

// IntValue validates one raw string for an int field with optional ge/le bounds.
func IntValue(raw string, loc []any, ge, le *int64) (int64, *httpx.FieldError) {
	n, ok := ParseIntParam(raw)
	if !ok {
		return 0, &httpx.FieldError{Type: "int_parsing", Loc: loc, Input: raw,
			Msg: "Input should be a valid integer, unable to parse string as an integer"}
	}
	if ge != nil && n.Cmp(big.NewInt(*ge)) < 0 {
		return 0, &httpx.FieldError{Type: "greater_than_equal", Loc: loc, Input: raw,
			Msg: "Input should be greater than or equal to " + strconv.FormatInt(*ge, 10), Ctx: map[string]any{"ge": *ge}}
	}
	if le != nil && n.Cmp(big.NewInt(*le)) > 0 {
		return 0, &httpx.FieldError{Type: "less_than_equal", Loc: loc, Input: raw,
			Msg: "Input should be less than or equal to " + strconv.FormatInt(*le, 10), Ctx: map[string]any{"le": *le}}
	}
	if !n.IsInt64() {
		return 0, &httpx.FieldError{Type: "int_parsing", Loc: loc, Input: raw,
			Msg: "Input should be a valid integer, unable to parse string as an integer"}
	}
	return n.Int64(), nil
}

// parseFloatString applies pydantic's lax float-from-string rules: whitespace
// trimmed, underscores tolerated except leading, trailing or doubled.
// kind: "" ok, "float_parsing", "finite_number".
func parseFloatString(s string) (float64, string) {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "_") {
		if s[0] == '_' || s[len(s)-1] == '_' || strings.Contains(s, "__") {
			return 0, "float_parsing"
		}
		s = strings.ReplaceAll(s, "_", "")
	}
	if s == "" {
		return 0, "float_parsing"
	}
	if nonFinite.MatchString(s) {
		return 0, "finite_number"
	}
	if !floatRE.MatchString(s) {
		return 0, "float_parsing"
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil && math.IsInf(f, 0) {
		return 0, "finite_number"
	}
	return f, ""
}

// budgetErr builds the BudgetRequest.max_usd validation error.
func budgetErr(typ, msg string, input any) []httpx.FieldError {
	return []httpx.FieldError{{Type: typ, Loc: bodyLoc("max_usd"), Msg: msg, Input: input}}
}

// ParseBudgetBody validates BudgetRequest{max_usd: float = 0.0, ge=0, allow_inf_nan=False}.
// A JSON NaN/Infinity literal is invalid JSON here (FastAPI parses it, then fails
// to serialise its own 422: a 500).
func ParseBudgetBody(raw []byte) (float64, []httpx.FieldError) {
	doc, errs := decodeBody(raw)
	if errs != nil {
		return 0, errs
	}
	if doc == nil {
		return 0, []httpx.FieldError{missingErr(bodyLoc(), nil)}
	}
	o, ok := doc.(*Obj)
	if !ok {
		return 0, notObjectErr(doc)
	}
	v, present := o.Get("max_usd")
	if !present {
		return 0, nil
	}
	var f float64
	switch t := v.(type) {
	case bool:
		if t {
			f = 1
		}
	case json.Number:
		s := string(t)
		if !strings.ContainsAny(s, ".eE") {
			if s == "-0" {
				s = "0"
			}
			bf, _ := new(big.Float).SetString(s)
			f, _ = bf.Float64()
			if math.IsInf(f, 0) {
				return 0, budgetErr("finite_number", "Input should be a finite number", t)
			}
		} else {
			var err error
			f, err = strconv.ParseFloat(s, 64)
			if err != nil && math.IsInf(f, 0) {
				return 0, budgetErr("finite_number", "Input should be a finite number", t)
			}
		}
	case string:
		var kind string
		f, kind = parseFloatString(t)
		switch kind {
		case "float_parsing":
			return 0, budgetErr("float_parsing", "Input should be a valid number, unable to parse string as a number", t)
		case "finite_number":
			return 0, budgetErr("finite_number", "Input should be a finite number", t)
		}
	default:
		return 0, budgetErr("float_type", "Input should be a valid number", v)
	}
	if f < 0 {
		e := budgetErr("greater_than_equal", "Input should be greater than or equal to 0", v)
		e[0].Ctx = map[string]any{"ge": json.Number("0.0")}
		return 0, e
	}
	return f, nil
}
