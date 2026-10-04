package manifest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sync/atomic"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

var schemaSeq atomic.Uint64

// normalizeJSON round-trips a plain value through JSON so the schema library
// sees canonical types (json.Number for numbers).
func normalizeJSON(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return jsonschema.UnmarshalJSON(bytes.NewReader(b))
}

// CompileSchema compiles a JSON Schema (draft 2020-12 by default). Remote
// $ref loading is disabled: plugin schemas must be self-contained.
func CompileSchema(schema any) (*jsonschema.Schema, error) {
	doc, err := normalizeJSON(schema)
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.UseLoader(denyLoader{})
	url := fmt.Sprintf("mem:///plugin-schema-%d.json", schemaSeq.Add(1))
	if err := c.AddResource(url, doc); err != nil {
		return nil, err
	}
	return c.Compile(url)
}

type denyLoader struct{}

func (denyLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("remote schema references are not allowed: %s", url)
}

// CheckSchema reports whether schema is a valid JSON Schema.
func CheckSchema(schema any) error {
	_, err := CompileSchema(schema)
	return err
}

// ValidateValue validates a plain value against a schema.
func ValidateValue(schema any, value any) error {
	sch, err := CompileSchema(schema)
	if err != nil {
		return err
	}
	doc, err := normalizeJSON(value)
	if err != nil {
		return err
	}
	return sch.Validate(doc)
}

// ValidateInputs checks run inputs against the plugin's inputs schema (if any).
func (p *Plugin) ValidateInputs(inputs map[string]any) error {
	if p.Inputs == nil {
		return nil
	}
	if inputs == nil {
		inputs = map[string]any{}
	}
	if err := ValidateValue(p.Inputs, inputs); err != nil {
		return fmt.Errorf("inputs do not match the plugin's inputs schema: %w", err)
	}
	return nil
}
