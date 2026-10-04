// Package declarative executes declarative (YAML) plugins in the Go host.
//
// RunProvider ports apps/api/services/leadgen/enrichment/declarative/
// compiler.py: request rendering from inputs, auth modes, headers, query,
// JSON body, timeout, error-envelope detection, response mappings, phone
// normalization, confidence and cost, with the same failure strings
// ("http_404", "non_json_response", "no_data", "timeout", "blocked_url: ...",
// "<name>: missing <ENV>"). RunScraper is the declarative scraper engine.
//
// All network access goes through *egress.Client and the plugin's network
// capability; only declared secrets are ever resolved.
package declarative

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/server/internal/egress"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/manifest"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/pycompat"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/template"
)

// SecretResolver returns a workspace secret value ("" when unset). The host
// resolves secrets per workspace at call time.
type SecretResolver interface {
	Secret(ctx context.Context, name string) (string, error)
}

// SecretMap is a SecretResolver backed by a map (CLI and tests).
type SecretMap map[string]string

// Secret implements SecretResolver.
func (m SecretMap) Secret(_ context.Context, name string) (string, error) { return m[name], nil }

// declaredSecrets resolves only the manifest's declared secrets, once, and
// remembers the values so they can be redacted from evidence and errors.
type declaredSecrets struct {
	values map[string]string
}

func resolveDeclared(ctx context.Context, p *manifest.Plugin, r SecretResolver) (*declaredSecrets, error) {
	s := &declaredSecrets{values: map[string]string{}}
	if r == nil {
		return s, nil
	}
	for _, name := range p.Capabilities.Secrets {
		v, err := r.Secret(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("resolve secret %s: %w", name, err)
		}
		s.values[name] = v
	}
	return s, nil
}

// env is the ${env:NAME} resolver: undeclared names render as "".
func (s *declaredSecrets) env(name string) string { return s.values[name] }

// Redact replaces every resolved secret value (raw and URL-encoded) in s.
func (s *declaredSecrets) Redact(text string) string {
	for _, v := range s.values {
		if len(v) < 4 {
			continue
		}
		text = strings.ReplaceAll(text, v, "REDACTED")
		if q := url.QueryEscape(v); q != v {
			text = strings.ReplaceAll(text, q, "REDACTED")
		}
		if p := url.PathEscape(v); p != v {
			text = strings.ReplaceAll(text, p, "REDACTED")
		}
	}
	return text
}

// ProviderEvidence records where provider fields came from.
type ProviderEvidence struct {
	SourceURL   string            `json:"source_url"` // secrets redacted
	Method      string            `json:"method"`
	Status      int               `json:"status"`
	FetchedAt   time.Time         `json:"fetched_at"`
	ContentType string            `json:"content_type,omitempty"`
	Bytes       int               `json:"bytes"`
	SHA256      string            `json:"sha256"`
	Mappings    map[string]string `json:"mappings"` // field -> expression that produced it
}

// ProviderResult mirrors the Python EnrichmentResult plus evidence and cost.
type ProviderResult struct {
	Provider   string            `json:"provider"`
	Success    bool              `json:"success"`
	Fields     *pycompat.Map     `json:"-"`
	FieldsJSON map[string]any    `json:"fields"`
	Confidence float64           `json:"confidence"`
	CostUSD    float64           `json:"cost_usd"`
	DurationMS float64           `json:"duration_ms"`
	Error      string            `json:"error,omitempty"`
	Evidence   *ProviderEvidence `json:"evidence,omitempty"`
}

// ErrNotProvider is returned for manifests that are not declarative providers.
var ErrNotProvider = errors.New("plugin is not a declarative provider")

// RunProvider executes a declarative provider for one set of inputs.
// Provider-level failures are reported in ProviderResult.Error (matching the
// Python strings); the error return is reserved for invalid manifests or
// inputs and context cancellation.
func RunProvider(ctx context.Context, p *manifest.Plugin, inputs map[string]any, secrets SecretResolver, client *egress.Client) (ProviderResult, error) {
	start := time.Now()
	res := ProviderResult{Provider: p.Name}
	if p.Provider == nil || p.Runtime != "declarative" || p.Kind != "provider" {
		return res, ErrNotProvider
	}
	if client == nil {
		return res, errors.New("declarative: egress client is required")
	}
	if err := p.ValidateInputs(inputs); err != nil {
		return res, err
	}
	sec, err := resolveDeclared(ctx, p, secrets)
	if err != nil {
		return res, err
	}
	pr := p.Provider
	req := pr.Request
	tctx := pycompat.NewMap()
	_ = tctx.Set("input", pycompat.FromPlain(inputs))

	rawURL := template.RenderString(req.URL, tctx, sec.env)
	headers := pycompat.NewMap()
	for _, e := range req.Headers.Entries() {
		_ = headers.Set(e.Key, template.RenderString(pycompat.Str(e.Value), tctx, sec.env))
	}
	params := pycompat.NewMap()
	for _, e := range req.Query.Entries() {
		_ = params.Set(e.Key, template.RenderString(pycompat.Str(e.Value), tctx, sec.env))
	}
	var body any
	hasBody := req.BodyTemplate != nil
	if hasBody {
		body = template.RenderTemplate(req.BodyTemplate, tctx, sec.env)
	}

	// Fail fast on a missing key before any network work (compiler.py order).
	authParam := ""
	if a := pr.Auth; a.Type != "none" {
		ev := a.EnvVarName()
		if ev != "" && sec.env(ev) == "" {
			res.Error = fmt.Sprintf("%s: missing %s", p.Name, ev)
			return res, nil
		}
		var value string
		if a.Value != nil && *a.Value != "" {
			value = template.RenderString(*a.Value, pycompat.NewMap(), sec.env)
		} else {
			value = sec.env(ev)
		}
		switch a.Type {
		case "bearer":
			_ = headers.Set("Authorization", "Bearer "+value)
			authParam = "Authorization"
		case "header":
			authParam = "Authorization"
			if a.Param != nil && *a.Param != "" {
				authParam = *a.Param
			}
			_ = headers.Set(authParam, value)
		case "query":
			name := "api_key"
			if a.Param != nil && *a.Param != "" {
				name = *a.Param
			}
			_ = params.Set(name, value)
		}
	}

	finalURL, err := mergeQuery(rawURL, params)
	if err != nil {
		res.Error = "blocked_url: " + template.Truncate("unparseable url", 60)
		return res, nil
	}
	h := http.Header{}
	for _, e := range headers.Entries() {
		h.Set(pycompat.Str(e.Key), pycompat.Str(e.Value))
	}
	var payload []byte
	if hasBody {
		payload, err = pycompat.Dumps(body, pycompat.DumpOptions{ItemSep: ",", KeySep: ":", DisallowNaN: true})
		if err != nil {
			res.Error = template.Truncate(sec.Redact(err.Error()), 120)
			return res, nil
		}
		if h.Get("Content-Type") == "" {
			h.Set("Content-Type", "application/json")
		}
	}
	sensitive := []string{}
	if authParam != "" {
		sensitive = append(sensitive, authParam)
	}
	resp, err := client.Do(ctx, egress.Request{
		Method:           pycompat.Upper(req.Method),
		URL:              finalURL,
		Header:           h,
		Body:             payload,
		RPS:              p.Limits.RequestsPerSecondPerDomain,
		MaxBodyBytes:     p.Limits.MaxResponseBytes,
		Timeout:          time.Duration(req.Timeout * float64(time.Second)),
		Allow:            p.Network().AllowsURL,
		SensitiveHeaders: sensitive,
	})
	res.DurationMS = float64(time.Since(start).Microseconds()) / 1000
	if err != nil {
		if ctx.Err() != nil {
			res.Error = "cancelled"
			return res, ctx.Err()
		}
		res.Error = classifyError(err, sec)
		return res, nil
	}
	res.Evidence = &ProviderEvidence{
		SourceURL: redactURL(resp.Evidence.URL, sec, params, authQueryName(pr.Auth)), Method: pycompat.Upper(req.Method),
		Status: resp.Status, FetchedAt: resp.Evidence.FetchedAt, ContentType: resp.Evidence.ContentType,
		Bytes: resp.Evidence.Bytes, SHA256: resp.Evidence.SHA256, Mappings: map[string]string{},
	}
	if resp.Status >= 400 {
		res.Error = fmt.Sprintf("http_%d", resp.Status)
		return res, nil
	}
	data, err := pycompat.LoadJSON(resp.Body)
	if err != nil {
		res.Error = "non_json_response"
		return res, nil
	}
	rs := pr.Response
	if rs.ErrorPath != nil && *rs.ErrorPath != "" && pycompat.Truthy(template.ProjectValue(data, "$."+*rs.ErrorPath)) {
		var msg any = "provider_error"
		if rs.ErrorMessagePath != nil && *rs.ErrorMessagePath != "" {
			msg = template.ProjectValue(data, "$."+*rs.ErrorMessagePath)
		}
		res.Error = template.Truncate(sec.Redact(pycompat.Str(msg)), 120)
		return res, nil
	}
	fields := NormalizeFields(template.ProjectResponse(data, rs.Mappings))
	res.Fields = fields
	plain, _ := pycompat.ToPlain(fields)
	res.FieldsJSON, _ = plain.(map[string]any)
	for _, m := range rs.Mappings {
		if fields.Has(m.Field) {
			res.Evidence.Mappings[m.Field] = m.Expr
		}
	}
	res.Success = fields.Len() > 0
	res.Confidence = pr.DefaultConfidence
	if res.Success {
		res.CostUSD = pr.CostPerLookup
	} else {
		res.Error = "no_data"
	}
	return res, nil
}

func authQueryName(a manifest.AuthSpec) string {
	if a.Type != "query" {
		return ""
	}
	if a.Param != nil && *a.Param != "" {
		return *a.Param
	}
	return "api_key"
}

// classifyError maps transport failures onto the Python error strings.
func classifyError(err error, sec *declaredSecrets) string {
	var be *egress.BlockedError
	var nd *manifest.ErrNetworkDenied
	switch {
	case errors.As(err, &be):
		return "blocked_url: " + template.Truncate(sec.Redact(be.Reason), 60)
	case errors.As(err, &nd):
		return "blocked_url: " + template.Truncate(sec.Redact(nd.Error()), 60)
	case errors.Is(err, context.DeadlineExceeded) || isTimeout(err):
		return "timeout"
	}
	return template.Truncate(sec.Redact(err.Error()), 120)
}

func isTimeout(err error) bool {
	var t interface{ Timeout() bool }
	return errors.As(err, &t) && t.Timeout()
}

// mergeQuery mirrors httpx 0.28 (the version the Python runtime pins):
// a non-empty params mapping replaces the URL's query string entirely, in
// mapping order; an empty one leaves the URL untouched.
func mergeQuery(raw string, params *pycompat.Map) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if params.Len() == 0 {
		return raw, nil
	}
	parts := make([]string, 0, params.Len())
	for _, e := range params.Entries() {
		parts = append(parts, url.QueryEscape(pycompat.Str(e.Key))+"="+url.QueryEscape(pycompat.Str(e.Value)))
	}
	u.RawQuery = strings.Join(parts, "&")
	return u.String(), nil
}

// redactURL removes secrets from evidence URLs: any declared secret value
// anywhere in the URL and the auth query parameter value.
func redactURL(raw string, sec *declaredSecrets, _ *pycompat.Map, authQuery string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return sec.Redact(raw)
	}
	if authQuery != "" && u.RawQuery != "" {
		parts := strings.Split(u.RawQuery, "&")
		for i, part := range parts {
			k, _, _ := strings.Cut(part, "=")
			if dk, _ := url.QueryUnescape(k); dk == authQuery {
				parts[i] = k + "=REDACTED"
			}
		}
		u.RawQuery = strings.Join(parts, "&")
	}
	return sec.Redact(u.String())
}
