package manifest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

const maxBodyBytes = 1 << 20

// Execute renders the manifest request for lead, calls the endpoint through
// client (which must be SSRF-guarded, e.g. egress.NewClient), and projects the
// response onto flat enrichment fields. cred resolves ${env:VAR} references and
// the auth key; an error or empty value counts as missing. Error strings match
// the Python DeclarativeProvider.enrich contract.
//
// cred contract: the implementation (Task 11's credentials client) must resolve
// settings-DB-first, then environment, like Python's _default_env_resolver, and
// swallow lookup errors itself where Python would. Execute additionally maps
// ANY error or empty value from cred to the Python "<name>: missing <ENV>"
// result (for auth.env_var) or "" (in templates), and makes no network call
// when the auth key is missing. A nil cred resolves everything to "".
func Execute(ctx context.Context, client *http.Client, m Manifest, lead map[string]string, cred func(envVar string) (string, error)) Result {
	return ExecuteChecked(ctx, client, m, lead, cred, nil)
}

// ExecuteChecked is Execute with an explicit URL guard (nil means
// egress.CheckURL(url, true)); the worker passes an allowlist-aware guard.
func ExecuteChecked(ctx context.Context, client *http.Client, m Manifest, lead map[string]string, cred func(envVar string) (string, error), check func(rawURL string) error) Result {
	t0 := time.Now()
	elapsed := func() float64 { return float64(time.Since(t0)) / float64(time.Millisecond) }
	p, perr := Prepare(m, lead, cred, check)
	if perr != "" {
		return Result{Error: perr}
	}
	target, headers, params, body, req := p.Target, p.Headers, p.Params, p.Body, p.Spec

	rctx := ctx
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		rctx, cancel = context.WithTimeout(ctx, time.Duration(req.Timeout*float64(time.Second)))
		defer cancel()
	}

	httpReq, err := buildRequest(rctx, req.Method, target, headers, params, body)
	if err != nil {
		return Result{Error: scrubbed(failure(err), p.Secrets)}
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return Result{Error: scrubbed(failure(err), p.Secrets)}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return Result{Error: "http_" + itoa(resp.StatusCode), HTTPStatus: resp.StatusCode, DurationMS: elapsed()}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return Result{Error: scrubbed(failure(err), p.Secrets), HTTPStatus: resp.StatusCode}
	}
	dur := elapsed()
	var data any
	if len(raw) > maxBodyBytes {
		return Result{Error: "non_json_response", HTTPStatus: resp.StatusCode, DurationMS: dur}
	}
	if data, err = canonjson.Decode(raw); err != nil {
		return Result{Error: "non_json_response", HTTPStatus: resp.StatusCode, DurationMS: dur}
	}

	rs := m.Response
	if rs.ErrorPath != "" && pyTruthy(ProjectValue(data, "$."+rs.ErrorPath)) {
		msg := "provider_error"
		if rs.ErrorMessagePath != "" {
			msg = pyStr(ProjectValue(data, "$."+rs.ErrorMessagePath))
		}
		return Result{Error: truncRunes(providers.Scrub(msg, p.Secrets...), 120), HTTPStatus: resp.StatusCode, DurationMS: dur}
	}

	fields := Normalize(Project(data, rs.Mappings))
	res := Result{
		Success: len(fields) > 0, Fields: fields, Confidence: m.DefaultConfidence,
		DurationMS: dur, HTTPStatus: resp.StatusCode,
	}
	if !res.Success {
		res.Error = "no_data"
	}
	return res
}

// Prepared is a fully rendered, vetted request: every step that can fail
// before any network I/O has already passed.
type Prepared struct {
	Target  string
	Headers map[string]string
	Params  map[string]string
	Body    any
	Spec    RequestSpec
	// Secrets are the non-empty credential values resolved for this request; error text that
	// leaves the package is scrubbed of them.
	Secrets []string
}

// SecurityProblem returns a stable refusal reason ("templated_authority" or
// "literal_auth_value") or "". Mirrors Python manifest_security_problem.
func SecurityProblem(m Manifest) string {
	a := m.Auth
	secret := (a.Type != "none" && a.Type != "") || len(allowedEnvNames(m)) > 0
	u := m.Request.URL
	if secret && strings.Contains(u[:authorityEnd(u)], "{{") {
		return "templated_authority"
	}
	if a.Type != "none" && a.Type != "" && a.Value != "" && !envRE.MatchString(a.Value) {
		return "literal_auth_value"
	}
	return ""
}

// authorityEnd is the index where the path/query/fragment of u begins.
func authorityEnd(u string) int {
	start := 0
	if i := strings.Index(u, "://"); i >= 0 {
		start = i + 3
	}
	if j := strings.IndexAny(u[start:], "/?#"); j >= 0 {
		return start + j
	}
	return len(u)
}

// Prepare renders the request and resolves the credential without any network
// I/O (cred may call the internal credentials API). It returns the Python-
// contract error string ("templated_authority", "<name>: missing ENV",
// "blocked_url: ...") when the call must not be made; callers that pay per
// call use this before reserving budget so only real network calls dispatch.
func Prepare(m Manifest, lead map[string]string, cred func(envVar string) (string, error), check func(rawURL string) error) (*Prepared, string) {
	if r := SecurityProblem(m); r != "" {
		return nil, r
	}
	allowed := allowedEnvNames(m)
	var secretVals []string
	env := func(v string) string {
		if cred == nil {
			return ""
		}
		if _, ok := allowed[v]; !ok {
			return ""
		}
		s, err := cred(v)
		if err != nil {
			return ""
		}
		if s != "" {
			secretVals = append(secretVals, s)
		}
		return s
	}
	tctx := map[string]any{"input": InputContext(lead)}
	req := m.Request
	target := RenderString(req.URL, tctx, env)
	headers := make(map[string]string, len(req.Headers)+1)
	for k, v := range req.Headers {
		headers[k] = RenderString(pyStr(v), tctx, env)
	}
	params := make(map[string]string, len(req.Query)+1)
	for k, v := range req.Query {
		params[k] = RenderString(pyStr(v), tctx, env)
	}
	var body any
	if req.BodyTemplate != nil {
		body = Render(req.BodyTemplate, tctx, env)
	}

	// Fail fast on a config error (missing key) before any network work.
	if a := m.Auth; a.Type != "none" && a.Type != "" {
		if a.EnvVar != "" && env(a.EnvVar) == "" {
			return nil, m.Name + ": missing " + a.EnvVar
		}
		var value string
		if a.Value != "" {
			value = RenderString(a.Value, map[string]any{}, env)
		} else {
			value = env(a.EnvVar)
		}
		switch a.Type {
		case "bearer":
			headers["Authorization"] = "Bearer " + value
		case "header":
			headers[orDefault(a.Param, "Authorization")] = value
		case "query":
			params[orDefault(a.Param, "api_key")] = value
		}
	}

	// SSRF guard on the templated URL; DNS vetting happens at dial time.
	if check == nil {
		check = func(raw string) error { return egress.CheckURL(raw, true) }
	}
	if err := check(target); err != nil {
		return nil, "blocked_url: " + truncRunes(blockedReason(err), 60)
	}
	return &Prepared{Target: target, Headers: headers, Params: params, Body: body, Spec: req, Secrets: secretVals}, ""
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func itoa(n int) string {
	b, _ := canonjson.Marshal(int64(n), false)
	return string(b)
}

func buildRequest(ctx context.Context, method, target string, headers, params map[string]string, body any) (*http.Request, error) {
	if len(params) > 0 {
		u, err := url.Parse(target)
		if err != nil {
			return nil, err
		}
		keys := make([]string, 0, len(params))
		for k := range params {
			keys = append(keys, k)
		}
		sort.Strings(keys) // manifest key order is not retained by Go maps
		var q strings.Builder
		q.WriteString(u.RawQuery)
		for _, k := range keys {
			if q.Len() > 0 {
				q.WriteByte('&')
			}
			q.WriteString(url.QueryEscape(k) + "=" + url.QueryEscape(params[k]))
		}
		u.RawQuery = q.String()
		target = u.String()
	}
	var rd io.Reader
	if body != nil {
		enc, err := canonjson.Marshal(body, false)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(enc)
	}
	r, err := http.NewRequestWithContext(ctx, strings.ToUpper(method), target, rd)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	if body != nil && r.Header.Get("Content-Type") == "" {
		r.Header.Set("Content-Type", "application/json")
	}
	return r, nil
}

// scrubbed removes credential values from error text (the vendor or transport may echo them).
func scrubbed(s string, secrets []string) string { return providers.Scrub(s, secrets...) }

// failure maps a transport error to the Python error string: "timeout" for
// deadline/net timeouts, else str(e)[:120]. The *url.Error wrapper is dropped
// because it echoes the request URL, which can carry a query-string API key.
func failure(err error) string {
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return "timeout"
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return truncRunes(err.Error(), 120)
}

// allowedEnvNames is the set of variables a manifest may read: its auth.env_var
// plus names literally written as ${env:NAME} in its own templates. Everything
// else resolves to "".
func allowedEnvNames(m Manifest) map[string]struct{} {
	names := map[string]struct{}{}
	if m.Auth.EnvVar != "" {
		names[m.Auth.EnvVar] = struct{}{}
	}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			for _, g := range envRE.FindAllStringSubmatch(x, -1) {
				names[g[1]] = struct{}{}
			}
		case map[string]any:
			for _, e := range x {
				walk(e)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(m.Request.URL)
	walk(m.Request.Headers)
	walk(m.Request.Query)
	walk(m.Request.BodyTemplate)
	walk(m.Auth.Value)
	return names
}

// blockedReason returns the egress guard's reason text, which is byte-identical
// to Python's BlockedUrlError message (no "egress: blocked: " log prefix).
func blockedReason(err error) string {
	var be *egress.BlockedError
	if errors.As(err, &be) {
		return be.Reason
	}
	return err.Error()
}
