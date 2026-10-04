package manifest

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/pycompat"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/template"
)

// AuthSpec mirrors the Python AuthSpec model.
type AuthSpec struct {
	Type   string  `json:"type"` // header | bearer | query | none
	Param  *string `json:"param"`
	Value  *string `json:"value"`
	EnvVar *string `json:"env_var"`
}

// EnvVarName returns env_var or "".
func (a AuthSpec) EnvVarName() string {
	if a.EnvVar == nil {
		return ""
	}
	return *a.EnvVar
}

// RequestSpec mirrors the Python RequestSpec model.
type RequestSpec struct {
	Method       string
	URL          string
	Headers      *pycompat.Map // str -> Any
	Query        *pycompat.Map // str -> Any
	BodyTemplate any           // nil when absent (Optional[Any])
	Timeout      float64
}

// ResponseSpec mirrors the Python ResponseSpec model.
type ResponseSpec struct {
	ErrorPath        *string
	ErrorMessagePath *string
	Mappings         []template.Mapping // ordered
}

// V1 mirrors ProviderManifest (connector manifest v1).
type V1 struct {
	ManifestVersion   string
	Name              string
	DisplayName       string
	Author            string
	Homepage          string
	License           string
	Tags              []string
	Capability        string
	Capabilities      []string
	Description       string
	DefaultConfidence float64
	CostPerLookup     float64
	Auth              AuthSpec
	Request           RequestSpec
	Response          ResponseSpec
	InputFields       []string
}

// AllCapabilities mirrors ProviderManifest.all_capabilities().
func (m *V1) AllCapabilities() []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	add(m.Capability)
	for _, c := range m.Capabilities {
		add(c)
	}
	for _, mp := range m.Response.Mappings {
		add(mp.Field)
	}
	return out
}

// DisplayNameOrDefault mirrors `display_name or name.replace("_", " ").title()`.
func (m *V1) DisplayNameOrDefault() string {
	if m.DisplayName != "" {
		return m.DisplayName
	}
	return pyTitle(strings.ReplaceAll(m.Name, "_", " "))
}

func pyTitle(s string) string {
	var b strings.Builder
	prevCased := false
	for _, r := range s {
		if prevCased {
			b.WriteRune(unicode.ToLower(r))
		} else {
			b.WriteRune(unicode.ToTitle(r))
		}
		prevCased = unicode.IsUpper(r) || unicode.IsLower(r) || unicode.IsTitle(r)
	}
	return b.String()
}

// CatalogEntry mirrors ProviderManifest.catalog_entry() as an ordered map.
func (m *V1) CatalogEntry() *pycompat.Map {
	out := pycompat.NewMap()
	set := func(k string, v any) { _ = out.Set(k, v) }
	set("manifest_version", m.ManifestVersion)
	set("id", m.Name)
	set("name", m.DisplayNameOrDefault())
	set("author", m.Author)
	set("homepage", m.Homepage)
	set("license", m.License)
	set("description", m.Description)
	set("capabilities", strList(m.AllCapabilities()))
	set("tags", strList(m.Tags))
	set("cost_per_lookup", m.CostPerLookup)
	set("default_confidence", m.DefaultConfidence)
	if m.Auth.EnvVar == nil {
		set("credential_key", nil)
	} else {
		set("credential_key", *m.Auth.EnvVar)
	}
	return out
}

func strList(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

// errNotIterable etc. mirror the Python exceptions raised by _coerce().
func pyDict(v any) (*pycompat.Map, error) {
	switch t := v.(type) {
	case *pycompat.Map:
		return t.Copy(), nil
	case string, []any, pycompat.Tuple, pycompat.Set, pycompat.Bytes:
		var elems []any
		switch tt := t.(type) {
		case string:
			for _, r := range tt {
				elems = append(elems, string(r))
			}
		case []any:
			elems = tt
		case pycompat.Tuple:
			elems = tt
		case pycompat.Set:
			elems = tt
		case pycompat.Bytes:
			for _, c := range tt {
				elems = append(elems, int64(c))
			}
		}
		out := pycompat.NewMap()
		for i, e := range elems {
			var pair []any
			switch et := e.(type) {
			case string:
				for _, r := range et {
					pair = append(pair, string(r))
				}
			case []any:
				pair = et
			case pycompat.Tuple:
				pair = et
			case pycompat.Set:
				pair = et
			case *pycompat.Map:
				pair = et.Keys()
			case pycompat.Bytes:
				for _, c := range et {
					pair = append(pair, int64(c))
				}
			default:
				return nil, fmt.Errorf("cannot convert dictionary update sequence element #%d to a sequence", i)
			}
			if len(pair) != 2 {
				return nil, fmt.Errorf("dictionary update sequence element #%d has length %d; 2 is required", i, len(pair))
			}
			if err := out.Set(pair[0], pair[1]); err != nil {
				return nil, err
			}
		}
		return out, nil
	case pycompat.Timestamp:
		if t.DateOnly {
			return nil, errors.New("'datetime.date' object is not iterable")
		}
		return nil, errors.New("'datetime.datetime' object is not iterable")
	}
	return nil, fmt.Errorf("'%s' object is not iterable", pycompat.TypeName(v))
}

// coerce mirrors _coerce(): request.body -> request.body_template.
func coerce(raw *pycompat.Map) (*pycompat.Map, error) {
	out := raw.Copy()
	reqRaw, _ := out.Get("request")
	var req *pycompat.Map
	if pycompat.Truthy(reqRaw) {
		var err error
		if req, err = pyDict(reqRaw); err != nil {
			return nil, err
		}
	} else {
		req = pycompat.NewMap()
	}
	if req.Has("body") && !req.Has("body_template") {
		body, _ := req.Get("body")
		req.Delete("body")
		_ = req.Set("body_template", body)
	}
	_ = out.Set("request", req)
	return out, nil
}

// ParseV1 mirrors `ProviderManifest(**_coerce(raw))`.
func ParseV1(raw *pycompat.Map) (*V1, error) {
	c, err := coerce(raw)
	if err != nil {
		return nil, err
	}
	for _, k := range c.Keys() {
		if _, ok := k.(string); !ok {
			return nil, errors.New("keywords must be strings")
		}
	}
	v := &validator{}
	m := &V1{ManifestVersion: "1", Author: "community", License: "Apache-2.0", DefaultConfidence: 0.7,
		Auth: AuthSpec{Type: "none"}, Tags: []string{}, Capabilities: []string{}, InputFields: []string{}}
	root := []any{}
	strField := func(name string, dst *string) {
		if r, ok := field(c, name); ok {
			if s, ok := v.str(at(root, name), r); ok {
				*dst = s
			}
		}
	}
	listField := func(name string, dst *[]string) {
		if r, ok := field(c, name); ok {
			if s, ok := v.strList(at(root, name), r); ok {
				*dst = s
			}
		}
	}
	floatField := func(name string, dst *float64) {
		if r, ok := field(c, name); ok {
			if f, ok := v.float(at(root, name), r); ok {
				*dst = f
			}
		}
	}
	// Field order matches the pydantic model so issues come out in order.
	if r, ok := field(c, "manifest_version"); ok {
		if s, ok := v.literal(at(root, "manifest_version"), r, "1"); ok {
			m.ManifestVersion = s
		}
	}
	if r, ok := field(c, "name"); ok {
		if s, ok := v.str(at(root, "name"), r); ok {
			m.Name = s
		}
	} else {
		v.add(at(root, "name"), "missing", "Field required")
	}
	strField("display_name", &m.DisplayName)
	strField("author", &m.Author)
	strField("homepage", &m.Homepage)
	strField("license", &m.License)
	listField("tags", &m.Tags)
	if r, ok := field(c, "capability"); ok {
		if s, ok := v.str(at(root, "capability"), r); ok {
			m.Capability = s
		}
	} else {
		v.add(at(root, "capability"), "missing", "Field required")
	}
	listField("capabilities", &m.Capabilities)
	strField("description", &m.Description)
	floatField("default_confidence", &m.DefaultConfidence)
	floatField("cost_per_lookup", &m.CostPerLookup)
	if r, ok := field(c, "auth"); ok {
		if am, ok := v.model(at(root, "auth"), r, "AuthSpec"); ok {
			parseAuth(v, at(root, "auth"), am, &m.Auth)
		}
	}
	reqRaw, _ := field(c, "request")
	if rm, ok := v.model(at(root, "request"), reqRaw, "RequestSpec"); ok {
		parseRequest(v, at(root, "request"), rm, &m.Request)
	}
	if r, ok := field(c, "response"); ok {
		if rm, ok := v.model(at(root, "response"), r, "ResponseSpec"); ok {
			parseResponse(v, at(root, "response"), rm, &m.Response)
		}
	}
	listField("input_fields", &m.InputFields)
	if len(v.issues) > 0 {
		return nil, &ModelError{Model: "ProviderManifest", Issues: v.issues}
	}
	return m, nil
}

func parseAuth(v *validator, loc []any, m *pycompat.Map, a *AuthSpec) {
	if r, ok := field(m, "type"); ok {
		if s, ok := v.literal(at(loc, "type"), r, "header", "bearer", "query", "none"); ok {
			a.Type = s
		}
	}
	for _, f := range []struct {
		name string
		dst  **string
	}{{"param", &a.Param}, {"value", &a.Value}, {"env_var", &a.EnvVar}} {
		if r, ok := field(m, f.name); ok {
			if s, ok := v.optStr(at(loc, f.name), r); ok {
				*f.dst = s
			}
		}
	}
}

func parseRequest(v *validator, loc []any, m *pycompat.Map, r *RequestSpec) {
	r.Method = "POST"
	r.Timeout = 20
	r.Headers = pycompat.NewMap()
	r.Query = pycompat.NewMap()
	if raw, ok := field(m, "method"); ok {
		if s, ok := v.str(at(loc, "method"), raw); ok {
			r.Method = s
		}
	}
	if raw, ok := field(m, "url"); ok {
		if s, ok := v.str(at(loc, "url"), raw); ok {
			r.URL = s
		}
	} else {
		v.add(at(loc, "url"), "missing", "Field required")
	}
	if raw, ok := field(m, "headers"); ok {
		if h, ok := v.strMap(at(loc, "headers"), raw, false); ok {
			r.Headers = h
		}
	}
	if raw, ok := field(m, "query"); ok {
		if q, ok := v.strMap(at(loc, "query"), raw, false); ok {
			r.Query = q
		}
	}
	if raw, ok := field(m, "body_template"); ok {
		r.BodyTemplate = raw
	}
	if raw, ok := field(m, "timeout"); ok {
		if f, ok := v.float(at(loc, "timeout"), raw); ok {
			r.Timeout = f
		}
	}
}

func parseResponse(v *validator, loc []any, m *pycompat.Map, r *ResponseSpec) {
	if raw, ok := field(m, "error_path"); ok {
		if s, ok := v.optStr(at(loc, "error_path"), raw); ok {
			r.ErrorPath = s
		}
	}
	if raw, ok := field(m, "error_message_path"); ok {
		if s, ok := v.optStr(at(loc, "error_message_path"), raw); ok {
			r.ErrorMessagePath = s
		}
	}
	if raw, ok := field(m, "mappings"); ok {
		if mp, ok := v.strMap(at(loc, "mappings"), raw, true); ok {
			for _, e := range mp.Entries() {
				r.Mappings = append(r.Mappings, template.Mapping{Field: e.Key.(string), Expr: e.Value.(string)})
			}
		}
	}
}

// LoadV1Bytes mirrors load_manifest() on already-read text.
func LoadV1Bytes(path string, text []byte) (*V1, error) {
	raw, err := pycompat.LoadYAML(text)
	if err != nil {
		return nil, err
	}
	m, ok := raw.(*pycompat.Map)
	if !ok {
		return nil, fmt.Errorf("manifest %s is not a mapping", path)
	}
	return ParseV1(m)
}

// isUTF8 is a small helper shared with the directory scanner.
func isUTF8(b []byte) bool { return utf8.Valid(b) }
