// Package template ports the declarative connector mini-language from
// apps/api/services/leadgen/enrichment/declarative/template.py exactly:
//
//   - RenderString / RenderTemplate substitute {{input.x}},
//     {{input.x | default: y}} and ${env:VAR} in strings (nested structures
//     are walked; non-string leaves pass through).
//   - ProjectValue / ProjectResponse evaluate JSONPath-lite expressions
//     ("$.a.b", "$.list[0].x", "$.list[].x", "prefix $.path") against a
//     decoded JSON response.
//
// Values use the pycompat model (LoadJSON output, *pycompat.Map for objects),
// so interpolated numbers and booleans format exactly as Python's str() does.
//
// Known, documented deviation: Python's _lookup falls back to getattr() on
// non-dict values, so "{{input.name.upper}}" renders a bound-method repr that
// includes a memory address. The Go port returns nothing for attribute lookups
// on non-objects (the default applies), which is the only sane reading.
//
// Like the Python original, ${env:VAR} is expanded after {{...}} substitution,
// so an input value containing "${env:VAR}" is expanded too. Hosts must
// therefore pass an EnvResolver that only knows the manifest's declared
// secrets (the declarative runtime does).
package template

import (
	"math/big"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/pycompat"
)

// EnvResolver returns the value for ${env:NAME}, or "" when unknown.
type EnvResolver func(name string) string

// pyWS is Python's Unicode \s class for str patterns.
const pyWS = `[\t\n\v\f\r\x1c-\x1f \x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}]`

var (
	varRE  = regexp.MustCompile(`\{\{` + pyWS + `*([^}|]+?)` + pyWS + `*(?:\|` + pyWS + `*default:` + pyWS + `*([^}]*?)` + pyWS + `*)?\}\}`)
	envRE  = regexp.MustCompile(`\$\{env:([A-Za-z0-9_]+)\}`)
	stepRE = regexp.MustCompile(`^([A-Za-z0-9_]*)(?:\[(\p{Nd}*)\])?\n?$`)
)

// Lookup resolves a dotted path like "input.first_name" against ctx.
func Lookup(path string, ctx any) any {
	cur := ctx
	for _, part := range strings.Split(pycompat.Strip(path), ".") {
		m, ok := cur.(*pycompat.Map)
		if !ok {
			return nil
		}
		cur, _ = m.Get(part)
		if cur == nil {
			return nil
		}
	}
	return cur
}

// RenderString renders {{input.x|default:y}} and ${env:VAR} in one string.
func RenderString(s string, ctx any, env EnvResolver) string {
	s = replaceAllSubmatch(varRE, s, func(m []string, present []bool) string {
		val := Lookup(m[1], ctx)
		if val == nil || val == "" {
			def := ""
			if present[2] {
				def = m[2]
			}
			return strings.Trim(pycompat.Strip(def), `'"`)
		}
		return pycompat.Str(val)
	})
	return replaceAllSubmatch(envRE, s, func(m []string, _ []bool) string {
		if env == nil {
			return ""
		}
		return env(m[1])
	})
}

func replaceAllSubmatch(re *regexp.Regexp, s string, fn func(m []string, present []bool) string) string {
	idx := re.FindAllStringSubmatchIndex(s, -1)
	if idx == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, loc := range idx {
		b.WriteString(s[last:loc[0]])
		n := len(loc) / 2
		groups := make([]string, n)
		present := make([]bool, n)
		for g := 0; g < n; g++ {
			if loc[2*g] >= 0 {
				groups[g] = s[loc[2*g]:loc[2*g+1]]
				present[g] = true
			}
		}
		b.WriteString(fn(groups, present))
		last = loc[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

// RenderTemplate recursively renders every string leaf of a nested value.
func RenderTemplate(value any, ctx any, env EnvResolver) any {
	switch t := value.(type) {
	case string:
		return RenderString(t, ctx, env)
	case *pycompat.Map:
		out := pycompat.NewMap()
		for _, e := range t.Entries() {
			_ = out.Set(e.Key, RenderTemplate(e.Value, ctx, env))
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = RenderTemplate(e, ctx, env)
		}
		return out
	}
	return value
}

// WalkPath walks a JSONPath-lite path (no leading "$."), supporting "[]"
// (first element) and "[i]".
func WalkPath(data any, path string) any {
	cur := data
	for _, raw := range strings.Split(path, ".") {
		if cur == nil {
			return nil
		}
		m := stepRE.FindStringSubmatchIndex(raw)
		if m == nil {
			return nil
		}
		key := raw[m[2]:m[3]]
		if key != "" {
			if mp, ok := cur.(*pycompat.Map); ok {
				cur, _ = mp.Get(key)
			} else {
				cur = nil
			}
		}
		if m[4] >= 0 { // bracket present
			list, ok := cur.([]any)
			if !ok {
				return nil
			}
			idx := raw[m[4]:m[5]]
			if idx == "" {
				if len(list) > 0 {
					cur = list[0]
				} else {
					cur = nil
				}
				continue
			}
			i := unicodeInt(idx)
			if i.Sign() >= 0 && i.Cmp(big.NewInt(int64(len(list)))) < 0 {
				cur = list[i.Int64()]
			} else {
				cur = nil
			}
		}
	}
	return cur
}

// unicodeInt mirrors int() over a string of Unicode decimal digits.
func unicodeInt(s string) *big.Int {
	n := new(big.Int)
	ten := big.NewInt(10)
	for _, r := range s {
		n.Mul(n, ten)
		n.Add(n, big.NewInt(int64(digitValue(r))))
	}
	return n
}

// digitValue returns the decimal value of a Unicode Nd rune. Unicode encodes
// every decimal digit set as a contiguous 0..9 run.
func digitValue(r rune) int {
	if r >= '0' && r <= '9' {
		return int(r - '0')
	}
	start := r
	for start > 0 && unicode.IsDigit(start-1) {
		start--
	}
	return int(r-start) % 10
}

// ProjectValue resolves one mapping expression against the response JSON,
// honoring a literal prefix before "$." (e.g. "https://x.com/$.handle").
func ProjectValue(data any, expr string) any {
	idx := strings.Index(expr, "$.")
	if idx == -1 {
		return expr
	}
	prefix, path := expr[:idx], expr[idx+2:]
	var val any
	if path != "" {
		val = WalkPath(data, path)
	} else {
		val = data
	}
	if val == nil {
		return nil
	}
	if prefix != "" {
		return prefix + pycompat.Str(val)
	}
	return val
}

// Mapping is one ordered output_field -> expression pair.
type Mapping struct {
	Field string
	Expr  string
}

// ProjectResponse projects the response onto {field: value}, dropping
// None and "" results, in mapping order.
func ProjectResponse(data any, mappings []Mapping) *pycompat.Map {
	out := pycompat.NewMap()
	for _, m := range mappings {
		val := ProjectValue(data, m.Expr)
		if val != nil && val != "" {
			_ = out.Set(m.Field, val)
		}
	}
	return out
}

// Truncate mirrors Python's s[:n] on code points.
func Truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}
