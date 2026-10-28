// Package intkit is the shared plumbing of the Airtable, Sheets, Instantly and
// Smartlead clients (ports of services/integrations/*.py): credential lookup,
// bounded calls over the caller's SSRF-safe client, Python-compatible quoting
// and value rendering. Credentials never reach an error string or log line.
package intkit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// Obj is the insertion-ordered JSON object used for payloads and results.
type Obj = wbcrud.Obj

// Timeout is the per-request deadline of these four vendors (Python: 15 s via
// httpx; the plan pins 10 s).
const Timeout = 10 * time.Second

// Secrets resolves one credential: the workspace secret only, or workspace,
// global settings, then process environment with ALLOW_GLOBAL_CREDENTIAL_FALLBACK=true
// (secrets.Resolver.Credential; final security Low 5). An empty value is "unset".
type Secrets interface {
	Get(ctx context.Context, ws, key string) (string, error)
}

// ResolverSecrets adapts *secrets.Resolver.
type ResolverSecrets struct{ R *secrets.Resolver }

func (r ResolverSecrets) Get(ctx context.Context, ws, key string) (string, error) {
	return r.R.Credential(ctx, ws, key)
}

// Key looks up a credential. A lookup failure is an infrastructure error.
func Key(ctx context.Context, s Secrets, ws, name string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return s.Get(ctx, ws, name)
}

// Fail is {"success": false, "error": msg}.
func Fail(msg string) *Obj {
	o := NewObj()
	o.Set("success", false)
	o.Set("error", msg)
	return o
}

// NewObj returns an empty ordered object.
func NewObj() *Obj { return wbcrud.NewObj() }

// Send performs one call within Timeout, never following a redirect. The
// error, if any, is scrubbed of secret. A cancelled parent context is returned
// as ctx.Err() so callers can stop; every other failure is a vendor failure.
func Send(ctx context.Context, c *http.Client, r providers.Req, secret string) (providers.Response, error) {
	cctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	resp, err := providers.Do(cctx, c, r, secret)
	if err != nil && ctx.Err() != nil {
		return providers.Response{}, ctx.Err()
	}
	return resp, err
}

// ErrText renders a vendor failure as str(exc) would, scrubbed.
func ErrText(err error, secret string) string {
	return providers.Scrub(err.Error(), secret)
}

// HTTPError is f"HTTP {status}: {text[:160]}" with the credential scrubbed.
func HTTPError(status int, body []byte, secret string) string {
	return "HTTP " + strconv.Itoa(status) + ": " + providers.TruncRunes(Text(body, secret), 160)
}

// Text is resp.text (invalid UTF-8 replaced) with the credential scrubbed.
func Text(body []byte, secret string) string {
	return providers.Scrub(strings.ToValidUTF8(string(body), "�"), secret)
}

// Marshal is httpx's json= encoding: compact, insertion order, ensure_ascii off.
func Marshal(o *Obj) []byte {
	b, _ := o.MarshalJSON()
	return b
}

// JSONHeaders is the header set every call carries (Authorization added by the
// caller where the vendor wants it).
func JSONHeaders(bearer string) [][2]string {
	h := [][2]string{}
	if bearer != "" {
		h = append(h, [2]string{"Authorization", "Bearer " + bearer})
	}
	return append(h, [2]string{"Content-Type", "application/json"})
}

// PyQuote is urllib.parse.quote(s, safe=""): every byte outside
// [A-Za-z0-9_.~-] is percent-encoded in upper-case hex. (url.PathEscape leaves
// "$&+=:@" and sub-delims alone, which differs from Python.)
func PyQuote(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == '-' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0xf])
	}
	return b.String()
}

// Empty is `v in (None, "", [])`.
func Empty(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	case []string:
		return len(t) == 0
	}
	return false
}

// Clean is {k: v for k, v in fields.items() if v not in (None, "", [])}.
func Clean(fields *Obj) *Obj {
	out := NewObj()
	for _, k := range fields.Keys() {
		if v, _ := fields.Get(k); !Empty(v) {
			out.Set(k, v)
		}
	}
	return out
}

// ParseJSON is resp.json(). It sharpens pyhttp's message for the most common
// malformed object (`{bad`): "Expecting property name enclosed in double
// quotes", as CPython words it.
func ParseJSON(body []byte) (any, error) {
	v, err := providers.ParseJSON(body)
	if err == nil {
		return v, nil
	}
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) > 1 && trimmed[0] == '{' {
		rest := bytes.TrimLeft(trimmed[1:], " \t\r\n")
		if len(rest) > 0 && rest[0] != '"' && rest[0] != '}' {
			pos := len(body) - len(rest)
			return nil, &providers.PyException{Msg: positioned("Expecting property name enclosed in double quotes", body, pos)}
		}
	}
	return nil, err
}

func positioned(msg string, body []byte, bytePos int) string {
	prefix := body[:bytePos]
	char := utf8.RuneCount(prefix)
	line := bytes.Count(prefix, []byte("\n")) + 1
	col := char + 1
	if i := bytes.LastIndexByte(prefix, '\n'); i >= 0 {
		col = utf8.RuneCount(prefix[i+1:]) + 1
	}
	return fmt.Sprintf("%s: line %d column %d (char %d)", msg, line, col, char)
}

// Index0 is v[0] for a decoded JSON value, with Python's failure text.
func Index0(v any) (any, error) {
	switch t := v.(type) {
	case []any:
		if len(t) == 0 {
			return nil, &providers.PyException{Msg: "list index out of range"}
		}
		return t[0], nil
	case string:
		if t == "" {
			return nil, &providers.PyException{Msg: "string index out of range"}
		}
		r, _ := utf8.DecodeRuneInString(t)
		return string(r), nil
	case map[string]any:
		return nil, &providers.PyException{Msg: "0"}
	}
	return nil, &providers.PyException{Msg: fmt.Sprintf("'%s' object is not subscriptable", providers.TypeName(v))}
}

// Str is str(v) with Python's repr for lists and dicts.
func Str(v any) string {
	switch t := v.(type) {
	case nil, bool, string, int64, int, float64:
		return providers.Str(v)
	case json.Number:
		n, _ := providers.ParseJSON([]byte(t))
		return providers.Str(n)
	}
	return repr(v)
}

func repr(v any) string {
	switch t := v.(type) {
	case string:
		return reprStr(t)
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = repr(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case []string:
		a := make([]any, len(t))
		for i, e := range t {
			a[i] = e
		}
		return repr(a)
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = reprStr(k) + ": " + repr(t[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case *Obj:
		keys := t.Keys()
		parts := make([]string, len(keys))
		for i, k := range keys {
			x, _ := t.Get(k)
			parts[i] = reprStr(k) + ": " + repr(x)
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return Str(v)
}

func reprStr(s string) string {
	q := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		q = '"'
	}
	var b strings.Builder
	b.WriteByte(q)
	for _, r := range s {
		switch {
		case r == rune(q) || r == '\\':
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
