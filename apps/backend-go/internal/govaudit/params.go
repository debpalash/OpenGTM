package govaudit

import (
	"math/big"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// MaxCursorLen is the FastAPI max_length of the cursor query parameter.
const MaxCursorLen = 1024

// ValidateListParams applies the router's Query constraints (cursor
// max_length=1024, limit int in [1,500], default 100) and returns FastAPI's
// 422 field errors, cursor first. limitRaw/hasLimit are the raw query value
// and whether it was present. Handlers call this before DecodeCursor/List so
// the body matches Python (string_too_long, int_parsing, greater_than_equal,
// less_than_equal).
func ValidateListParams(cursor, limitRaw string, hasLimit bool) (int, []httpx.FieldError) {
	var errs []httpx.FieldError
	if n := len([]rune(cursor)); n > MaxCursorLen {
		errs = append(errs, httpx.FieldError{Type: "string_too_long", Loc: []any{"query", "cursor"},
			Msg: "String should have at most 1024 characters", Input: cursor, Ctx: map[string]any{"max_length": MaxCursorLen}})
	}
	if !hasLimit {
		return DefaultLimit, errs
	}
	loc := []any{"query", "limit"}
	v, ok := parsePyInt(limitRaw)
	if !ok {
		errs = append(errs, httpx.FieldError{Type: "int_parsing", Loc: loc,
			Msg: "Input should be a valid integer, unable to parse string as an integer", Input: limitRaw})
		return DefaultLimit, errs
	}
	switch {
	case v.Cmp(big.NewInt(1)) < 0:
		errs = append(errs, httpx.FieldError{Type: "greater_than_equal", Loc: loc,
			Msg: "Input should be greater than or equal to 1", Input: limitRaw, Ctx: map[string]any{"ge": 1}})
	case v.Cmp(big.NewInt(MaxLimit)) > 0:
		errs = append(errs, httpx.FieldError{Type: "less_than_equal", Loc: loc,
			Msg: "Input should be less than or equal to 500", Input: limitRaw, Ctx: map[string]any{"le": MaxLimit}})
	default:
		return int(v.Int64()), errs
	}
	return DefaultLimit, errs
}

// parsePyInt accepts what pydantic v2 lax mode takes for an int query value:
// surrounding whitespace, a sign, single underscores between digits, and a
// ".0" style whole-number fraction. Anything else is not an int.
func parsePyInt(s string) (*big.Int, bool) {
	s = strings.Trim(s, " \t\n\r\v\f")
	neg := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg = s[0] == '-'
		s = s[1:]
	}
	ip, fp, hasFrac := strings.Cut(s, ".")
	if hasFrac && strings.Trim(fp, "0") != "" || hasFrac && fp == "" {
		return nil, false
	}
	if ip == "" || ip[0] == '_' || ip[len(ip)-1] == '_' || strings.Contains(ip, "__") {
		return nil, false
	}
	ip = strings.ReplaceAll(ip, "_", "")
	for _, c := range ip {
		if c < '0' || c > '9' {
			return nil, false
		}
	}
	v, ok := new(big.Int).SetString(ip, 10)
	if !ok {
		return nil, false
	}
	if neg {
		v.Neg(v)
	}
	return v, true
}
