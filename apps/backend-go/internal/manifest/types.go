package manifest

import (
	"encoding/json"
	"fmt"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
)

// AuthSpec mirrors the Pydantic AuthSpec (type: header | bearer | query | none).
type AuthSpec struct {
	Type   string `json:"type"`
	Param  string `json:"param"`
	Value  string `json:"value"`
	EnvVar string `json:"env_var"`
	// ExtraEnv lists additional ${env:NAME} variables the manifest may template.
	ExtraEnv []string `json:"extra_env"`
}

// RequestSpec mirrors RequestSpec. Header/query values and the body template
// are decoded with canonjson so ints stay ints and floats stay floats (the
// distinction is visible through str()).
type RequestSpec struct {
	Method       string         `json:"method"`
	URL          string         `json:"url"`
	Headers      map[string]any `json:"headers"`
	Query        map[string]any `json:"query"`
	BodyTemplate any            `json:"body_template"`
	Timeout      float64        `json:"timeout"` // seconds
}

// ResponseSpec mirrors ResponseSpec.
type ResponseSpec struct {
	ErrorPath        string            `json:"error_path"`
	ErrorMessagePath string            `json:"error_message_path"`
	Mappings         map[string]string `json:"mappings"`
}

// Manifest mirrors ProviderManifest.model_dump(mode="json"). Decode it from
// the payload with encoding/json (Unmarshal applies the Pydantic defaults).
type Manifest struct {
	ManifestVersion   string       `json:"manifest_version"`
	Name              string       `json:"name"`
	DisplayName       string       `json:"display_name"`
	Author            string       `json:"author"`
	Homepage          string       `json:"homepage"`
	License           string       `json:"license"`
	Tags              []string     `json:"tags"`
	Capability        string       `json:"capability"`
	Capabilities      []string     `json:"capabilities"`
	Description       string       `json:"description"`
	DefaultConfidence float64      `json:"default_confidence"`
	CostPerLookup     float64      `json:"cost_per_lookup"`
	Auth              AuthSpec     `json:"auth"`
	Request           RequestSpec  `json:"request"`
	Response          ResponseSpec `json:"response"`
	InputFields       []string     `json:"input_fields"`
}

type rawRequest struct {
	Method       *string                    `json:"method"`
	URL          string                     `json:"url"`
	Headers      map[string]json.RawMessage `json:"headers"`
	Query        map[string]json.RawMessage `json:"query"`
	BodyTemplate json.RawMessage            `json:"body_template"`
	Timeout      *float64                   `json:"timeout"`
}

// UnmarshalJSON applies the Pydantic defaults (method POST, timeout 20.0) and
// decodes dynamic values with Python-exact number semantics.
func (r *RequestSpec) UnmarshalJSON(b []byte) error {
	var raw rawRequest
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	out := RequestSpec{Method: "POST", URL: raw.URL, Timeout: 20.0}
	if raw.Method != nil {
		out.Method = *raw.Method
	}
	if raw.Timeout != nil {
		out.Timeout = *raw.Timeout
	}
	var err error
	if out.Headers, err = decodeValues("headers", raw.Headers); err != nil {
		return err
	}
	if out.Query, err = decodeValues("query", raw.Query); err != nil {
		return err
	}
	if len(raw.BodyTemplate) > 0 {
		if out.BodyTemplate, err = canonjson.Decode(raw.BodyTemplate); err != nil {
			return fmt.Errorf("manifest: body_template: %w", err)
		}
	}
	*r = out
	return nil
}

func decodeValues(field string, in map[string]json.RawMessage) (map[string]any, error) {
	out := make(map[string]any, len(in))
	for k, raw := range in {
		v, err := canonjson.Decode(raw)
		if err != nil {
			return nil, fmt.Errorf("manifest: request.%s[%q]: %w", field, k, err)
		}
		out[k] = v
	}
	return out, nil
}

// UnmarshalJSON pre-fills the Pydantic defaults before decoding.
func (m *Manifest) UnmarshalJSON(b []byte) error {
	type plain Manifest
	p := plain{
		ManifestVersion: "1", Author: "community", License: "Apache-2.0",
		DefaultConfidence: 0.7, Auth: AuthSpec{Type: "none"},
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*m = Manifest(p)
	return nil
}

// Result mirrors EnrichmentResult.
type Result struct {
	Success    bool
	Fields     map[string]any
	Confidence float64
	DurationMS float64
	Error      string
	HTTPStatus int
}
