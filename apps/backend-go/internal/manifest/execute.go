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

	rctx := ctx
	if p.Spec.Timeout > 0 {
		var cancel context.CancelFunc
		rctx, cancel = context.WithTimeout(ctx, time.Duration(p.Spec.Timeout*float64(time.Second)))
		defer cancel()
	}
	data, status, errStr := p.do(rctx, client, p)
	reqs := 1
	if errStr != "" {
		return Result{Error: errStr, HTTPStatus: status, DurationMS: elapsed(), Requests: reqs}
	}

	rs := m.Response
	if m.Poll != nil {
		// A vendor error on the submit response ends the lookup before any poll request.
		if msg, bad := responseError(rs, data, p.Secrets); bad {
			return Result{Error: msg, HTTPStatus: status, DurationMS: elapsed(), Requests: reqs}
		}
		var perr string
		if data, status, perr = runPoll(ctx, client, m, p, data, &reqs); perr != "" {
			if perr == "poll_failed" {
				if msg, bad := responseError(rs, data, p.Secrets); bad && msg != "provider_error" {
					perr = msg
				}
			}
			return Result{Error: scrubbed(perr, p.Secrets), HTTPStatus: status, DurationMS: elapsed(), Requests: reqs}
		}
	}
	dur := elapsed()
	if msg, bad := responseError(rs, data, p.Secrets); bad {
		return Result{Error: msg, HTTPStatus: status, DurationMS: dur, Requests: reqs}
	}
	if sw := rs.SuccessWhen; sw != nil && !sw.Match(data, status) {
		// A 200 that says "not a success": the vendor's message when it gave one, else a miss.
		msg := "no_data"
		if rs.ErrorMessagePath != "" {
			if t, ok := condText(ProjectValue(data, "$."+rs.ErrorMessagePath)); ok {
				msg = truncRunes(providers.Scrub(t, p.Secrets...), 120)
			}
		}
		return Result{Error: msg, HTTPStatus: status, DurationMS: dur, Requests: reqs}
	}

	fields := Normalize(ApplyValueMaps(Project(data, rs.Mappings), rs.ValueMaps))
	res := Result{
		Success: len(fields) > 0, Fields: fields, Confidence: m.DefaultConfidence,
		DurationMS: dur, HTTPStatus: status, Requests: reqs,
	}
	if !res.Success {
		res.Error = "no_data"
	}
	return res
}

// responseError is the vendor-reported error of a decoded response (response.error_path), with
// credential values scrubbed and the text capped.
func responseError(rs ResponseSpec, data any, secrets []string) (string, bool) {
	if rs.ErrorPath == "" || !pyTruthy(ProjectValue(data, "$."+rs.ErrorPath)) {
		return "", false
	}
	msg := "provider_error"
	if rs.ErrorMessagePath != "" {
		msg = pyStr(ProjectValue(data, "$."+rs.ErrorMessagePath))
	}
	return truncRunes(providers.Scrub(msg, secrets...), 120), true
}

// do sends one prepared request and decodes its JSON response. errStr is the Python-contract
// error text ("http_NNN", "non_json_response", a scrubbed transport failure) or "".
func (p *Prepared) do(ctx context.Context, client *http.Client, pr *Prepared) (data any, status int, errStr string) {
	httpReq, err := buildRequest(ctx, pr.Spec.Method, pr.Target, pr.Headers, pr.Params, pr.Body)
	if err != nil {
		return nil, 0, scrubbed(failure(err), p.Secrets)
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, 0, scrubbed(failure(err), p.Secrets)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, "http_" + itoa(resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return nil, resp.StatusCode, scrubbed(failure(err), p.Secrets)
	}
	if len(raw) > maxBodyBytes {
		return nil, resp.StatusCode, "non_json_response"
	}
	if data, err = canonjson.Decode(raw); err != nil {
		return nil, resp.StatusCode, "non_json_response"
	}
	return data, resp.StatusCode, ""
}

// Prepared is a fully rendered, vetted request: every step that can fail
// before any network I/O has already passed.
type Prepared struct {
	Target  string
	Headers map[string]string
	Params  map[string]string
	Body    any
	Spec    RequestSpec
	// Secrets are the non-empty credential values resolved for this request (and any poll request
	// rendered from it); error text that leaves the package is scrubbed of them.
	Secrets []string

	m     Manifest
	env   func(string) string
	tctx  map[string]any
	check func(rawURL string) error
}

// SecurityProblem returns a stable refusal reason ("templated_authority" or
// "literal_auth_value") or "". Mirrors Python manifest_security_problem.
func SecurityProblem(m Manifest) string {
	a := m.Auth
	secret := (a.Type != "none" && a.Type != "") || len(allowedEnvNames(m)) > 0 || len(templateEnvRefs(m)) > 0
	u := m.Request.URL
	if secret && strings.Contains(u[:authorityEnd(u)], "{{") {
		return "templated_authority"
	}
	if a.Type != "none" && a.Type != "" && a.Value != "" && !envRE.MatchString(a.Value) {
		return "literal_auth_value"
	}
	return extProblem(m)
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
	p := &Prepared{m: m}
	p.env = func(v string) string {
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
			p.Secrets = append(p.Secrets, s)
		}
		return s
	}
	p.tctx = map[string]any{"input": InputContext(lead)}
	// SSRF guard on the templated URL; DNS vetting happens at dial time.
	if check == nil {
		check = func(raw string) error { return egress.CheckURL(raw, true) }
	}
	p.check = check
	r, perr := p.render(m.Request, nil, nil)
	if perr != "" {
		return nil, perr
	}
	p.Target, p.Headers, p.Params, p.Body, p.Spec = r.Target, r.Headers, r.Params, r.Body, r.Spec
	return p, ""
}

// render builds one request of the manifest (the main request, or the poll request when extra
// carries the first response). hosts, when non-nil, is the set of hosts the rendered URL may
// have. Both credentials are applied here and only here: into a header or query parameter,
// never into the URL path.
func (p *Prepared) render(req RequestSpec, extra map[string]any, hosts map[string]bool) (*Prepared, string) {
	m, env := p.m, p.env
	tctx := p.tctx
	if extra != nil {
		tctx = map[string]any{}
		for k, v := range p.tctx {
			tctx[k] = v
		}
		for k, v := range extra {
			tctx[k] = v
		}
		if missingReference(req, tctx) {
			return nil, "poll_missing_reference"
		}
	}
	target := renderURL(req.URL, tctx, env)
	headers := make(map[string]string, len(req.Headers)+2)
	for k, v := range req.Headers {
		headers[k] = RenderString(pyStr(v), tctx, env)
	}
	params := make(map[string]string, len(req.Query)+2)
	for k, v := range req.Query {
		params[k] = RenderString(pyStr(v), tctx, env)
	}
	var body any
	if req.BodyTemplate != nil {
		body = Render(req.BodyTemplate, tctx, env)
	}
	omitEmpty(req.OmitEmpty, headers, params, body)

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
		if s := a.Secondary; s != nil {
			sv := env(s.EnvVar)
			if sv == "" {
				return nil, m.Name + ": missing " + s.EnvVar
			}
			if s.Type == "header" {
				headers[s.Param] = sv
			} else {
				params[s.Param] = sv
			}
		}
	}

	if err := p.check(target); err != nil {
		return nil, "blocked_url: " + truncRunes(blockedReason(err), 60)
	}
	if hosts != nil && !hosts[hostOf(target)] {
		return nil, "blocked_url: host not allowed"
	}
	return &Prepared{Target: target, Headers: headers, Params: params, Body: body, Spec: req}, ""
}

// missingReference reports a {{response.x}} reference without a default that has no value in the
// first response: polling a URL with an empty id would hit the wrong resource.
func missingReference(v any, ctx map[string]any) bool {
	switch x := v.(type) {
	case RequestSpec:
		if missingReference(x.URL, ctx) || missingReference(x.BodyTemplate, ctx) {
			return true
		}
		for _, h := range x.Headers {
			if missingReference(h, ctx) {
				return true
			}
		}
		for _, q := range x.Query {
			if missingReference(q, ctx) {
				return true
			}
		}
	case string:
		for _, g := range varRE.FindAllStringSubmatch(x, -1) {
			path := pyStrip(g[1])
			if !strings.HasPrefix(path, "response.") || g[2] != "" {
				continue
			}
			if s, ok := condText(lookup(path, ctx)); !ok || s == "" {
				return true
			}
		}
	case map[string]any:
		for _, e := range x {
			if missingReference(e, ctx) {
				return true
			}
		}
	case []any:
		for _, e := range x {
			if missingReference(e, ctx) {
				return true
			}
		}
	}
	return false
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
// plus its explicit auth.extra_env list, minus the platform deny-list. Names
// merely written as ${env:NAME} in templates are not granted; everything not
// allowed resolves to "".
func allowedEnvNames(m Manifest) map[string]struct{} {
	names := map[string]struct{}{}
	if m.Auth.EnvVar != "" && !EnvDenied(m.Auth.EnvVar) {
		names[m.Auth.EnvVar] = struct{}{}
	}
	if n := secondaryEnv(m.Auth); n != "" && !EnvDenied(n) {
		names[n] = struct{}{}
	}
	for _, n := range m.Auth.ExtraEnv {
		if n != "" && !EnvDenied(n) {
			names[n] = struct{}{}
		}
	}
	return names
}

// templateEnvRefs lists the names written as ${env:NAME} in the manifest's templates. They are
// not readable (see allowedEnvNames) but still mark the request as secret-bearing.
func templateEnvRefs(m Manifest) map[string]struct{} {
	names := map[string]struct{}{}
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

// EnvDenied reports whether name is a platform secret a manifest may never read.
func EnvDenied(name string) bool {
	u := strings.ToUpper(name)
	for _, p := range []string{"DATABASE_URL", "STRIPE_"} {
		if strings.HasPrefix(u, p) {
			return true
		}
	}
	if strings.HasSuffix(u, "_MASTER_KEY") {
		return true
	}
	switch u {
	case "SECRET_KEY", "SECRETS_MASTER_KEY", "INTERNAL_SERVICE_TOKEN", "REDIS_URL", "SMTP_PASSWORD", "IMAP_PASSWORD":
		return true
	}
	return false
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

// omitEmpty drops the named headers, query parameters and top-level body keys whose rendered
// value is "" (request.omit_empty). Credentials are added afterwards and are never affected.
func omitEmpty(names []string, headers, params map[string]string, body any) {
	obj, _ := body.(map[string]any)
	for _, n := range names {
		if headers[n] == "" {
			delete(headers, n)
		}
		if params[n] == "" {
			delete(params, n)
		}
		if s, ok := obj[n].(string); ok && s == "" {
			delete(obj, n)
		}
	}
}

// ApplyValueMaps applies response.value_maps to projected fields. The input map is not mutated.
func ApplyValueMaps(fields map[string]any, maps map[string]ValueMap) map[string]any {
	if len(maps) == 0 || len(fields) == 0 {
		return fields
	}
	out := make(map[string]any, len(fields))
	for k, v := range fields {
		out[k] = v
	}
	for field, vm := range maps {
		v, ok := out[field]
		if !ok {
			continue
		}
		if mapped, hit := vm.Values[pyStr(v)]; hit {
			out[field] = mapped
		} else if vm.Default != nil {
			out[field] = *vm.Default
		}
	}
	return out
}
