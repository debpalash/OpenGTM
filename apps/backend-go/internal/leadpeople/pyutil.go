package leadpeople

import (
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// pyS is Python's str \s (str.isspace) as a regexp class body.
const pyS = "\\t\\n\\x0b\\f\\r\\x1c-\\x1f\\x85\\p{Z}"

// pyW is Python's str \w without the underscore-free letters/digits split.
const pyW = `\p{L}\p{N}_`

var wsRun = regexp.MustCompile("[" + pyS + "]+")

// collapse is re.sub(r"\s+", " ", s).
func collapse(s string) string { return wsRun.ReplaceAllString(s, " ") }

// clean is re.sub(r"\s+", " ", s.strip()).
func clean(s string) string { return collapse(providers.PyStrip(s)) }

// trunc is s[:n] on code points.
func trunc(s string, n int) string { return providers.TruncRunes(s, n) }

// valStr is `str(v or "")` for a decoded JSON value.
func valStr(v any) string {
	if !providers.Truthy(v) {
		return ""
	}
	return providers.Str(v)
}

// getStr is `str(m.get(key) or "")`.
func getStr(m map[string]any, key string) string { return valStr(m[key]) }

// firstStr is `str(a or b or c or "")` over the keys, in order.
func firstStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if providers.Truthy(m[k]) {
			return providers.Str(m[k])
		}
	}
	return ""
}

// toFloat is float(v or 0): numbers and numeric strings; anything else is 0
// (Python raised ValueError/TypeError; deviation, see the package doc).
func toFloat(v any) float64 {
	if !providers.Truthy(v) {
		return 0
	}
	if f, ok := providers.Num(v); ok {
		return f
	}
	if s, ok := v.(string); ok {
		if f, err := strconv.ParseFloat(providers.PyStrip(s), 64); err == nil {
			return f
		}
	}
	return 0
}

// toInt is int(v or 0) for numbers and integer strings; otherwise 0.
func toInt(v any) int64 {
	if !providers.Truthy(v) {
		return 0
	}
	switch t := v.(type) {
	case int64:
		return t
	case int:
		return int64(t)
	case float64:
		return int64(t)
	case string:
		if n, err := strconv.ParseInt(providers.PyStrip(t), 10, 64); err == nil {
			return n
		}
	}
	return 0
}

// round4 is round(x, 4) (correctly rounded, ties to even like Python).
func round4(x float64) float64 {
	f, err := strconv.ParseFloat(strconv.FormatFloat(x, 'f', 4, 64), 64)
	if err != nil {
		return x
	}
	return f
}

// roundInt is round(x) for a float (banker's rounding).
func roundInt(x float64) int64 { return int64(math.RoundToEven(x)) }

// isoDate is datetime.date().isoformat().
func isoDate(t time.Time) string { return t.UTC().Format("2006-01-02") }

// isoFull is datetime.isoformat() for an aware UTC datetime.
func isoFull(t time.Time) string {
	t = t.UTC()
	if t.Nanosecond()/1000 != 0 {
		return t.Format("2006-01-02T15:04:05") + fmt.Sprintf(".%06d", t.Nanosecond()/1000) + "+00:00"
	}
	return t.Format("2006-01-02T15:04:05") + "+00:00"
}

func isWordRune(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) }

func wordBefore(s string, i int) bool {
	if i <= 0 {
		return false
	}
	r, _ := utf8.DecodeLastRuneInString(s[:i])
	return isWordRune(r)
}

func wordAt(s string, i int) bool {
	if i >= len(s) {
		return false
	}
	r, _ := utf8.DecodeRuneInString(s[i:])
	return isWordRune(r)
}

// boundary is Python's (Unicode) \b at byte offset i.
func boundary(s string, i int) bool { return wordBefore(s, i) != wordAt(s, i) }

// litRe is a case-insensitive literal.
func litRe(lit string) *regexp.Regexp {
	return regexp.MustCompile("(?i)" + regexp.QuoteMeta(lit))
}

// anchoredLit is a case-insensitive literal anchored at the start.
func anchoredLit(lit string) *regexp.Regexp { return anchored(regexp.QuoteMeta(lit)) }

// anchored compiles `(?i)^pat` for matching at a fixed offset.
func anchored(pat string) *regexp.Regexp { return regexp.MustCompile("(?i)^(?:" + pat + ")") }

// matchAt matches re (anchored) at byte offset p and returns the end offset.
func matchAt(re *regexp.Regexp, s string, p int) (int, bool) {
	if p > len(s) {
		return 0, false
	}
	loc := re.FindStringIndex(s[p:])
	if loc == nil {
		return 0, false
	}
	return p + loc[1], true
}

// boundedAt is `\bLIT\b` anchored at p.
func boundedAt(re *regexp.Regexp, s string, p int) (int, bool) {
	end, ok := matchAt(re, s, p)
	if !ok || !boundary(s, p) || !boundary(s, end) {
		return 0, false
	}
	return end, true
}

// occurrences are the start/end offsets of every `\bLIT\b` (overlaps allowed,
// leftmost first) from offset from.
func occurrences(lit *regexp.Regexp, s string, from int) [][2]int {
	var out [][2]int
	for from <= len(s) {
		loc := lit.FindStringIndex(s[from:])
		if loc == nil {
			break
		}
		st, en := from+loc[0], from+loc[1]
		if boundary(s, st) && boundary(s, en) {
			out = append(out, [2]int{st, en})
		}
		_, w := utf8.DecodeRuneInString(s[st:])
		if w == 0 {
			w = 1
		}
		from = st + w
	}
	return out
}

// gapStarts lists the byte offsets e, e+1 rune, ... at most gap runes after e
// without crossing a newline (what `.{0,gap}` can consume).
func gapStarts(s string, e, gap int) []int {
	out := []int{e}
	p := e
	for i := 0; i < gap && p < len(s); i++ {
		r, w := utf8.DecodeRuneInString(s[p:])
		if r == '\n' {
			break
		}
		p += w
		out = append(out, p)
	}
	return out
}

func anySubstr(s string, terms []string) bool {
	for _, t := range terms {
		if strings.Contains(s, t) {
			return true
		}
	}
	return false
}

func dedupStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// anyList converts []string / []any / nil to []any (the JSON-decoded shape).
func anyList(v any) []any {
	switch t := v.(type) {
	case nil:
		return []any{}
	case []any:
		return t
	case []string:
		out := make([]any, len(t))
		for i, s := range t {
			out[i] = s
		}
		return out
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Slice {
		out := make([]any, rv.Len())
		for i := range out {
			out[i] = rv.Index(i).Interface()
		}
		return out
	}
	return []any{}
}

// pyURL is the (hostname, path) of urllib.parse.urlparse(raw), the only parts
// person_entity_id reads. It never fails: unparsable input gives no hostname.
func pyURL(raw string) (host, path string) {
	rest := raw
	if i := strings.IndexByte(rest, ':'); i > 0 {
		sch := rest[:i]
		ok := unicode.IsLetter(rune(sch[0])) && sch[0] < 128
		for j := 0; j < len(sch) && ok; j++ {
			c := sch[j]
			ok = c < 128 && (unicode.IsLetter(rune(c)) || unicode.IsDigit(rune(c)) || c == '+' || c == '-' || c == '.')
		}
		if ok {
			rest = rest[i+1:]
		}
	}
	netloc := ""
	if strings.HasPrefix(rest, "//") {
		rest = rest[2:]
		end := len(rest)
		for i := 0; i < len(rest); i++ {
			if rest[i] == '/' || rest[i] == '?' || rest[i] == '#' {
				end = i
				break
			}
		}
		netloc, rest = rest[:end], rest[end:]
	}
	if i := strings.IndexByte(rest, '#'); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.IndexByte(rest, '?'); i >= 0 {
		rest = rest[:i]
	}
	// urlparse (not urlsplit) cuts ";params" off the last path segment.
	if i := strings.IndexByte(rest, ';'); i >= 0 {
		slash := strings.LastIndexByte(rest, '/')
		if j := strings.IndexByte(rest[slash+1:], ';'); j >= 0 {
			rest = rest[:slash+1+j]
		}
	}
	path = rest
	if netloc == "" {
		return "", path
	}
	h := netloc
	if i := strings.LastIndexByte(h, '@'); i >= 0 {
		h = h[i+1:]
	}
	if strings.HasPrefix(h, "[") {
		if j := strings.IndexByte(h, ']'); j > 0 {
			h = h[1:j]
		} else {
			return "", path
		}
	} else if i := strings.IndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	return strings.ToLower(h), path
}
