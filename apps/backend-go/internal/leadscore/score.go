package leadscore

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Lead is the accessor Score reads. Get returns the field's decoded value (nil, string, bool,
// json.Number / int / float64, []any, map[string]any) and false for an absent key. leadsread.Lead
// satisfies it; MapLead covers Lead.from_dict inline bodies (unknown keys are simply never read).
//
// Wrong-typed values are scored as a Python dataclass would: they are not validated at
// construction, so e.g. email=5 raises TypeError inside score_lead. ScoreE reports that as a
// *PyError; the inline-body route answers 422 for it.
type Lead interface {
	Get(key string) (any, bool)
}

// MapLead is a Lead over a decoded JSON object.
type MapLead map[string]any

// Get implements Lead.
func (m MapLead) Get(key string) (any, bool) { v, ok := m[key]; return v, ok }

// PyError is the Python exception score_lead would raise for a wrong-typed field.
type PyError struct{ Kind, Msg string }

func (e *PyError) Error() string { return e.Kind + ": " + e.Msg }

// Score is score_lead; a wrong-typed field (where Python raises) scores 0, use ScoreE to see the error.
func Score(l Lead, reliability *float64) int {
	n, _ := ScoreE(l, reliability)
	return n
}

// ScoreE is score_lead, returning *PyError where Python would raise.
func ScoreE(l Lead, reliability *float64) (int, error) {
	get := func(k string) any { v, _ := l.Get(k); return v }
	score := 0

	if hasValue(get("website")) {
		score += weights["has_website"]
	}
	hasEmail, err := emailPresent(get("email"))
	if err != nil {
		return 0, err
	}
	if hasEmail {
		score += weights["has_email"]
	}
	hasPhone := hasValue(get("phone"))
	if hasPhone {
		score += weights["has_phone"]
	}
	hasLinkedin := hasValue(get("linkedin_url"))
	if hasLinkedin {
		score += weights["has_linkedin"]
	}

	switch NormalizeBand(get("company_size")) {
	case "51-200", "201-500", "500+":
		score += weights["company_size_large"]
	}

	if pyTruthy(get("specialization")) {
		specLower, err := pyLowerOf(get("specialization"))
		if err != nil {
			return 0, err
		}
		matched := false
		for _, pref := range icp.PreferredSpecialization {
			p := pyLower(pref)
			if strings.Contains(specLower, p) || strings.Contains(p, specLower) {
				score += weights["specialization_match"]
				matched = true
				break
			}
		}
		if !matched {
			for _, ind := range icp.TargetIndustries {
				p := pyLower(ind)
				if strings.Contains(specLower, p) || strings.Contains(p, specLower) {
					score += weights["specialization_match"] / 2
					break
				}
			}
		}
	}

	if pyTruthy(get("industry_tags")) {
		tags, err := pyLowerOf(get("industry_tags"))
		if err != nil {
			return 0, err
		}
		matches := 0
		for _, pref := range icp.PreferredSpecialization {
			if strings.Contains(tags, pyLower(pref)) {
				matches++
			}
		}
		if matches >= 2 {
			score += 5
		}
	}

	if city, ok := get("city").(string); ok {
		if containsStr(icp.Tier1Cities, city) {
			score += weights["tier1_city"]
		} else if containsStr(icp.TargetCities, city) {
			score += weights["tier1_city"] / 2
		}
	}

	if hasValue(get("contact_person")) {
		score += weights["decision_maker_found"]
	}

	if dm, ok := get("decision_makers").(string); ok && dm != "" {
		if n, ok := jsonLen(dm); ok && n >= 2 {
			score += 5
		}
	}

	contacts := 0
	for _, b := range []bool{hasEmail, hasPhone, hasLinkedin, pyTruthy(get("twitter_url"))} {
		if b {
			contacts++
		}
	}
	if contacts >= 3 {
		score += weights["multiple_contacts"]
	}

	if pyTruthy(get("founded_year")) {
		score += 3
	}
	if pyTruthy(get("revenue_range")) {
		score += 3
	}
	if pyTruthy(get("glassdoor_rating")) {
		score += 2
	}

	if score > 100 {
		score = 100
	}
	return ApplyReliability(score, reliability), nil
}

// ApplyReliability is _apply_source_reliability: clamp(round(base + SWING*(r-0.5)), 0, 100) with
// Python's banker's round; nil is the identity.
func ApplyReliability(score int, r *float64) int {
	if r == nil {
		return score
	}
	adjusted := math.RoundToEven(float64(score) + ReliabilitySwing*(*r-0.5))
	if adjusted < 0 {
		return 0
	}
	if adjusted > 100 {
		return 100
	}
	return int(adjusted)
}

// hasValue is `bool(x) and x not in ("N/A", "nan", "")`.
func hasValue(v any) bool {
	if !pyTruthy(v) {
		return false
	}
	if s, ok := v.(string); ok {
		return s != "N/A" && s != "nan"
	}
	return true
}

// emailPresent is Lead.has_email, whose `"@" in self.email` raises TypeError for a number.
func emailPresent(v any) (bool, error) {
	if !hasValue(v) {
		return false, nil
	}
	switch x := v.(type) {
	case string:
		return strings.Contains(x, "@"), nil
	case []any:
		for _, e := range x {
			if s, ok := e.(string); ok && s == "@" {
				return true, nil
			}
		}
		return false, nil
	case map[string]any:
		_, ok := x["@"]
		return ok, nil
	}
	return false, &PyError{"TypeError", fmt.Sprintf("argument of type '%s' is not iterable", pyTypeName(v))}
}

func pyTypeName(v any) string {
	switch v.(type) {
	case bool:
		return "bool"
	case json.Number:
		if strings.ContainsAny(v.(json.Number).String(), ".eE") {
			return "float"
		}
		return "int"
	case float64:
		return "float"
	}
	return "int"
}

func pyTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case *string:
		return x != nil && *x != ""
	case int:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	case json.Number:
		f, _ := strconv.ParseFloat(x.String(), 64)
		return f != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// pyLowerOf is `value.lower()`: AttributeError for anything but a str.
func pyLowerOf(v any) (string, error) {
	if s, ok := v.(string); ok {
		return pyLower(s), nil
	}
	return "", &PyError{"AttributeError", fmt.Sprintf("'%s' object has no attribute 'lower'", pyTypeNameAny(v))}
}

func pyTypeNameAny(v any) string {
	switch v.(type) {
	case []any:
		return "list"
	case map[string]any:
		return "dict"
	}
	return pyTypeName(v)
}

// pyLower is str.lower; Go's ToLower differs for U+0130 only among non-final-sigma cases.
func pyLower(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "İ", "i̇"))
}

func containsStr(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

// jsonLen is len(json.loads(s)) with Python's json grammar (NaN/Infinity accepted, duplicate object
// keys collapse). ok is false for invalid JSON and for values without a len (number, bool, null).
func jsonLen(s string) (int, bool) {
	p := &jparser{s: s}
	p.ws()
	n, ok := p.value(0)
	if !ok {
		return 0, false
	}
	p.ws()
	if p.i != len(p.s) || n < 0 {
		return 0, false
	}
	return n, true
}

type jparser struct {
	s string
	i int
}

func (p *jparser) ws() {
	for p.i < len(p.s) && strings.IndexByte(" \t\n\r", p.s[p.i]) >= 0 {
		p.i++
	}
}

// value parses one value and returns its len(), or -1 when it has none.
func (p *jparser) value(depth int) (int, bool) {
	if depth > 900 || p.i >= len(p.s) {
		return 0, false
	}
	switch c := p.s[p.i]; {
	case c == '"':
		_, n, ok := p.str()
		return n, ok
	case c == '[':
		p.i++
		p.ws()
		if p.i < len(p.s) && p.s[p.i] == ']' {
			p.i++
			return 0, true
		}
		count := 0
		for {
			p.ws()
			if _, ok := p.value(depth + 1); !ok {
				return 0, false
			}
			count++
			p.ws()
			if p.i >= len(p.s) {
				return 0, false
			}
			if p.s[p.i] == ',' {
				p.i++
				continue
			}
			if p.s[p.i] == ']' {
				p.i++
				return count, true
			}
			return 0, false
		}
	case c == '{':
		p.i++
		p.ws()
		keys := map[string]bool{}
		if p.i < len(p.s) && p.s[p.i] == '}' {
			p.i++
			return 0, true
		}
		for {
			p.ws()
			if p.i >= len(p.s) || p.s[p.i] != '"' {
				return 0, false
			}
			k, _, ok := p.str()
			if !ok {
				return 0, false
			}
			p.ws()
			if p.i >= len(p.s) || p.s[p.i] != ':' {
				return 0, false
			}
			p.i++
			p.ws()
			if _, ok := p.value(depth + 1); !ok {
				return 0, false
			}
			keys[k] = true
			p.ws()
			if p.i >= len(p.s) {
				return 0, false
			}
			if p.s[p.i] == ',' {
				p.i++
				continue
			}
			if p.s[p.i] == '}' {
				p.i++
				return len(keys), true
			}
			return 0, false
		}
	}
	for _, lit := range []string{"true", "false", "null", "NaN", "Infinity", "-Infinity"} {
		if strings.HasPrefix(p.s[p.i:], lit) {
			p.i += len(lit)
			return -1, true
		}
	}
	return p.number()
}

func (p *jparser) number() (int, bool) {
	j := p.i
	if j < len(p.s) && p.s[j] == '-' {
		j++
	}
	digits := func() int {
		st := j
		for j < len(p.s) && p.s[j] >= '0' && p.s[j] <= '9' {
			j++
		}
		return j - st
	}
	if j < len(p.s) && p.s[j] == '0' {
		j++
	} else if digits() == 0 {
		return 0, false
	}
	if j+1 < len(p.s) && p.s[j] == '.' && p.s[j+1] >= '0' && p.s[j+1] <= '9' {
		j++
		digits()
	}
	if j < len(p.s) && (p.s[j] == 'e' || p.s[j] == 'E') {
		k := j + 1
		if k < len(p.s) && (p.s[k] == '+' || p.s[k] == '-') {
			k++
		}
		if k < len(p.s) && p.s[k] >= '0' && p.s[k] <= '9' {
			j = k
			digits()
		}
	}
	p.i = j
	return -1, true
}

// str parses a JSON string, returning a collision-free key and its length in code points
// (a valid surrogate pair counts once, as in Python).
func (p *jparser) str() (string, int, bool) {
	p.i++
	var units []rune
	for p.i < len(p.s) {
		c := p.s[p.i]
		switch {
		case c == '"':
			p.i++
			n := 0
			var key strings.Builder
			for i := 0; i < len(units); i++ {
				if units[i] >= 0xD800 && units[i] < 0xDC00 && i+1 < len(units) && units[i+1] >= 0xDC00 && units[i+1] < 0xE000 {
					fmt.Fprintf(&key, "%x,", 0x10000+(units[i]-0xD800)<<10+(units[i+1]-0xDC00))
					i++
				} else {
					fmt.Fprintf(&key, "%x,", units[i])
				}
				n++
			}
			return key.String(), n, true
		case c < 0x20:
			return "", 0, false
		case c == '\\':
			p.i++
			if p.i >= len(p.s) {
				return "", 0, false
			}
			e := p.s[p.i]
			switch e {
			case '"', '\\', '/':
				units = append(units, rune(e))
			case 'b':
				units = append(units, '\b')
			case 'f':
				units = append(units, '\f')
			case 'n':
				units = append(units, '\n')
			case 'r':
				units = append(units, '\r')
			case 't':
				units = append(units, '\t')
			case 'u':
				if p.i+4 >= len(p.s) {
					return "", 0, false
				}
				v, err := strconv.ParseUint(p.s[p.i+1:p.i+5], 16, 32)
				if err != nil || strings.ContainsAny(p.s[p.i+1:p.i+5], "+-_") {
					return "", 0, false
				}
				units = append(units, rune(v))
				p.i += 4
			default:
				return "", 0, false
			}
			p.i++
		default:
			r, sz := decodeRune(p.s[p.i:])
			units = append(units, r)
			p.i += sz
		}
	}
	return "", 0, false
}

func decodeRune(s string) (rune, int) { return utf8.DecodeRuneInString(s) }
