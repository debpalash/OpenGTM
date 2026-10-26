package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
)

// MaxBody caps every vendor response body (decision D2-D5 hardening; Python
// reads the whole body).
const MaxBody = 1 << 20

// CallTimeout is the per-request deadline (the Python providers use 15s).
const CallTimeout = 15 * time.Second

// Response is a fully read vendor response.
type Response struct {
	Status int
	Body   []byte
}

// Req is one outbound request. Query keeps insertion order (httpx does not
// sort). Header names are sent as given.
type Req struct {
	Method string
	URL    string
	Query  [][2]string
	Header [][2]string
	Body   []byte
}

// Do sends r through c without following redirects and reads at most MaxBody
// bytes. On a transport failure the returned error is already scrubbed of the
// secrets (the request URL carries query-string keys); it still satisfies
// IsTimeout.
func Do(ctx context.Context, c *http.Client, r Req, secrets ...string) (Response, error) {
	u := r.URL
	if len(r.Query) > 0 {
		var q strings.Builder
		for i, kv := range r.Query {
			if i > 0 {
				q.WriteByte('&')
			}
			q.WriteString(url.QueryEscape(kv[0]))
			q.WriteByte('=')
			q.WriteString(url.QueryEscape(kv[1]))
		}
		u += "?" + q.String()
	}
	ctx2, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	var body io.Reader
	if r.Body != nil {
		body = bytes.NewReader(r.Body)
	}
	req, err := http.NewRequestWithContext(ctx2, r.Method, u, body)
	if err != nil {
		return Response{}, &Failure{msg: Scrub(unwrapURL(err).Error(), secrets...)}
	}
	for _, kv := range r.Header {
		req.Header.Set(kv[0], kv[1])
	}
	cc := *c
	cc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := cc.Do(req)
	if err != nil {
		return Response{}, &Failure{msg: Scrub(unwrapURL(err).Error(), secrets...), timeout: isTimeout(err)}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody+1))
	if err != nil {
		return Response{}, &Failure{msg: Scrub(unwrapURL(err).Error(), secrets...), timeout: isTimeout(err)}
	}
	if len(b) > MaxBody {
		return Response{}, &Failure{msg: fmt.Sprintf("response body exceeds %d bytes", MaxBody)}
	}
	return Response{Status: resp.StatusCode, Body: b}, nil
}

// Failure is a scrubbed transport-level failure.
type Failure struct {
	msg     string
	timeout bool
}

func (f *Failure) Error() string { return f.msg }

// IsTimeout reports whether err is a request timeout (httpx.TimeoutException).
func IsTimeout(err error) bool {
	var f *Failure
	if errors.As(err, &f) {
		return f.timeout
	}
	return isTimeout(err)
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func unwrapURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// Scrub removes every secret value from s.
func Scrub(s string, secrets ...string) string {
	for _, sec := range secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "***")
		}
	}
	return s
}

// ── Python value semantics ────────────────────────────────────────────────

// PyException is an error whose text is what Python's str(exc) would be.
type PyException struct{ Msg string }

func (e *PyException) Error() string { return e.Msg }

// Trunc200 is str(e)[:200] (by characters).
func Trunc200(s string) string {
	if utf8.RuneCountInString(s) <= 200 {
		return s
	}
	return string([]rune(s)[:200])
}

// TruncRunes is s[:n] by characters.
func TruncRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// ParseJSON is resp.json(): numbers become int64 or float64, objects
// map[string]any. A syntax error carries Python's JSONDecodeError text.
func ParseJSON(body []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, &PyException{Msg: jsonErrText(body, err)}
	}
	if rest := bytes.TrimLeft(body[dec.InputOffset():], " \t\r\n"); len(rest) > 0 {
		pos := len(body) - len(rest)
		return nil, &PyException{Msg: jsonPos("Extra data", body, pos)}
	}
	return normNumbers(v), nil
}

func normNumbers(v any) any {
	switch t := v.(type) {
	case json.Number:
		s := string(t)
		if !strings.ContainsAny(s, ".eE") {
			if i, err := strconv.ParseInt(s, 10, 64); err == nil {
				return i
			}
		}
		f, _ := strconv.ParseFloat(s, 64)
		return f
	case []any:
		for i := range t {
			t[i] = normNumbers(t[i])
		}
	case map[string]any:
		for k := range t {
			t[k] = normNumbers(t[k])
		}
	}
	return v
}

// jsonErrText approximates json.JSONDecodeError's message. It is exact for an
// empty body, a body whose first token is not JSON (an HTML error page) and
// trailing data; other syntax errors report "Expecting value" at the position
// Go gave, which can differ from CPython's finer-grained messages.
func jsonErrText(body []byte, err error) string {
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	lead := len(body) - len(trimmed)
	if len(trimmed) == 0 || !bytes.ContainsRune([]byte(`{["-0123456789tfn`), rune(trimmed[0])) {
		return jsonPos("Expecting value", body, lead)
	}
	var se *json.SyntaxError
	if errors.As(err, &se) {
		off := int(se.Offset) - 1
		if off < lead {
			off = lead
		}
		if off > len(body) {
			off = len(body)
		}
		return jsonPos("Expecting value", body, off)
	}
	return jsonPos("Expecting value", body, lead)
}

func jsonPos(msg string, body []byte, bytePos int) string {
	if bytePos > len(body) {
		bytePos = len(body)
	}
	prefix := body[:bytePos]
	char := utf8.RuneCount(prefix)
	line := bytes.Count(prefix, []byte("\n")) + 1
	col := char + 1
	if i := bytes.LastIndexByte(prefix, '\n'); i >= 0 {
		col = utf8.RuneCount(prefix[i+1:]) + 1
	}
	return fmt.Sprintf("%s: line %d column %d (char %d)", msg, line, col, char)
}

// TypeName is Python's type(v).__name__ for a decoded JSON value.
func TypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case int64, int:
		return "int"
	case float64:
		return "float"
	case string:
		return "str"
	case []any:
		return "list"
	case map[string]any:
		return "dict"
	}
	return "object"
}

// PyGet is v.get(key, def): v must be an object, otherwise Python raises
// AttributeError.
func PyGet(v any, key string, def any) (any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, &PyException{Msg: fmt.Sprintf("'%s' object has no attribute 'get'", TypeName(v))}
	}
	if x, found := m[key]; found {
		return x, nil
	}
	return def, nil
}

// Truthy is Python's bool(v) for a decoded JSON value.
func Truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case int64:
		return t != 0
	case int:
		return t != 0
	case float64:
		return t != 0
	case string:
		return t != ""
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}

// Str is Python's str(v) for a decoded JSON value.
func Str(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case bool:
		if t {
			return "True"
		}
		return "False"
	case string:
		return t
	case int64:
		return strconv.FormatInt(t, 10)
	case int:
		return strconv.Itoa(t)
	case float64:
		s, err := canonjson.FloatRepr(t)
		if err != nil {
			return strconv.FormatFloat(t, 'g', -1, 64)
		}
		return s
	}
	return fmt.Sprint(v)
}

// Num converts a decoded JSON number to float64 (for arithmetic).
func Num(v any) (float64, bool) {
	switch t := v.(type) {
	case int64:
		return float64(t), true
	case int:
		return float64(t), true
	case float64:
		return t, true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// LeadStr is `lead.<key> or ""`: a non-string value is stringified like Python.
func LeadStr(lead map[string]any, key string) string {
	v := lead[key]
	if !Truthy(v) {
		return ""
	}
	return Str(v)
}

// ExtractDomain is the Python providers' _extract_domain.
func ExtractDomain(s string) string {
	if s == "" {
		return ""
	}
	s = PyStrip(s)
	for _, p := range []string{"https://", "http://", "www."} {
		s = strings.TrimPrefix(s, p)
	}
	s, _, _ = strings.Cut(s, "/")
	s, _, _ = strings.Cut(s, "?")
	return strings.ToLower(s)
}

func isPySpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

// PyStrip is str.strip().
func PyStrip(s string) string { return strings.TrimFunc(s, isPySpace) }

// PyFields is str.split().
func PyFields(s string) []string { return strings.FieldsFunc(s, isPySpace) }

// PySplit1 is str.split(None, 1) on an already stripped string.
func PySplit1(s string) []string {
	s = PyStrip(s)
	if s == "" {
		return nil
	}
	i := strings.IndexFunc(s, isPySpace)
	if i < 0 {
		return []string{s}
	}
	rest := strings.TrimLeftFunc(s[i:], isPySpace)
	return []string{s[:i], rest}
}
