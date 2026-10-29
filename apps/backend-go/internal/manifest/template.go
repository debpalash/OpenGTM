package manifest

import (
	"regexp"
	"strings"
)

var (
	varRE = regexp.MustCompile(`\{\{` + pyWS + `*([^}|]+?)` + pyWS + `*(?:\|` + pyWS + `*default:` + pyWS + `*([^}]*?)` + pyWS + `*)?\}\}`)
	envRE = regexp.MustCompile(`\$\{env:([A-Za-z0-9_]+)\}`)
)

// lookup resolves a dotted path like "input.first_name" against ctx. Python
// falls back to getattr() on non-dict values; that yields bound methods and
// is deliberately not reproduced (treated as missing).
func lookup(path string, ctx map[string]any) any {
	var cur any = ctx
	for _, part := range strings.Split(pyStrip(path), ".") {
		switch m := cur.(type) {
		case map[string]any:
			cur = m[part]
		case map[string]string:
			if s, ok := m[part]; ok {
				cur = s
			} else {
				cur = nil
			}
		default:
			return nil
		}
		if cur == nil {
			return nil
		}
	}
	return cur
}

func expandEnv(text string, env func(string) string) string {
	return envRE.ReplaceAllStringFunc(text, func(m string) string {
		if env == nil {
			return ""
		}
		return env(envRE.FindStringSubmatch(m)[1])
	})
}

// RenderString renders {{input.x | default: y}} and ${env:VAR} in one string.
//
// SECURITY: ${env:VAR} is expanded only in manifest-authored literal text.
// Substituted values (lead data) and defaults are written verbatim, so a lead
// field containing "${env:SECRET}" can never exfiltrate a secret.
func RenderString(s string, ctx map[string]any, env func(string) string) string {
	var b strings.Builder
	last := 0
	for _, m := range varRE.FindAllStringSubmatchIndex(s, -1) {
		b.WriteString(expandEnv(s[last:m[0]], env))
		last = m[1]
		val := lookup(s[m[2]:m[3]], ctx)
		if str, ok := val.(string); val == nil || (ok && str == "") {
			def := ""
			if m[4] >= 0 {
				def = s[m[4]:m[5]]
			}
			b.WriteString(strings.Trim(pyStrip(def), `'"`))
		} else {
			b.WriteString(pyStr(val))
		}
	}
	b.WriteString(expandEnv(s[last:], env))
	return b.String()
}

// Render recursively renders every string leaf of a nested map/list value;
// non-string leaves pass through. Map keys are not rendered.
func Render(v any, ctx map[string]any, env func(string) string) any {
	switch x := v.(type) {
	case string:
		return RenderString(x, ctx, env)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = Render(e, ctx, env)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = Render(e, ctx, env)
		}
		return out
	}
	return v
}
