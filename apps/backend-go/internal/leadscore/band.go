package leadscore

import (
	"encoding/json"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
)

var (
	sizeBands = []string{"1-50", "51-200", "201-500", "500+"}
	// \s and \d are Unicode-aware in Python's str patterns.
	pySpace      = `[\s\x{1c}-\x{1f}\x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}]`
	negBareRe    = regexp.MustCompile(`^-` + pySpace + `*\p{Nd}[\p{Nd},]*$`)
	intsRe       = regexp.MustCompile(`\p{Nd}[\p{Nd},]*`)
	bigThreshold = [3]*big.Int{big.NewInt(50), big.NewInt(200), big.NewInt(500)}
)

func isPySpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)
}

// NormalizeBand is size_heuristic.normalize_band: any company_size value (str(value) semantics) into
// a canonical band, else "".
func NormalizeBand(v any) string {
	if v == nil {
		return ""
	}
	s := strings.TrimFunc(pyStr(v), isPySpace)
	if s == "" {
		return ""
	}
	for _, b := range sizeBands {
		if s == b {
			return s
		}
	}
	if negBareRe.MatchString(s) {
		return ""
	}
	nums := intsRe.FindAllString(s, -1)
	if len(nums) == 0 {
		return ""
	}
	var min *big.Int
	for _, n := range nums {
		i := parseDigits(strings.ReplaceAll(n, ",", ""))
		if min == nil || i.Cmp(min) < 0 {
			min = i
		}
	}
	// One value: use it; a range: the lower bound. Both are the minimum.
	return bandFromBig(min)
}

// parseDigits is int() over a run of Unicode decimal digits.
func parseDigits(s string) *big.Int {
	var b strings.Builder
	for _, r := range s {
		b.WriteByte(byte('0' + digitValue(r)))
	}
	n, _ := new(big.Int).SetString(b.String(), 10)
	return n
}

// digitValue is unicode.digit(r): Nd runs are contiguous blocks of ten starting at a zero.
func digitValue(r rune) int {
	if r >= '0' && r <= '9' {
		return int(r - '0')
	}
	start := r
	for unicode.IsDigit(start-1) && r-start < 9 {
		start--
	}
	return int(r-start) % 10
}

func bandFromBig(n *big.Int) string {
	switch {
	case n.Sign() <= 0:
		return ""
	case n.Cmp(bigThreshold[0]) <= 0:
		return "1-50"
	case n.Cmp(bigThreshold[1]) <= 0:
		return "51-200"
	case n.Cmp(bigThreshold[2]) <= 0:
		return "201-500"
	}
	return "500+"
}

// pyStr is str(v) for a decoded JSON / Go value.
func pyStr(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case *string:
		if x == nil {
			return "None"
		}
		return *x
	}
	return pyRepr(v)
}

func pyRepr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case string:
		q := "'"
		if strings.Contains(x, "'") && !strings.Contains(x, `"`) {
			q = `"`
		}
		s := strings.NewReplacer(`\`, `\\`, "\n", `\n`, "\r", `\r`, "\t", `\t`).Replace(x)
		if q == "'" {
			s = strings.ReplaceAll(s, "'", `\'`)
		}
		return q + s + q
	case json.Number:
		t := x.String()
		if !strings.ContainsAny(t, ".eE") {
			return t
		}
		f, _ := strconv.ParseFloat(t, 64)
		return pyFloat(f)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return pyFloat(x)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = pyRepr(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		// Decoded maps carry no order; digits are all that matters here.
		parts := make([]string, 0, len(x))
		for k, e := range x {
			parts = append(parts, pyRepr(k)+": "+pyRepr(e))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return ""
}

func pyFloat(f float64) string {
	switch {
	case f != f:
		return "nan"
	case f > 1.7976931348623157e308:
		return "inf"
	case f < -1.7976931348623157e308:
		return "-inf"
	}
	s, err := canonjson.FloatRepr(f)
	if err != nil {
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	return s
}
