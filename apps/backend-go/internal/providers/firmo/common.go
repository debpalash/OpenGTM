// Package firmo ports the firmographic and registry HTTP providers: ipinfo,
// gleif, companies house, mca, wikidata, sec edgar, jsonld, tech stack, ats
// hiring and google maps. Each file is a port of the Python module of the
// same name under legacy/python/.../enrichment/providers/. Registration is in
// register.go; consumers blank-import this package.
package firmo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

type fields = providers.Obj

func newFields() *fields { return providers.NewObj() }

func fail(msg string) providers.Output { return providers.Output{Error: msg} }

// failNamed is fail; the provider name is not part of Output.
func success(in providers.Input, f *fields, conf float64) providers.Output {
	var val any
	if in.Field != "" {
		val, _ = f.Get(in.Field)
	}
	return providers.Output{Value: val, Confidence: conf, Extra: f}
}

func lead(in providers.Input, k string) string { return providers.LeadStr(in.Lead, k) }

// finish turns a Python-style exception into the provider result
// (`except Exception as e: error=str(e)[:n]`, n=0 meaning untruncated); a
// cancelled context is surfaced as an error instead of a vendor failure.
func finish(ctx context.Context, out providers.Output, err error, n int) (providers.Output, error) {
	if err == nil {
		return out, nil
	}
	if cerr := ctx.Err(); cerr != nil {
		return providers.Output{}, cerr
	}
	msg := err.Error()
	if n > 0 {
		msg = providers.TruncRunes(msg, n)
	}
	return fail(msg), nil
}

// isTransport reports a transport-level failure (httpx.TransportError or
// TimeoutException) as opposed to a JSON decode or attribute error.
func isTransport(err error) bool {
	var f *providers.Failure
	return errors.As(err, &f)
}

// fetchJSON sends r and parses the body as JSON regardless of status
// (httpx's resp.json()).
func fetchJSON(ctx context.Context, c *http.Client, r providers.Req, secrets ...string) (status int, data any, err error) {
	resp, err := providers.Do(ctx, c, r, secrets...)
	if err != nil {
		return 0, nil, err
	}
	d, perr := providers.ParseJSON(resp.Body)
	return resp.Status, d, perr
}

// ── Python value helpers ──────────────────────────────────────────────────

func attrErr(v any) error {
	return &providers.PyException{Msg: fmt.Sprintf("'%s' object has no attribute 'get'", providers.TypeName(v))}
}

// get is v.get(k) (None default) with Python's AttributeError for non-dicts.
func get(v any, k string) (any, error) { return providers.PyGet(v, k, nil) }

// orEmpty is `v or {}`.
func orEmpty(v any) any {
	if providers.Truthy(v) {
		return v
	}
	return map[string]any{}
}

// chain is `((v.get(k1) or {}).get(k2) or {}) ... .get(kn)`: every step but the
// first is applied to `x or {}`. The first step is applied to v itself.
func chain(v any, keys ...string) (any, error) {
	var err error
	for i, k := range keys {
		if i > 0 {
			v = orEmpty(v)
		}
		v, err = get(v, k)
		if err != nil {
			return nil, err
		}
	}
	return v, nil
}

// dict returns v as an object or the AttributeError Python raises on .get.
func dict(v any) (map[string]any, error) {
	if m, ok := v.(map[string]any); ok {
		return m, nil
	}
	return nil, attrErr(v)
}

// elems is the sequence Python iterates for v: list items, or the characters
// of a string / keys of a dict (never dicts, so every .get on them fails).
func elems(v any) ([]any, error) {
	switch t := v.(type) {
	case []any:
		return t, nil
	case map[string]any:
		out := make([]any, 0, len(t))
		for k := range t {
			out = append(out, k)
		}
		return out, nil
	case string:
		out := make([]any, 0, len(t))
		for _, r := range t {
			out = append(out, string(r))
		}
		return out, nil
	}
	return nil, &providers.PyException{Msg: fmt.Sprintf("'%s' object is not iterable", providers.TypeName(v))}
}

// pyStrSplitJoin is " ".join(s.split()).
func pyStrSplitJoin(s string) string { return strings.Join(providers.PyFields(s), " ") }

// ── json.dumps (default: ensure_ascii, ", " and ": " separators) ──────────

type kv struct {
	k string
	v any
}

// jo is an insertion-ordered JSON object.
type jo []kv

func (o *jo) set(k string, v any) {
	for i := range *o {
		if (*o)[i].k == k {
			(*o)[i].v = v
			return
		}
	}
	*o = append(*o, kv{k, v})
}

func (o jo) get(k string) (any, bool) {
	for _, p := range o {
		if p.k == k {
			return p.v, true
		}
	}
	return nil, false
}

func dumps(v any) string { return dumpsSep(v, ", ", ": ") }

// dumpsCompact is json.dumps(v, separators=(",", ":")).
func dumpsCompact(v any) string { return dumpsSep(v, ",", ":") }

func dumpsSep(v any, is, ks string) string {
	var b bytes.Buffer
	dumpValue(&b, v, is, ks)
	return b.String()
}

func dumpValue(b *bytes.Buffer, v any, is, ks string) {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(t))
	case int:
		b.WriteString(strconv.Itoa(t))
	case int64:
		b.WriteString(strconv.FormatInt(t, 10))
	case float64:
		s, err := canonjson.FloatRepr(t)
		if err != nil { // json.dumps would write NaN / Infinity
			switch {
			case t != t:
				s = "NaN"
			case t > 0:
				s = "Infinity"
			default:
				s = "-Infinity"
			}
		}
		b.WriteString(s)
	case string:
		dumpString(b, t)
	case []string:
		b.WriteByte('[')
		for i, s := range t {
			if i > 0 {
				b.WriteString(is)
			}
			dumpString(b, s)
		}
		b.WriteByte(']')
	case []any:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteString(is)
			}
			dumpValue(b, e, is, ks)
		}
		b.WriteByte(']')
	case []jo:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteString(is)
			}
			dumpValue(b, e, is, ks)
		}
		b.WriteByte(']')
	case jo:
		b.WriteByte('{')
		for i, p := range t {
			if i > 0 {
				b.WriteString(is)
			}
			dumpString(b, p.k)
			b.WriteString(ks)
			dumpValue(b, p.v, is, ks)
		}
		b.WriteByte('}')
	case map[string]any:
		// Decoded vendor objects carry no order; sorted keys keep it stable.
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteString(is)
			}
			dumpString(b, k)
			b.WriteString(ks)
			dumpValue(b, t[k], is, ks)
		}
		b.WriteByte('}')
	default:
		panic(fmt.Sprintf("firmo: cannot dump %T", v))
	}
}

func dumpString(b *bytes.Buffer, s string) {
	const hexd = "0123456789abcdef"
	u := func(x uint32) {
		b.WriteString(`\u`)
		b.WriteByte(hexd[x>>12&0xf])
		b.WriteByte(hexd[x>>8&0xf])
		b.WriteByte(hexd[x>>4&0xf])
		b.WriteByte(hexd[x&0xf])
	}
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r >= ' ' && r <= '~':
			b.WriteRune(r)
		case r == utf8.RuneError:
			u(0xfffd)
		case r < 0x10000:
			u(uint32(r))
		default:
			r -= 0x10000
			u(0xd800 + uint32(r>>10))
			u(0xdc00 + uint32(r&0x3ff))
		}
	}
	b.WriteByte('"')
}

// pyStr is Python's str(v) for a decoded JSON value, including the repr of
// lists and dicts (dict keys come out sorted: decoded objects carry no order).
func pyStr(v any) string {
	switch t := v.(type) {
	case []any, map[string]any:
		return pyRepr(v)
	case float64:
		switch {
		case t != t:
			return "nan"
		case t > 1.797693134862315708145274237317043567981e308:
			return "inf"
		case t < -1.797693134862315708145274237317043567981e308:
			return "-inf"
		}
	}
	return providers.Str(v)
}

func pyRepr(v any) string {
	switch t := v.(type) {
	case string:
		return pyStrRepr(t)
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = pyRepr(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = pyStrRepr(k) + ": " + pyRepr(t[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return pyStr(v)
}

func pyStrRepr(s string) string {
	q := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		q = '"'
	}
	var b strings.Builder
	b.WriteByte(q)
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == rune(q):
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(q)
	return b.String()
}
