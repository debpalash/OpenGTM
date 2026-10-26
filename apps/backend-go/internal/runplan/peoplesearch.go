package runplan

import (
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"strings"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
)

// people_search source configuration: a port of
// apps/api/services/workbook/people_search.py validate_people_search_config.
// The scrapers that consume the config stay in Python (decision D6); Go only
// validates and normalises it when the column is created.

const (
	// DefaultMaxPerCompany and the other limits are the module constants of people_search.py.
	DefaultMaxPerCompany = 10
	MaxPerCompanyCap     = 25
	DefaultMaxSearches   = 100
	MaxSearchesCap       = 100
)

// PeopleSearchConfig is the normalised config, in Python's return key order.
type PeopleSearchConfig struct {
	Companies     []string
	FromColumn    string
	Titles        []string
	Seniority     string
	Geo           string
	MaxPerCompany int64
	MaxSearches   int64
}

// ErrPeopleSearch is a ValueError of validate_people_search_config; its text is
// Python's message (the route answers it as the 422 detail).
type ErrPeopleSearch string

func (e ErrPeopleSearch) Error() string { return string(e) }

var errNeedCompanies = ErrPeopleSearch("people_search requires 'companies' or 'from_column'")

// normScalar maps json.Number to int64 / float64 so Python str()/int() rules apply.
func normScalar(v any) any {
	n, ok := v.(json.Number)
	if !ok {
		return v
	}
	s := string(n)
	if !strings.ContainsAny(s, ".eE") {
		if i, err := n.Int64(); err == nil {
			return i
		}
		if b, ok := new(big.Int).SetString(s, 10); ok {
			return b
		}
	}
	f, _ := n.Float64()
	return f
}

// truthy is bool(v) for the scalar shapes a JSON body can carry.
func psTruthy(v any) bool {
	switch x := normScalar(v).(type) {
	case *big.Int:
		return x.Sign() != 0
	default:
		return manifest.PyTruthy(x)
	}
}

// pyStrScalar is str(v).
func pyStrScalar(v any) string {
	if b, ok := normScalar(v).(*big.Int); ok {
		return b.String()
	}
	return manifest.PyStr(normScalar(v))
}

// strList is [str(c).strip() for c in (v or []) if c is not None and str(c).strip()].
// Python iterates whatever it is given; a list iterates its items and a string its
// characters (reachable only by a direct call: the route has a list[str] model).
func strList(v any) []string {
	out := []string{}
	add := func(s string) {
		if s = manifest.PyStrip(s); s != "" {
			out = append(out, s)
		}
	}
	if !psTruthy(v) {
		return out
	}
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			if e != nil {
				add(pyStrScalar(e))
			}
		}
	case []string:
		for _, e := range x {
			add(e)
		}
	case string:
		for _, r := range x {
			add(string(r))
		}
	}
	return out
}

// pyInt is int(v) for a JSON value; ok is false where Python raises TypeError or ValueError.
func pyInt(v any) (*big.Int, bool) {
	switch x := normScalar(v).(type) {
	case bool:
		if x {
			return big.NewInt(1), true
		}
		return big.NewInt(0), true
	case int64:
		return big.NewInt(x), true
	case int:
		return big.NewInt(int64(x)), true
	case *big.Int:
		return x, true
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil, false
		}
		i, _ := new(big.Float).SetFloat64(math.Trunc(x)).Int(nil)
		return i, true
	case string:
		return pyIntString(x)
	}
	return nil, false
}

// pyIntString is int(str): surrounding whitespace, one sign, ASCII digits with
// single underscores between digits. (Non-ASCII decimal digits, which Python also
// accepts, are rejected: no client sends them and the route model never produces them.)
func pyIntString(s string) (*big.Int, bool) {
	s = manifest.PyStrip(s)
	neg := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg = s[0] == '-'
		s = s[1:]
	}
	if s == "" || !utf8.ValidString(s) {
		return nil, false
	}
	var digits strings.Builder
	prevUnderscore := true // a leading underscore is invalid
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			digits.WriteByte(c)
			prevUnderscore = false
		case c == '_' && !prevUnderscore:
			prevUnderscore = true
		default:
			return nil, false
		}
	}
	if prevUnderscore {
		return nil, false
	}
	n, ok := new(big.Int).SetString(digits.String(), 10)
	if !ok {
		return nil, false
	}
	if neg {
		n.Neg(n)
	}
	return n, true
}

// intCfg is the nested _int_cfg: default for a missing / None / "" value, else
// max(1, min(int(val), cap)), or ValueError "<key> must be an integer".
func intCfg(cfg map[string]any, key string, def, limit int64) (int64, error) {
	val, present := cfg[key]
	if !present || val == nil {
		return def, nil
	}
	if s, ok := val.(string); ok && s == "" {
		return def, nil
	}
	n, ok := pyInt(val)
	if !ok {
		return 0, ErrPeopleSearch(key + " must be an integer")
	}
	if n.Cmp(big.NewInt(limit)) > 0 {
		return limit, nil
	}
	if n.Cmp(big.NewInt(1)) < 0 {
		return 1, nil
	}
	return n.Int64(), nil
}

// ValidatePeopleSearchConfig validates and normalises a people_search source
// config (a JSON-shaped map; numbers may be json.Number). The returned error is an
// ErrPeopleSearch whose text is Python's ValueError message.
func ValidatePeopleSearchConfig(cfg map[string]any) (PeopleSearchConfig, error) {
	out := PeopleSearchConfig{Companies: strList(cfg["companies"])}
	if v := cfg["from_column"]; psTruthy(v) {
		out.FromColumn = manifest.PyStrip(pyStrScalar(v))
	}
	if len(out.Companies) == 0 && out.FromColumn == "" {
		return PeopleSearchConfig{}, errNeedCompanies
	}
	out.Titles = strList(cfg["titles"])
	if v := cfg["seniority"]; psTruthy(v) {
		out.Seniority = manifest.PyStrip(pyStrScalar(v))
	}
	if v := cfg["geo"]; psTruthy(v) {
		out.Geo = manifest.PyStrip(pyStrScalar(v))
	}
	var err error
	if out.MaxPerCompany, err = intCfg(cfg, "max_per_company", DefaultMaxPerCompany, MaxPerCompanyCap); err != nil {
		return PeopleSearchConfig{}, err
	}
	if out.MaxSearches, err = intCfg(cfg, "max_searches", DefaultMaxSearches, MaxSearchesCap); err != nil {
		return PeopleSearchConfig{}, err
	}
	return out, nil
}

// IsPeopleSearchError reports whether err is a validation ValueError.
func IsPeopleSearchError(err error) (ErrPeopleSearch, bool) {
	var e ErrPeopleSearch
	if errors.As(err, &e) {
		return e, true
	}
	return "", false
}
