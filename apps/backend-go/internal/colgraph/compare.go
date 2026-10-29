package colgraph

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

var (
	decRe   = regexp.MustCompile(`^([+-]?)([0-9]*)(?:\.([0-9]*))?(?:[eE]([+-]?[0-9]+))?$`)
	specRe  = regexp.MustCompile(`(?i)^[+-]?(inf|infinity|s?nan[0-9]*)$`)
	floatRe = regexp.MustCompile(`^[+-]?([0-9]+\.?[0-9]*|\.[0-9]+)([eE][+-]?[0-9]+)?$`)
)

const (
	decEmax  = 999999999999999999
	decEtiny = -1999999999999999997
)

// asciiDigits maps Unicode decimal digits (Nd) to ASCII and reports whether
// every underscore sits between two digits (float() rules).
func asciiDigits(s string) (string, bool) {
	rs := []rune(s)
	isDigit := func(r rune) bool { return r < 128 && r >= '0' && r <= '9' || r >= 128 && unicode.Is(unicode.Nd, r) }
	var b strings.Builder
	underscoresOK := true
	for i, r := range rs {
		switch {
		case r == '_':
			if i == 0 || i == len(rs)-1 || !isDigit(rs[i-1]) || !isDigit(rs[i+1]) {
				underscoresOK = false
			}
			b.WriteRune(r)
		case r >= 128 && unicode.Is(unicode.Nd, r):
			n := 0
			for x := r - 1; unicode.Is(unicode.Nd, x); x-- {
				n++
			}
			b.WriteByte(byte('0' + n%10))
		default:
			b.WriteRune(r)
		}
	}
	return b.String(), underscoresOK
}

type decimal struct {
	neg    bool
	coef   string // digits without leading/trailing zeros; "" is zero
	adj    int64  // adjusted exponent
	finite bool
}

// parseDecimal is decimal.Decimal(str). ok is false when Python raises.
func parseDecimal(s string) (decimal, bool) {
	s = pyStrip(s)
	s, _ = asciiDigits(s)
	s = strings.ReplaceAll(s, "_", "")
	if specRe.MatchString(s) {
		return decimal{}, true
	}
	m := decRe.FindStringSubmatch(s)
	if m == nil || m[2]+m[3] == "" {
		return decimal{}, false
	}
	var exp int64
	if m[4] != "" {
		d := strings.TrimLeft(strings.TrimLeft(m[4], "+-"), "0")
		if len(d) > 19 {
			return decimal{}, false
		}
		e, err := strconv.ParseInt(m[4], 10, 64)
		if err != nil {
			return decimal{}, false
		}
		exp = e
	}
	frac := int64(len(m[3]))
	coef := strings.TrimLeft(m[2]+m[3], "0")
	nd := int64(len(coef))
	if nd == 0 {
		nd = 1
	}
	exp -= frac
	adj := exp + nd - 1
	if adj > decEmax || exp < decEtiny {
		return decimal{}, false
	}
	return decimal{neg: m[1] == "-", coef: strings.TrimRight(coef, "0"), adj: adj, finite: true}, true
}

func (d decimal) sign() int {
	switch {
	case d.coef == "":
		return 0
	case d.neg:
		return -1
	}
	return 1
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmpDecimal(a, b decimal) int {
	sa, sb := a.sign(), b.sign()
	if sa != sb {
		return cmpInt(int64(sa), int64(sb))
	}
	if sa == 0 {
		return 0
	}
	c := cmpInt(a.adj, b.adj)
	if c == 0 {
		c = strings.Compare(a.coef, b.coef) // prefix sorts first, as a shorter coefficient
	}
	return c * sa
}

// parseFloatStrict is float(str); ok false where Python raises ValueError.
func parseFloatStrict(s string) (float64, bool) {
	// float() converts non-ASCII spaces to ' ' but only strips the six ASCII
	// whitespace characters (\x1c-\x1f stay and make it fail).
	s = strings.TrimFunc(s, func(r rune) bool {
		return r == ' ' || (r >= 0x09 && r <= 0x0d) || (r >= 128 && pyIsSpace(r))
	})
	s, ok := asciiDigits(s)
	if !ok {
		return 0, false
	}
	s = strings.ReplaceAll(s, "_", "")
	switch strings.ToLower(strings.TrimLeft(s, "+-")) {
	case "inf", "infinity":
		if strings.HasPrefix(s, "-") {
			return math.Inf(-1), true
		}
		return math.Inf(1), true
	case "nan":
		return math.NaN(), true
	}
	if !floatRe.MatchString(s) {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil && !math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

func cmpOK(c int, op string) bool {
	switch op {
	case "==":
		return c == 0
	case "!=":
		return c != 0
	case ">":
		return c > 0
	case "<":
		return c < 0
	case ">=":
		return c >= 0
	case "<=":
		return c <= 0
	}
	return false
}

// compareValues is _compare: numeric (Decimal, float for NaN/Infinity) when
// both sides parse, otherwise string comparison; unknown operator is false.
func compareValues(left, right, op string) bool {
	if l, ok := parseDecimal(left); ok {
		if r, ok := parseDecimal(right); ok {
			if l.finite && r.finite {
				return cmpOK(cmpDecimal(l, r), op)
			}
			lf, lok := parseFloatStrict(left)
			rf, rok := parseFloatStrict(right)
			if lok && rok {
				switch op {
				case "==":
					return lf == rf
				case "!=":
					return lf != rf
				case ">":
					return lf > rf
				case "<":
					return lf < rf
				case ">=":
					return lf >= rf
				case "<=":
					return lf <= rf
				}
				return false
			}
		}
	}
	return cmpOK(strings.Compare(left, right), op)
}
