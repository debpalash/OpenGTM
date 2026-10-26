// Package contact ports the contact and email-verification HTTP providers:
// hunter, apollo, prospeo, snov.io, people data labs, debounce, abstractapi,
// numverify and reacher. Each file is a port of the Python module of the same
// name under legacy/python/.../enrichment/providers/.
package contact

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

type fields = providers.Obj

func newFields() *fields { return providers.NewObj() }

// kv is one ordered JSON member; jo is an ordered JSON object (httpx sends
// json= bodies compact, ensure_ascii=False, in insertion order).
type kv struct {
	k string
	v any
}
type jo []kv

func marshal(v any) []byte {
	var b bytes.Buffer
	writeJSON(&b, v)
	return b.Bytes()
}

func writeJSON(b *bytes.Buffer, v any) {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case string:
		writeStr(b, t)
	case bool:
		b.WriteString(strconv.FormatBool(t))
	case int:
		b.WriteString(strconv.Itoa(t))
	case int64:
		b.WriteString(strconv.FormatInt(t, 10))
	case float64:
		s, _ := canonjson.FloatRepr(t)
		b.WriteString(s)
	case []string:
		b.WriteByte('[')
		for i, s := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			writeStr(b, s)
		}
		b.WriteByte(']')
	case []any:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJSON(b, e)
		}
		b.WriteByte(']')
	case jo:
		b.WriteByte('{')
		for i, p := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			writeStr(b, p.k)
			b.WriteByte(':')
			writeJSON(b, p.v)
		}
		b.WriteByte('}')
	default:
		panic(fmt.Sprintf("contact: cannot marshal %T", v))
	}
}

// writeStr is json.dumps(s, ensure_ascii=False).
func writeStr(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else if r == utf8.RuneError {
				b.WriteString("�")
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// jsonReq builds a JSON POST like httpx's post(..., json=body).
func jsonReq(url string, body jo, header ...[2]string) providers.Req {
	h := append([][2]string{{"Content-Type", "application/json"}}, header...)
	return providers.Req{Method: http.MethodPost, URL: url, Header: h, Body: marshal(body)}
}

// call sends r and parses the JSON body (resp.json()), ignoring the status
// unless the caller inspects it.
func call(ctx context.Context, c *http.Client, r providers.Req, secrets ...string) (status int, data any, err error) {
	resp, err := providers.Do(ctx, c, r, secrets...)
	if err != nil {
		return 0, nil, err
	}
	d, perr := providers.ParseJSON(resp.Body)
	return resp.Status, d, perr
}

// callOK is call but a body is only parsed when the status is 200 (providers
// that test `resp.status_code == 200` before touching resp.json()).
func callOK(ctx context.Context, c *http.Client, r providers.Req, secrets ...string) (ok bool, data any, err error) {
	resp, err := providers.Do(ctx, c, r, secrets...)
	if err != nil {
		return false, nil, err
	}
	if resp.Status != 200 {
		return false, nil, nil
	}
	d, perr := providers.ParseJSON(resp.Body)
	return true, d, perr
}

func fail(msg string) providers.Output { return providers.Output{Error: msg} }

func success(in providers.Input, f *fields, conf float64) providers.Output {
	var val any
	if in.Field != "" {
		val, _ = f.Get(in.Field)
	}
	return providers.Output{Value: val, Confidence: conf, Extra: f}
}

// finish converts a Python-style exception into the provider result
// (`except Exception as e: error=str(e)[:200]`), except that a cancelled
// context is surfaced as an error so the caller never records a vendor failure.
func finish(ctx context.Context, out providers.Output, err error) (providers.Output, error) {
	if err == nil {
		return out, nil
	}
	if cerr := ctx.Err(); cerr != nil {
		return providers.Output{}, cerr
	}
	return fail(providers.Trunc200(err.Error())), nil
}

func lead(in providers.Input, k string) string { return providers.LeadStr(in.Lead, k) }

// keyOrMissing resolves the credential lazily. A lookup failure is returned as
// an error; an empty key yields ok=false.
func resolveKey(key func() (string, error)) (k string, ok bool, err error) {
	k, err = key()
	if err != nil {
		return "", false, err
	}
	return k, k != "", nil
}

func hasSub(s, sub string) bool { return strings.Contains(s, sub) }

// div100 is `v / 100` for a decoded JSON number.
func div100(v any) (float64, error) {
	f, ok := providers.Num(v)
	if !ok {
		return 0, &providers.PyException{Msg: fmt.Sprintf("unsupported operand type(s) for /: '%s' and 'int'", providers.TypeName(v))}
	}
	return f / 100, nil
}

// slice500 is `v[:500]`.
func slice500(v any) (any, error) {
	if s, ok := v.(string); ok {
		return providers.TruncRunes(s, 500), nil
	}
	if l, ok := v.([]any); ok {
		if len(l) > 500 {
			return l[:500], nil
		}
		return l, nil
	}
	return nil, &providers.PyException{Msg: fmt.Sprintf("'%s' object is not subscriptable", providers.TypeName(v))}
}

// getd chains v.get(k1, d1).get(k2, d2)... returning the Python AttributeError
// text when an intermediate value is not a dict.
func getd(v any, path ...any) (any, error) {
	for i := 0; i+1 < len(path); i += 2 {
		var err error
		v, err = providers.PyGet(v, path[i].(string), path[i+1])
		if err != nil {
			return nil, err
		}
	}
	return v, nil
}
