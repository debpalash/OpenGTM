package manifest

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/pycompat"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/template"
)

// Kinds and runtimes of manifest v2.
var (
	Kinds    = []string{"provider", "scraper", "signal", "destination", "function", "tool"}
	Runtimes = []string{"declarative", "wasm", "process"}
)

// Default limits applied when a manifest omits them.
const (
	DefaultRPS              = 1.0
	DefaultTimeoutSeconds   = 20.0
	DefaultMaxPages         = 10
	DefaultMaxResponseBytes = 5 << 20
	DefaultMemoryMB         = 64
)

// Capabilities is what a plugin may touch. The host enforces it.
type Capabilities struct {
	Network []string `json:"network"`
	Secrets []string `json:"secrets"`
	Browser bool     `json:"browser"`
}

// Limits bounds a plugin's resource use.
type Limits struct {
	RequestsPerSecondPerDomain float64 `json:"requests_per_second_per_domain"`
	TimeoutSeconds             float64 `json:"timeout_seconds"`
	MaxPages                   int     `json:"max_pages"`
	MaxResponseBytes           int64   `json:"max_response_bytes"`
	MemoryMB                   int     `json:"memory_mb"`
}

// Provider is the declarative provider block (same shape as manifest v1).
type Provider struct {
	Capability        string
	DefaultConfidence float64
	CostPerLookup     float64
	Auth              AuthSpec
	Request           RequestSpec
	Response          ResponseSpec
	InputFields       []string
}

// Field is one named selector of a scraper.
type Field struct {
	Name     string
	Selector string
}

// Paginate follows "next page" links.
type Paginate struct {
	Next     string
	MaxPages int
}

// Scrape is the declarative scraper block.
type Scrape struct {
	Start    string
	Items    string
	Fields   []Field
	Format   string // "html" (default) or "json"
	Paginate *Paginate
}

// Wasm points at a WebAssembly module pinned by SHA-256.
type Wasm struct {
	Module string
	SHA256 string
}

// Process describes an out-of-process plugin (validated, not executed here).
type Process struct {
	Command []string
}

// Plugin is the normalized manifest. v1 connectors load into it as
// declarative providers with derived capabilities.
type Plugin struct {
	ManifestVersion string
	Name            string
	DisplayName     string
	Kind            string
	Runtime         string
	Version         string
	Author          string
	License         string
	Description     string
	Homepage        string
	Tags            []string
	Capabilities    Capabilities
	Limits          Limits
	Inputs          map[string]any // normalized JSON Schema (type: object)
	Outputs         any            // entity name (string) or JSON Schema
	Config          map[string]any // JSON Schema or nil
	Provider        *Provider
	Scrape          *Scrape
	Wasm            *Wasm
	Process         *Process

	// V1 is set when the manifest was a v1 connector.
	V1 *V1
	// Path is the manifest file; Dir its directory (module paths are relative to it).
	Path string
	Dir  string

	network Network
}

// Network returns the compiled network capability.
func (p *Plugin) Network() Network { return p.network }

// HasSecret reports whether name is a declared secret.
func (p *Plugin) HasSecret(name string) bool {
	for _, s := range p.Capabilities.Secrets {
		if s == name {
			return true
		}
	}
	return false
}

// Issue2 is one v2 validation problem.
type Issue2 struct {
	Path string `json:"path"`
	Msg  string `json:"message"`
}

// ValidationError lists every problem found in a v2 manifest.
type ValidationError struct{ Issues []Issue2 }

func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Issues))
	for i, is := range e.Issues {
		if is.Path == "" {
			parts[i] = is.Msg
		} else {
			parts[i] = is.Path + ": " + is.Msg
		}
	}
	return "invalid plugin manifest: " + strings.Join(parts, "; ")
}

type v2check struct {
	issues []Issue2
	raw    *pycompat.Map // ordered source document, for key order
}

func (c *v2check) add(path, format string, args ...any) {
	c.issues = append(c.issues, Issue2{Path: path, Msg: fmt.Sprintf(format, args...)})
}

var (
	semverRE   = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-((?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?(?:\+([0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?$`)
	secretRE   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	entityRE   = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	fieldRE    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	selectorRE = regexp.MustCompile(`^(css|json|re):.+`)
	sha256RE   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	envRefRE   = regexp.MustCompile(`\$\{env:([A-Za-z0-9_]+)\}`)
	inputTypes = map[string]bool{"string": true, "number": true, "integer": true, "boolean": true, "object": true, "array": true}
)

var topLevelKeys = map[string]bool{
	"manifest_version": true, "name": true, "display_name": true, "kind": true, "runtime": true,
	"version": true, "author": true, "license": true, "description": true, "homepage": true,
	"tags": true, "capabilities": true, "limits": true, "inputs": true, "outputs": true, "config": true,
	"capability": true, "default_confidence": true, "cost_per_lookup": true, "auth": true,
	"request": true, "response": true, "input_fields": true,
	"scrape": true, "wasm": true, "process": true,
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// strict object key check; "x-" prefixed keys are extensions.
func (c *v2check) keys(path string, m map[string]any, allowed ...string) {
	for k := range m {
		if strings.HasPrefix(k, "x-") || contains(allowed, k) {
			continue
		}
		c.add(path, "unknown key %q", k)
	}
}

func (c *v2check) str(path string, v any, required bool) (string, bool) {
	if v == nil {
		if required {
			c.add(path, "is required")
		}
		return "", false
	}
	s, ok := v.(string)
	if !ok {
		c.add(path, "must be a string")
		return "", false
	}
	return s, true
}

func (c *v2check) num(path string, v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			c.add(path, "must be a finite number")
			return 0, false
		}
		return t, true
	case int64:
		return float64(t), true
	}
	c.add(path, "must be a number")
	return 0, false
}

func (c *v2check) integer(path string, v any) (int64, bool) {
	switch t := v.(type) {
	case int64:
		return t, true
	case float64:
		if t == math.Trunc(t) && !math.IsInf(t, 0) {
			return int64(t), true
		}
	}
	c.add(path, "must be an integer")
	return 0, false
}

func (c *v2check) strs(path string, v any, unique bool) []string {
	list, ok := v.([]any)
	if !ok {
		c.add(path, "must be a list of strings")
		return nil
	}
	out := make([]string, 0, len(list))
	seen := map[string]bool{}
	for i, e := range list {
		s, ok := e.(string)
		if !ok {
			c.add(fmt.Sprintf("%s[%d]", path, i), "must be a string")
			continue
		}
		if unique && seen[s] {
			c.add(fmt.Sprintf("%s[%d]", path, i), "duplicate value %q", s)
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func (c *v2check) obj(path string, v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		c.add(path, "must be a mapping")
	}
	return m, ok
}

// ParseV2 validates a manifest_version "2" document (already decoded with
// pycompat.LoadYAML). The rules match packages/contracts/
// plugin-manifest.v2.schema.json plus semantic checks a schema cannot express
// (secret references, network coverage of static URLs, schema compilation).
func ParseV2(raw *pycompat.Map) (*Plugin, error) {
	plainAny, err := pycompat.ToPlain(raw)
	if err != nil {
		return nil, &ValidationError{Issues: []Issue2{{Msg: "manifest must be JSON-compatible: " + err.Error()}}}
	}
	doc := plainAny.(map[string]any)
	c := &v2check{raw: raw}
	p := &Plugin{ManifestVersion: "2", Capabilities: Capabilities{Network: []string{}, Secrets: []string{}}, Tags: []string{}}

	for k := range doc {
		if !topLevelKeys[k] && !strings.HasPrefix(k, "x-") {
			c.add("", "unknown key %q", k)
		}
	}
	if v, _ := doc["manifest_version"].(string); v != "2" {
		c.add("manifest_version", `must be "2"`)
	}
	if s, ok := c.str("name", doc["name"], true); ok {
		if !nameRE.MatchString(s) {
			c.add("name", "must match ^[a-z][a-z0-9_]{2,63}$")
		}
		p.Name = s
	}
	if s, ok := c.str("kind", doc["kind"], true); ok {
		if !contains(Kinds, s) {
			c.add("kind", "must be one of %s", strings.Join(Kinds, ", "))
		}
		p.Kind = s
	}
	if s, ok := c.str("runtime", doc["runtime"], true); ok {
		if !contains(Runtimes, s) {
			c.add("runtime", "must be one of %s", strings.Join(Runtimes, ", "))
		}
		p.Runtime = s
	}
	if s, ok := c.str("version", doc["version"], true); ok {
		if !semverRE.MatchString(s) {
			c.add("version", "must be a semantic version (e.g. 0.1.0)")
		}
		p.Version = s
	}
	for key, dst := range map[string]*string{"display_name": &p.DisplayName, "author": &p.Author, "license": &p.License, "description": &p.Description, "homepage": &p.Homepage} {
		if v, ok := doc[key]; ok {
			if s, ok := c.str(key, v, false); ok {
				*dst = s
			}
		}
	}
	if v, ok := doc["tags"]; ok {
		p.Tags = c.strs("tags", v, true)
	}
	parseCapabilities(c, doc, p)
	parseLimits(c, doc, p)
	parseIO(c, doc, p)

	declarative := p.Runtime == "declarative"
	providerKeys := []string{"auth", "request", "response", "input_fields"}
	if p.Kind != "provider" {
		for _, k := range append([]string{"capability", "default_confidence", "cost_per_lookup"}, providerKeys...) {
			if _, ok := doc[k]; ok {
				c.add(k, "is only valid for kind provider")
			}
		}
	} else if !declarative {
		for _, k := range providerKeys {
			if _, ok := doc[k]; ok {
				c.add(k, "is only valid for declarative providers")
			}
		}
	}
	if _, ok := doc["scrape"]; ok && !(p.Kind == "scraper" && declarative) {
		c.add("scrape", "is only valid for declarative scrapers")
	}
	if _, ok := doc["wasm"]; ok && p.Runtime != "wasm" {
		c.add("wasm", "is only valid for runtime wasm")
	}
	if _, ok := doc["process"]; ok && p.Runtime != "process" {
		c.add("process", "is only valid for runtime process")
	}

	switch p.Runtime {
	case "declarative":
		switch p.Kind {
		case "provider":
			parseProvider(c, doc, p, true)
		case "scraper":
			parseScrape(c, doc, p)
		case "":
		default:
			c.add("kind", "declarative runtime supports kinds provider and scraper")
		}
		if len(p.Capabilities.Network) == 0 {
			c.add("capabilities.network", "declarative plugins must declare at least one network pattern")
		}
	case "wasm":
		parseWasm(c, doc, p)
		if p.Kind == "provider" {
			parseProvider(c, doc, p, false)
		}
	case "process":
		parseProcess(c, doc, p)
		if p.Kind == "provider" {
			parseProvider(c, doc, p, false)
		}
	}
	if len(c.issues) == 0 {
		semanticChecks(c, p)
	}
	if len(c.issues) > 0 {
		sort.SliceStable(c.issues, func(i, j int) bool { return c.issues[i].Path < c.issues[j].Path })
		return nil, &ValidationError{Issues: c.issues}
	}
	return p, nil
}

func parseCapabilities(c *v2check, doc map[string]any, p *Plugin) {
	v, ok := doc["capabilities"]
	if !ok {
		return
	}
	m, ok := c.obj("capabilities", v)
	if !ok {
		return
	}
	c.keys("capabilities", m, "network", "secrets", "browser")
	if n, ok := m["network"]; ok {
		p.Capabilities.Network = c.strs("capabilities.network", n, true)
		for i, pat := range p.Capabilities.Network {
			if _, err := ParsePattern(pat); err != nil {
				c.add(fmt.Sprintf("capabilities.network[%d]", i), "%v", err)
			}
		}
	}
	if s, ok := m["secrets"]; ok {
		p.Capabilities.Secrets = c.strs("capabilities.secrets", s, true)
		for i, name := range p.Capabilities.Secrets {
			if !secretRE.MatchString(name) {
				c.add(fmt.Sprintf("capabilities.secrets[%d]", i), "secret names must match ^[A-Za-z_][A-Za-z0-9_]*$")
			}
		}
	}
	if b, ok := m["browser"]; ok {
		bv, isBool := b.(bool)
		if !isBool {
			c.add("capabilities.browser", "must be a boolean")
		}
		p.Capabilities.Browser = bv
	}
}

func parseLimits(c *v2check, doc map[string]any, p *Plugin) {
	p.Limits = Limits{DefaultRPS, DefaultTimeoutSeconds, DefaultMaxPages, DefaultMaxResponseBytes, DefaultMemoryMB}
	v, ok := doc["limits"]
	if !ok {
		return
	}
	m, ok := c.obj("limits", v)
	if !ok {
		return
	}
	c.keys("limits", m, "requests_per_second_per_domain", "timeout_seconds", "max_pages", "max_response_bytes", "memory_mb")
	if x, ok := m["requests_per_second_per_domain"]; ok {
		if f, ok := c.num("limits.requests_per_second_per_domain", x); ok {
			if f <= 0 || f > 100 {
				c.add("limits.requests_per_second_per_domain", "must be > 0 and <= 100")
			}
			p.Limits.RequestsPerSecondPerDomain = f
		}
	}
	if x, ok := m["timeout_seconds"]; ok {
		if f, ok := c.num("limits.timeout_seconds", x); ok {
			if f <= 0 || f > 300 {
				c.add("limits.timeout_seconds", "must be > 0 and <= 300")
			}
			p.Limits.TimeoutSeconds = f
		}
	}
	if x, ok := m["max_pages"]; ok {
		if n, ok := c.integer("limits.max_pages", x); ok {
			if n < 1 || n > 1000 {
				c.add("limits.max_pages", "must be between 1 and 1000")
			}
			p.Limits.MaxPages = int(n)
		}
	}
	if x, ok := m["max_response_bytes"]; ok {
		if n, ok := c.integer("limits.max_response_bytes", x); ok {
			if n < 1 || n > 50<<20 {
				c.add("limits.max_response_bytes", "must be between 1 and 52428800")
			}
			p.Limits.MaxResponseBytes = n
		}
	}
	if x, ok := m["memory_mb"]; ok {
		if n, ok := c.integer("limits.memory_mb", x); ok {
			if n < 1 || n > 4096 {
				c.add("limits.memory_mb", "must be between 1 and 4096")
			}
			p.Limits.MemoryMB = int(n)
		}
	}
}

// NormalizeInputs turns the shorthand {name: type} (append "?" for optional)
// into a JSON Schema object; a mapping with a "type" key is already a schema.
func NormalizeInputs(v map[string]any) (map[string]any, error) {
	if _, isSchema := v["type"]; isSchema {
		if v["type"] != "object" {
			return nil, errors.New(`an inputs schema must have type "object"`)
		}
		return v, nil
	}
	props := map[string]any{}
	required := []any{}
	names := make([]string, 0, len(v))
	for k := range v {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		t, ok := v[name].(string)
		optional := strings.HasSuffix(t, "?")
		t = strings.TrimSuffix(t, "?")
		if !ok || !inputTypes[t] {
			return nil, fmt.Errorf("input %q: shorthand type must be one of string, number, integer, boolean, object, array (optionally suffixed with ?)", name)
		}
		if !fieldRE.MatchString(name) {
			return nil, fmt.Errorf("input name %q must match ^[A-Za-z_][A-Za-z0-9_]*$", name)
		}
		props[name] = map[string]any{"type": t}
		if !optional {
			required = append(required, name)
		}
	}
	return map[string]any{"type": "object", "properties": props, "required": required}, nil
}

func parseIO(c *v2check, doc map[string]any, p *Plugin) {
	if v, ok := doc["inputs"]; ok {
		if m, ok := c.obj("inputs", v); ok {
			norm, err := NormalizeInputs(m)
			if err != nil {
				c.add("inputs", "%v", err)
			} else if err := CheckSchema(norm); err != nil {
				c.add("inputs", "invalid JSON Schema: %v", err)
			}
			p.Inputs = norm
		}
	}
	if v, ok := doc["outputs"]; ok {
		switch t := v.(type) {
		case string:
			if !entityRE.MatchString(t) {
				c.add("outputs", "entity names must match ^[a-z][a-z0-9_]*$")
			}
		case map[string]any:
			if t["type"] != "object" {
				c.add("outputs", `an outputs schema must have type "object"`)
			} else if err := CheckSchema(t); err != nil {
				c.add("outputs", "invalid JSON Schema: %v", err)
			}
		default:
			c.add("outputs", "must be an entity name or a JSON Schema object")
		}
		p.Outputs = v
	}
	if v, ok := doc["config"]; ok {
		if m, ok := c.obj("config", v); ok {
			if m["type"] != "object" {
				c.add("config", `a config schema must have type "object"`)
			} else if err := CheckSchema(m); err != nil {
				c.add("config", "invalid JSON Schema: %v", err)
			}
			p.Config = m
		}
	}
}

func parseProvider(c *v2check, doc map[string]any, p *Plugin, declarative bool) {
	pr := &Provider{DefaultConfidence: 0.7, Auth: AuthSpec{Type: "none"}}
	p.Provider = pr
	if s, ok := c.str("capability", doc["capability"], declarative); ok {
		if !entityRE.MatchString(s) {
			c.add("capability", "must match ^[a-z][a-z0-9_]*$")
		}
		pr.Capability = s
	}
	if v, ok := doc["default_confidence"]; ok {
		if f, ok := c.num("default_confidence", v); ok {
			if f < 0 || f > 1 {
				c.add("default_confidence", "must be between 0 and 1")
			}
			pr.DefaultConfidence = f
		}
	}
	if v, ok := doc["cost_per_lookup"]; ok {
		if f, ok := c.num("cost_per_lookup", v); ok {
			if f < 0 {
				c.add("cost_per_lookup", "must be non-negative")
			}
			pr.CostPerLookup = f
		}
	}
	if !declarative {
		return
	}
	if v, ok := doc["input_fields"]; ok {
		pr.InputFields = c.strs("input_fields", v, true)
	}
	if v, ok := doc["auth"]; ok {
		if m, ok := c.obj("auth", v); ok {
			c.keys("auth", m, "type", "param", "value", "env_var")
			if s, ok := c.str("auth.type", m["type"], false); ok {
				if !contains([]string{"none", "header", "bearer", "query"}, s) {
					c.add("auth.type", "must be one of none, header, bearer, query")
				}
				pr.Auth.Type = s
			}
			for _, f := range []struct {
				key string
				dst **string
			}{{"param", &pr.Auth.Param}, {"value", &pr.Auth.Value}, {"env_var", &pr.Auth.EnvVar}} {
				if x, ok := m[f.key]; ok && x != nil {
					if s, ok := c.str("auth."+f.key, x, false); ok {
						s := s
						*f.dst = &s
					}
				}
			}
			if pr.Auth.Type != "none" && pr.Auth.EnvVarName() == "" {
				c.add("auth.env_var", "is required when auth.type is not none")
			}
		}
	}
	reqRaw, ok := doc["request"]
	if !ok {
		c.add("request", "is required for declarative providers")
	} else if m, ok := c.obj("request", reqRaw); ok {
		c.keys("request", m, "method", "url", "headers", "query", "body", "body_template", "timeout")
		r := &pr.Request
		r.Method, r.Timeout = "POST", p.Limits.TimeoutSeconds
		if s, ok := c.str("request.method", m["method"], false); ok {
			if !contains([]string{"GET", "POST", "PUT", "PATCH"}, s) {
				c.add("request.method", "must be GET, POST, PUT, or PATCH")
			}
			r.Method = s
		}
		if s, ok := c.str("request.url", m["url"], true); ok {
			if !strings.HasPrefix(s, "https://") {
				c.add("request.url", "must use HTTPS")
			}
			r.URL = s
		}
		r.Headers = scalarMap(c, "request.headers", m["headers"])
		r.Query = scalarMap(c, "request.query", m["query"])
		if _, both := m["body"]; both {
			if _, bt := m["body_template"]; bt {
				c.add("request", "use either body or body_template, not both")
			}
		}
		if b, ok := m["body"]; ok {
			r.BodyTemplate = pycompat.FromPlain(b)
		} else if b, ok := m["body_template"]; ok {
			r.BodyTemplate = pycompat.FromPlain(b)
		}
		if t, ok := m["timeout"]; ok {
			if f, ok := c.num("request.timeout", t); ok {
				if f <= 0 || f > 120 {
					c.add("request.timeout", "must be between 0 and 120 seconds")
				}
				r.Timeout = f
			}
		}
	}
	respRaw, ok := doc["response"]
	if !ok {
		c.add("response", "is required for declarative providers")
	} else if m, ok := c.obj("response", respRaw); ok {
		c.keys("response", m, "error_path", "error_message_path", "mappings")
		for key, dst := range map[string]**string{"error_path": &pr.Response.ErrorPath, "error_message_path": &pr.Response.ErrorMessagePath} {
			if x, ok := m[key]; ok && x != nil {
				if s, ok := c.str("response."+key, x, false); ok {
					s := s
					*dst = &s
				}
			}
		}
		mp, ok := m["mappings"].(map[string]any)
		if !ok || len(mp) == 0 {
			c.add("response.mappings", "must be a non-empty mapping of field -> expression")
		} else {
			keys := yamlOrder(c.raw, mp, "response", "mappings")
			for _, k := range keys {
				s, ok := mp[k].(string)
				if !ok {
					c.add("response.mappings."+k, "must be a string expression")
					continue
				}
				pr.Response.Mappings = append(pr.Response.Mappings, template.Mapping{Field: k, Expr: s})
			}
		}
	}
}

// sortedKeys orders mapping keys alphabetically.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// yamlOrder returns the keys of the mapping at path in document order, so
// mappings, fields and headers keep the author's order.
func yamlOrder(raw *pycompat.Map, m map[string]any, path ...string) []string {
	cur := raw
	for _, seg := range path {
		v, _ := cur.Get(seg)
		next, ok := v.(*pycompat.Map)
		if !ok {
			return sortedKeys(m)
		}
		cur = next
	}
	out := make([]string, 0, len(m))
	for _, k := range cur.Keys() {
		if ks, ok := k.(string); ok {
			if _, present := m[ks]; present {
				out = append(out, ks)
			}
		}
	}
	if len(out) != len(m) {
		return sortedKeys(m)
	}
	return out
}

func scalarMap(c *v2check, path string, v any) *pycompat.Map {
	out := pycompat.NewMap()
	if v == nil {
		return out
	}
	m, ok := c.obj(path, v)
	if !ok {
		return out
	}
	for _, k := range yamlOrder(c.raw, m, strings.Split(path, ".")...) {
		switch m[k].(type) {
		case string, float64, int64, bool:
			_ = out.Set(k, pycompat.FromPlain(m[k]))
		default:
			c.add(path+"."+k, "must be a string, number or boolean")
		}
	}
	return out
}

func parseScrape(c *v2check, doc map[string]any, p *Plugin) {
	raw, ok := doc["scrape"]
	if !ok {
		c.add("scrape", "is required for declarative scrapers")
		return
	}
	m, ok := c.obj("scrape", raw)
	if !ok {
		return
	}
	c.keys("scrape", m, "start", "items", "fields", "format", "paginate")
	s := &Scrape{Format: "html"}
	p.Scrape = s
	if v, ok := c.str("scrape.start", m["start"], true); ok {
		if !strings.HasPrefix(v, "https://") {
			c.add("scrape.start", "must use HTTPS")
		}
		s.Start = v
	}
	if v, ok := m["items"]; ok {
		if str, ok := c.str("scrape.items", v, false); ok {
			if !selectorRE.MatchString(str) {
				c.add("scrape.items", "selectors must start with css:, json: or re:")
			}
			s.Items = str
		}
	}
	if v, ok := m["format"]; ok {
		if str, ok := c.str("scrape.format", v, false); ok {
			if str != "html" && str != "json" {
				c.add("scrape.format", "must be html or json")
			}
			s.Format = str
		}
	}
	fields, ok := m["fields"].(map[string]any)
	if !ok || len(fields) == 0 {
		c.add("scrape.fields", "must be a non-empty mapping of field -> selector")
	} else {
		for _, k := range yamlOrder(c.raw, fields, "scrape", "fields") {
			if !fieldRE.MatchString(k) {
				c.add("scrape.fields."+k, "field names must match ^[A-Za-z_][A-Za-z0-9_]*$")
			}
			sel, ok := fields[k].(string)
			if !ok || !selectorRE.MatchString(sel) {
				c.add("scrape.fields."+k, "selectors must start with css:, json: or re:")
				continue
			}
			s.Fields = append(s.Fields, Field{Name: k, Selector: sel})
		}
	}
	if v, ok := m["paginate"]; ok {
		if pm, ok := c.obj("scrape.paginate", v); ok {
			c.keys("scrape.paginate", pm, "next", "max_pages")
			pg := &Paginate{MaxPages: p.Limits.MaxPages}
			if str, ok := c.str("scrape.paginate.next", pm["next"], true); ok {
				if !selectorRE.MatchString(str) {
					c.add("scrape.paginate.next", "selectors must start with css:, json: or re:")
				}
				pg.Next = str
			}
			if x, ok := pm["max_pages"]; ok {
				if n, ok := c.integer("scrape.paginate.max_pages", x); ok {
					if n < 1 || n > 1000 {
						c.add("scrape.paginate.max_pages", "must be between 1 and 1000")
					}
					pg.MaxPages = int(n)
				}
			}
			s.Paginate = pg
		}
	}
}

func parseWasm(c *v2check, doc map[string]any, p *Plugin) {
	raw, ok := doc["wasm"]
	if !ok {
		c.add("wasm", "is required for runtime wasm")
		return
	}
	m, ok := c.obj("wasm", raw)
	if !ok {
		return
	}
	c.keys("wasm", m, "module", "sha256")
	w := &Wasm{}
	p.Wasm = w
	if s, ok := c.str("wasm.module", m["module"], true); ok {
		if err := SafeRelPath(s); err != nil || !strings.HasSuffix(s, ".wasm") {
			c.add("wasm.module", "must be a relative path inside the plugin ending in .wasm")
		}
		w.Module = s
	}
	if s, ok := c.str("wasm.sha256", m["sha256"], true); ok {
		if !sha256RE.MatchString(s) {
			c.add("wasm.sha256", "must be 64 lower-case hex characters")
		}
		w.SHA256 = s
	}
}

func parseProcess(c *v2check, doc map[string]any, p *Plugin) {
	raw, ok := doc["process"]
	if !ok {
		c.add("process", "is required for runtime process")
		return
	}
	m, ok := c.obj("process", raw)
	if !ok {
		return
	}
	c.keys("process", m, "command")
	cmd, _ := m["command"].([]any)
	if len(cmd) == 0 {
		c.add("process.command", "must be a non-empty list of strings")
		return
	}
	p.Process = &Process{Command: c.strs("process.command", m["command"], false)}
}

// SafeRelPath rejects absolute paths and any ".." segment.
func SafeRelPath(s string) error {
	if s == "" || strings.HasPrefix(s, "/") || strings.HasPrefix(s, "\\") || filepath.IsAbs(s) || strings.Contains(s, "\\") {
		return fmt.Errorf("path %q must be relative", s)
	}
	for _, seg := range strings.Split(s, "/") {
		if seg == ".." || seg == "" || seg == "." {
			return fmt.Errorf("path %q must not contain empty, . or .. segments", s)
		}
	}
	return nil
}

// envRefs collects ${env:NAME} references in any string leaf.
func envRefs(v any, out map[string]bool) {
	switch t := v.(type) {
	case string:
		for _, m := range envRefRE.FindAllStringSubmatch(t, -1) {
			out[m[1]] = true
		}
	case *pycompat.Map:
		for _, e := range t.Entries() {
			envRefs(e.Value, out)
		}
	case []any:
		for _, e := range t {
			envRefs(e, out)
		}
	}
}

// staticOrigin returns the URL when its scheme://authority part contains no
// template, so it can be checked against the network capability at load time.
func staticOrigin(raw string) (*url.URL, bool) {
	rest := strings.TrimPrefix(raw, "https://")
	authority := rest
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		authority = rest[:i]
	}
	if strings.Contains(authority, "{{") || strings.Contains(authority, "${") {
		return nil, false
	}
	u, err := url.Parse("https://" + authority + "/")
	if err != nil {
		return nil, false
	}
	return u, true
}

func semanticChecks(c *v2check, p *Plugin) {
	net, err := CompileNetwork(p.Capabilities.Network)
	if err != nil {
		c.add("capabilities.network", "%v", err)
		return
	}
	p.network = net
	if pr := p.Provider; pr != nil && p.Runtime == "declarative" {
		if ev := pr.Auth.EnvVarName(); ev != "" && !p.HasSecret(ev) {
			c.add("auth.env_var", "secret %q must be declared in capabilities.secrets", ev)
		}
		refs := map[string]bool{}
		envRefs(pr.Request.URL, refs)
		envRefs(pr.Request.Headers, refs)
		envRefs(pr.Request.Query, refs)
		envRefs(pr.Request.BodyTemplate, refs)
		if pr.Auth.Value != nil {
			envRefs(*pr.Auth.Value, refs)
		}
		for _, name := range sortedBoolKeys(refs) {
			if !p.HasSecret(name) {
				c.add("request", "${env:%s} is not declared in capabilities.secrets", name)
			}
		}
		if u, ok := staticOrigin(pr.Request.URL); ok {
			if err := net.AllowsURL(withPath(u, pr.Request.URL)); err != nil {
				c.add("request.url", "host is not covered by capabilities.network")
			}
		}
	}
	if s := p.Scrape; s != nil {
		refs := map[string]bool{}
		envRefs(s.Start, refs)
		for _, name := range sortedBoolKeys(refs) {
			if !p.HasSecret(name) {
				c.add("scrape.start", "${env:%s} is not declared in capabilities.secrets", name)
			}
		}
		if u, ok := staticOrigin(s.Start); ok {
			if err := net.AllowsURL(withPath(u, s.Start)); err != nil {
				c.add("scrape.start", "host is not covered by capabilities.network")
			}
		}
	}
}

// withPath keeps the static path prefix (up to the first template) so path
// restricted patterns are honored at load time.
func withPath(origin *url.URL, raw string) *url.URL {
	rest := strings.TrimPrefix(raw, "https://")
	i := strings.Index(rest, "/")
	if i < 0 {
		return origin
	}
	p := rest[i:]
	if j := strings.IndexAny(p, "?#{$"); j >= 0 {
		p = p[:j]
	}
	u := *origin
	u.Path = p
	return &u
}

func sortedBoolKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// FromV1 converts a v1 connector into a v2 declarative provider with derived
// capabilities: network = the endpoint origin (or https://* when the host is
// templated, matching what the Python runtime allowed), secrets = auth.env_var.
func FromV1(m *V1) *Plugin {
	p := &Plugin{
		ManifestVersion: "1", Name: m.Name, DisplayName: m.DisplayName, Kind: "provider", Runtime: "declarative",
		Version: "0.0.0", Author: m.Author, License: m.License, Description: m.Description, Homepage: m.Homepage,
		Tags:         m.Tags,
		Limits:       Limits{DefaultRPS, m.Request.Timeout, DefaultMaxPages, DefaultMaxResponseBytes, DefaultMemoryMB},
		Capabilities: Capabilities{Network: []string{}, Secrets: []string{}},
		Provider: &Provider{Capability: m.Capability, DefaultConfidence: m.DefaultConfidence, CostPerLookup: m.CostPerLookup,
			Auth: m.Auth, Request: m.Request, Response: m.Response, InputFields: m.InputFields},
		Outputs: m.Capability,
		V1:      m,
	}
	if u, ok := staticOrigin(m.Request.URL); ok {
		host := strings.ToLower(u.Hostname())
		pat := "https://" + host
		if strings.Contains(host, ":") {
			pat = "https://[" + host + "]"
		}
		if u.Port() != "" {
			pat += ":" + u.Port()
		}
		if _, err := ParsePattern(pat); err == nil {
			p.Capabilities.Network = []string{pat}
		}
	} else {
		p.Capabilities.Network = []string{"https://*"}
	}
	if ev := m.Auth.EnvVarName(); ev != "" {
		p.Capabilities.Secrets = []string{ev}
	}
	p.network, _ = CompileNetwork(p.Capabilities.Network)
	return p
}

// ManifestFileNames are searched, in order, when a plugin directory is given.
var ManifestFileNames = []string{"plugin.yaml", "plugin.yml", "connector.yaml"}

// Resolve returns the manifest file for a path that may be a directory.
func Resolve(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return path, nil
	}
	for _, n := range ManifestFileNames {
		candidate := filepath.Join(path, n)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s: no plugin.yaml found", path)
}

// Load reads a v1 or v2 manifest (file or plugin directory) into a Plugin.
// v1 manifests get the full Python-compatible validation (CheckV1).
func Load(path string) (*Plugin, error) {
	file, err := Resolve(path)
	if err != nil {
		return nil, err
	}
	text, err := readManifest(file)
	if err != nil {
		return nil, err
	}
	p, err := LoadBytes(file, text)
	if err != nil {
		return nil, err
	}
	abs, _ := filepath.Abs(file)
	p.Path = abs
	p.Dir = filepath.Dir(abs)
	return p, nil
}

// LoadBytes parses manifest text; path is used in messages only.
func LoadBytes(path string, text []byte) (*Plugin, error) {
	raw, err := pycompat.LoadYAML(text)
	if err != nil {
		return nil, err
	}
	m, ok := raw.(*pycompat.Map)
	if !ok {
		return nil, fmt.Errorf("manifest %s is not a mapping", path)
	}
	switch v, _ := m.Get("manifest_version"); v {
	case "2", int64(2), 2.0:
		// Unquoted 2 is routed to v2 so the author gets a v2 error message.
		return ParseV2(m)
	}
	v1, err := ParseV1(m)
	if err != nil {
		return nil, err
	}
	if err := CheckV1(v1, nil); err != nil {
		return nil, err
	}
	if _, err := pycompat.Canonical(m); err != nil {
		return nil, err
	}
	return FromV1(v1), nil
}
