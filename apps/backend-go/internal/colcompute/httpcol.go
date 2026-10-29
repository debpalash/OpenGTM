package colcompute

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
	"strings"
	"sync"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
)

// HTTPColumn is the config of an http action column (http_url, http_method, http_headers,
// http_body, http_extract).
type HTTPColumn struct {
	URL     string            // URL template; {column} references expand, nothing is percent-encoded
	Method  string            // default GET
	Headers map[string]string // header value templates; {secret:NAME} resolves through the secrets callback
	// Body is a string body template. JSONBody is a structured (object/array/string) body
	// template kept as raw JSON so key order survives; it wins over Body when both are set.
	Body     string
	JSONBody json.RawMessage
	JSONPath string // JSONPath-lite expression for the cell value (empty: raw text)
	Timeout  time.Duration
}

// HTTPResult is {success, value, error} of an http column cell.
type HTTPResult struct {
	Success bool
	Value   any
	Error   string
}

const (
	httpDefaultTimeout = 20 * time.Second
	httpMaxBody        = 1 << 20 // response body cap
	httpMaxRaw         = 2000    // characters kept of a raw text value
)

// egressCheck is the pre-flight SSRF verdict on the expanded URL (a variable so tests can
// reach loopback servers; the guarded client still vets the address at dial time).
var egressCheck = func(raw string) error { return egress.CheckURL(raw, true) }

var (
	defaultHTTPOnce   sync.Once
	defaultHTTPClient *http.Client
	// testTransport replaces the wire transport under the guarded client. Only _test files
	// assign it; production always dials through egress.
	testTransport http.RoundTripper
)

// allowedMethods: the verbs the http column sends (the vectors pin DELETE passthrough).
var allowedMethods = map[string]bool{"GET": true, "HEAD": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true}

// RunHTTPColumn is http_column.execute_http_column: expand the templates against row
// (column id or name -> value), vet the resulting URL, send one request without following
// redirects, and project the JSON response. The error is only for caller-fatal problems
// (cancelled context, ambiguous {column} reference); everything Python reports per cell is
// in the result.
func RunHTTPColumn(ctx context.Context, c *http.Client, col HTTPColumn, row map[string]any, secrets func(name string) (string, bool)) (HTTPResult, error) {
	fail := func(msg string) (HTTPResult, error) { return HTTPResult{Error: msg}, nil }
	urlTmpl := pyStripSpace(col.URL)
	if urlTmpl == "" {
		return fail("no_url")
	}
	target, err := ResolvePrompt(urlTmpl, row, false)
	if err != nil {
		return HTTPResult{}, err
	}
	if strings.Contains(target, "[") && strings.Contains(target, ": not found]") {
		return fail("unresolved_url_placeholder")
	}
	// SSRF guard on the expanded URL: a row value must never point us at a private host.
	if err := egressCheck(target); err != nil {
		var be *egress.BlockedError
		if errors.As(err, &be) {
			return fail("blocked_url: " + truncChars(be.Reason, 60))
		}
		return fail("blocked_url: " + truncChars(err.Error(), 60))
	}

	method := pyUpper(col.Method)
	if method == "" {
		method = "GET"
	}
	if !allowedMethods[method] {
		return fail("unsupported_method")
	}

	// headers (secrets are expanded only here, and never toward a row-chosen host)
	var used []string
	usedSecret := false
	headers := make(map[string]string, len(col.Headers))
	for k, v := range col.Headers {
		val, err := subPlaceholders(v, func(group string) (string, error) {
			key := pyStripSpace(group)
			if name, ok := strings.CutPrefix(key, "secret:"); ok && secrets != nil {
				s, found := secrets(name)
				if !found {
					return "", &secretErr{name}
				}
				usedSecret = true
				if s != "" {
					used = append(used, s)
				}
				return s, nil
			}
			return resolveGroup(group, row, false)
		})
		var se *secretErr
		if errors.As(err, &se) {
			return fail("unknown_secret: " + se.name)
		}
		if err != nil {
			return HTTPResult{}, err
		}
		headers[k] = val
	}
	if usedSecret && authorityTemplated(urlTmpl) {
		return fail("secret_with_templated_host")
	}

	var body []byte
	jsonBody := false
	if method == "POST" || method == "PUT" || method == "PATCH" {
		switch {
		case len(col.JSONBody) > 0:
			structured, perr := parseJSON(col.JSONBody)
			if perr != nil {
				return fail("invalid_body_template")
			}
			resolved, rerr := resolveAny(structured, row)
			if rerr != nil {
				return HTTPResult{}, rerr
			}
			switch x := resolved.(type) {
			case *pyDict, []any:
				s, derr := jsonDumps(x, true, false)
				if derr != nil {
					return fail(truncChars(redact(derr.Error(), used), 120))
				}
				body, jsonBody = []byte(s), true
			case string:
				body = []byte(x)
			}
		case col.Body != "":
			s, rerr := ResolvePrompt(col.Body, row, false)
			if rerr != nil {
				return HTTPResult{}, rerr
			}
			body = []byte(s)
		}
	}

	timeout := col.Timeout
	if timeout <= 0 {
		timeout = httpDefaultTimeout
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var reader io.Reader
	if method == "POST" || method == "PUT" || method == "PATCH" {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(rctx, method, quoteURL(target), reader)
	if err != nil {
		return fail(truncChars(redact(innerErr(err).Error(), used), 120))
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if jsonBody && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	// The caller's client is deliberately ignored: only the dial-time-guarded egress client
	// may carry a row-controlled URL (a pre-flight check alone is open to DNS rebinding).
	_ = c
	defaultHTTPOnce.Do(func() { defaultHTTPClient = egress.NewClient(nil) })
	cc := *defaultHTTPClient
	if testTransport != nil {
		cc.Transport = testTransport
	}
	cc.Timeout = 0 // the context carries the deadline
	// A redirect can turn an approved public URL into a private or metadata target: never
	// follow, surface the 3xx instead.
	cc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	resp, err := cc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return HTTPResult{}, ctx.Err()
		}
		var ne net.Error
		if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout() {
			return fail("timeout")
		}
		var be *egress.BlockedError
		if errors.As(err, &be) {
			return fail("blocked_url: " + truncChars(be.Reason, 60))
		}
		return fail(truncChars(redact(innerErr(err).Error(), used), 120))
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fail(fmt.Sprintf("http_%d", resp.StatusCode))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, httpMaxBody+1))
	if err != nil {
		if ctx.Err() != nil {
			return HTTPResult{}, ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return fail("timeout")
		}
		return fail(truncChars(redact(innerErr(err).Error(), used), 120))
	}
	if len(raw) > httpMaxBody {
		return fail("response_too_large")
	}

	extract := pyStripSpace(col.JSONPath)
	if extract == "" { // no extractor: the raw text, truncated
		text := truncChars(redact(decodeBody(raw, resp.Header.Get("Content-Type")), used), httpMaxRaw)
		if text == "" {
			return fail("empty")
		}
		return HTTPResult{Success: true, Value: text}, nil
	}
	data, perr := parseJSON(raw)
	if perr != nil {
		return fail("non_json_response")
	}
	value := projectValue(data, extract)
	if value == nil || pyEq(value, "") {
		return fail("no_match")
	}
	switch value.(type) {
	case *pyDict, []any: // keep cells scalar
		s, derr := jsonDumps(value, false, true)
		if derr != nil {
			return fail("no_match")
		}
		value = truncChars(s, httpMaxRaw)
	}
	if sv, ok := value.(string); ok {
		value = redact(sv, used)
	}
	return HTTPResult{Success: true, Value: value}, nil
}

type secretErr struct{ name string }

func (e *secretErr) Error() string { return "unknown secret " + e.name }

// authorityTemplated reports whether the URL template's scheme-and-authority part takes
// any value from the row (or has no recognisable authority at all).
func authorityTemplated(tmpl string) bool {
	_, rest, ok := strings.Cut(tmpl, "://")
	if !ok {
		return true
	}
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	return strings.Contains(rest, "{") || strings.Contains(tmpl[:strings.Index(tmpl, "://")], "{")
}

func redact(s string, secrets []string) string {
	for _, sec := range secrets {
		s = strings.ReplaceAll(s, sec, "[redacted]")
	}
	return s
}

// innerErr drops the *url.Error wrapper, which carries the full request URL.
func innerErr(err error) error {
	var ue *url.Error
	for errors.As(err, &ue) {
		err = ue.Err
		ue = nil
	}
	return err
}

// resolveAny is http_column._resolve_any: expand {column} in every string of a structure.
func resolveAny(v any, row map[string]any) (any, error) {
	switch x := v.(type) {
	case string:
		return ResolvePrompt(x, row, false)
	case *pyDict:
		out := &pyDict{keys: x.keys, vals: make(map[string]any, len(x.vals))}
		for _, k := range x.keys {
			r, err := resolveAny(x.vals[k], row)
			if err != nil {
				return nil, err
			}
			out.vals[k] = r
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			r, err := resolveAny(e, row)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	}
	return v, nil
}

// decodeBody is httpx's Response.text: the declared charset (utf-8 and latin-1 families
// supported) else UTF-8, undecodable bytes replaced.
func decodeBody(b []byte, contentType string) string {
	lower := strings.ToLower(contentType)
	if i := strings.Index(lower, "charset="); i >= 0 {
		cs := strings.Trim(strings.SplitN(lower[i+len("charset="):], ";", 2)[0], " \"'")
		switch cs {
		case "iso-8859-1", "latin-1", "latin1", "ascii", "us-ascii", "windows-1252", "cp1252":
			rs := make([]rune, len(b))
			for i, c := range b {
				rs[i] = rune(c)
			}
			return string(rs)
		}
	}
	return strings.ToValidUTF8(string(b), "�")
}

// ---- httpx-compatible URL quoting ----

const (
	unreservedChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
	pathSafe        = "!$%&'()*+,-./0123456789:;=@ABCDEFGHIJKLMNOPQRSTUVWXYZ[\\]^_abcdefghijklmnopqrstuvwxyz|~"
	querySafe       = "!$%&'()*+,-./0123456789:;=?@ABCDEFGHIJKLMNOPQRSTUVWXYZ[\\]^_`abcdefghijklmnopqrstuvwxyz{|}~"
)

func isHexByte(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F'
}

// quoteComponent percent-encodes bytes outside safe, keeping existing %XX escapes.
func quoteComponent(s, safe string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '%' && i+2 < len(s) && isHexByte(s[i+1]) && isHexByte(s[i+2]):
			b.WriteString(s[i : i+3])
			i += 2
		case strings.IndexByte(safe, c) >= 0 || strings.IndexByte(unreservedChars, c) >= 0:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// quoteURL renders the URL the way httpx's str(request.url) does (space -> %20, non-ASCII
// UTF-8 percent-encoded, dot segments removed) so the request line is always well formed.
func quoteURL(raw string) string {
	s := strings.TrimSpace(raw)
	frag := ""
	if i := strings.IndexByte(s, '#'); i >= 0 {
		s, frag = s[:i], s[i:]
	}
	query, hasQuery := "", false
	if i := strings.IndexByte(s, '?'); i >= 0 {
		s, query, hasQuery = s[:i], s[i+1:], true
	}
	prefix, path := "", s
	if i := strings.Index(s, "://"); i >= 0 {
		rest := s[i+3:]
		j := strings.IndexByte(rest, '/')
		if j < 0 {
			prefix, path = s, ""
		} else {
			prefix, path = s[:i+3+j], rest[j:]
		}
	}
	if prefix != "" {
		path = removeDotSegments(path)
	}
	out := prefix + quoteComponent(path, pathSafe)
	if hasQuery {
		out += "?" + quoteComponent(query, querySafe)
	}
	_ = frag // fragments are never sent
	return out
}

func removeDotSegments(path string) string {
	if path == "" {
		return path
	}
	var out []string
	segs := strings.Split(path, "/")
	for i, s := range segs {
		switch s {
		case ".":
			if i == len(segs)-1 {
				out = append(out, "")
			}
		case "..":
			if len(out) > 1 {
				out = out[:len(out)-1]
			}
			if i == len(segs)-1 {
				out = append(out, "")
			}
		default:
			out = append(out, s)
		}
	}
	return strings.Join(out, "/")
}
