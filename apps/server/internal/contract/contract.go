// Package contract loads the committed OpenAPI document for the Go server's
// /api/v2 surface (packages/contracts/openapi.v2.yaml) and validates real HTTP
// responses against it.
//
// The document is the contract the typed web client is generated from, so a
// handler that drifts from it would make the client lie. Tests across the
// server call Validate on the responses they already produce; any response
// whose status, content type or body does not match the spec fails the test.
// It is test support, not part of the running server.
package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

// SpecPath is the repository-relative location of the document.
const SpecPath = "packages/contracts/openapi.v2.yaml"

// Operation is one documented method and path.
type Operation struct {
	Method string // upper case
	Path   string // template, e.g. /api/v2/plugin-runs/{id}
	ID     string // operationId
}

func (o Operation) String() string { return o.Method + " " + o.Path }

// Spec is the parsed document.
type Spec struct {
	doc      map[string]any
	compiler *jsonschema.Compiler
	mu       sync.Mutex
	compiled map[string]*jsonschema.Schema
}

var (
	loadOnce sync.Once
	loaded   *Spec
	loadErr  error
)

// Load parses the committed spec once per test binary.
func Load(t testing.TB) *Spec {
	t.Helper()
	loadOnce.Do(func() {
		_, file, _, _ := runtime.Caller(0)
		dir := filepath.Dir(file)
		for {
			p := filepath.Join(dir, SpecPath)
			if _, err := os.Stat(p); err == nil {
				loaded, loadErr = Parse(p)
				return
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				loadErr = fmt.Errorf("%s not found above %s", SpecPath, file)
				return
			}
			dir = parent
		}
	})
	if loadErr != nil {
		t.Fatalf("contract: %v", loadErr)
	}
	return loaded
}

// Parse reads an OpenAPI 3.1 YAML file.
func Parse(path string) (*Spec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var yamlDoc any
	if err := yaml.Unmarshal(raw, &yamlDoc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	// Round-trip through JSON so every value has the types the JSON Schema
	// compiler expects (yaml.v3 yields int, not float64 or json.Number).
	js, err := json.Marshal(toJSONValue(yamlDoc))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(js))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	if err := c.AddResource("spec.json", inst); err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(js, &doc); err != nil {
		return nil, err
	}
	if v, _ := doc["openapi"].(string); !strings.HasPrefix(v, "3.1") {
		return nil, fmt.Errorf("%s: openapi version %q, want 3.1.x (schemas are validated as JSON Schema 2020-12)", path, v)
	}
	return &Spec{doc: doc, compiler: c, compiled: map[string]*jsonschema.Schema{}}, nil
}

func toJSONValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = toJSONValue(e)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[fmt.Sprint(k)] = toJSONValue(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = toJSONValue(e)
		}
		return out
	default:
		return v
	}
}

var methods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

// Operations lists every documented operation, sorted.
func (s *Spec) Operations() []Operation {
	var ops []Operation
	paths, _ := s.doc["paths"].(map[string]any)
	for p, item := range paths {
		m, _ := item.(map[string]any)
		for _, method := range methods {
			op, ok := m[method].(map[string]any)
			if !ok {
				continue
			}
			id, _ := op["operationId"].(string)
			ops = append(ops, Operation{Method: strings.ToUpper(method), Path: p, ID: id})
		}
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].String() < ops[j].String() })
	return ops
}

// operation returns the raw operation object.
func (s *Spec) operation(method, path string) (map[string]any, bool) {
	paths, _ := s.doc["paths"].(map[string]any)
	item, _ := paths[path].(map[string]any)
	op, ok := item[strings.ToLower(method)].(map[string]any)
	return op, ok
}

// resolve follows a {"$ref": "#/..."} to the object it names.
func (s *Spec) resolve(v any) any {
	for range 8 {
		m, ok := v.(map[string]any)
		if !ok {
			return v
		}
		ref, ok := m["$ref"].(string)
		if !ok {
			return v
		}
		cur := any(s.doc)
		for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
			next, _ := cur.(map[string]any)
			cur = next[part]
		}
		v = cur
	}
	return v
}

// schemaFor compiles the response schema for method, path, status and media
// type; it returns nil when the response declares no schema for that type.
func (s *Spec) schemaFor(method, path, status, mediaType string) (*jsonschema.Schema, string, error) {
	op, ok := s.operation(method, path)
	if !ok {
		return nil, "", fmt.Errorf("%s %s is not in the spec", method, path)
	}
	responses, _ := op["responses"].(map[string]any)
	resp, ok := responses[status]
	if !ok {
		return nil, "", fmt.Errorf("%s %s has no documented %s response (documented: %s)", method, path, status, keys(responses))
	}
	rm, _ := s.resolve(resp).(map[string]any)
	content, _ := rm["content"].(map[string]any)
	media, ok := content[mediaType].(map[string]any)
	if !ok {
		return nil, "", fmt.Errorf("%s %s %s: content type %q is not documented (documented: %s)", method, path, status, mediaType, keys(content))
	}
	if _, ok := media["schema"]; !ok {
		return nil, "", nil
	}
	loc := "spec.json#/paths/" + escape(path) + "/" + strings.ToLower(method) + "/responses/" + status
	// A response given as a $ref lives under components/responses.
	if ref, ok := resp.(map[string]any)["$ref"].(string); ok {
		loc = "spec.json" + ref
	}
	loc += "/content/" + escape(mediaType) + "/schema"

	s.mu.Lock()
	defer s.mu.Unlock()
	if sch, ok := s.compiled[loc]; ok {
		return sch, loc, nil
	}
	sch, err := s.compiler.Compile(loc)
	if err != nil {
		return nil, loc, fmt.Errorf("compile %s: %w", loc, err)
	}
	s.compiled[loc] = sch
	return sch, loc, nil
}

func escape(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1") }

func keys(m map[string]any) string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, ", ")
}

// Validate checks a response against the spec: the operation, status and
// content type must be documented, JSON bodies must satisfy the schema, and
// headers the spec marks as present must be (Retry-After on 503 is the one the
// server promises). path is the spec path template, such as
// /api/v2/plugin-runs/{id}.
func (s *Spec) Validate(method, path string, status int, header http.Header, body []byte) error {
	code := fmt.Sprint(status)
	op, ok := s.operation(method, path)
	if !ok {
		return fmt.Errorf("%s %s is not in the spec", method, path)
	}
	responses, _ := op["responses"].(map[string]any)
	resp, ok := responses[code]
	if !ok {
		return fmt.Errorf("%s %s returned %d, which the spec does not document (documented: %s)\nbody: %s",
			method, path, status, keys(responses), truncate(body))
	}
	rm, _ := s.resolve(resp).(map[string]any)
	content, _ := rm["content"].(map[string]any)
	if len(content) == 0 { // e.g. 204: nothing to validate
		if len(bytes.TrimSpace(body)) != 0 {
			return fmt.Errorf("%s %s %d is documented without a body but returned one: %s", method, path, status, truncate(body))
		}
		return nil
	}
	mt, _, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil {
		return fmt.Errorf("%s %s %d: unreadable Content-Type %q", method, path, status, header.Get("Content-Type"))
	}
	if headers, ok := rm["headers"].(map[string]any); ok {
		if _, want := headers["Retry-After"]; want && status == http.StatusServiceUnavailable && header.Get("Retry-After") == "" {
			return fmt.Errorf("%s %s 503 is missing the documented Retry-After header", method, path)
		}
	}
	if mt != "application/json" {
		_, _, err := s.schemaFor(method, path, code, mt) // documented?
		return err
	}
	sch, loc, err := s.schemaFor(method, path, code, mt)
	if err != nil {
		return err
	}
	if sch == nil {
		return nil
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%s %s %d: body is not JSON: %v\nbody: %s", method, path, status, err, truncate(body))
	}
	if err := sch.Validate(inst); err != nil {
		return fmt.Errorf("%s %s %d: body does not match %s:\n%v\nbody: %s", method, path, status, loc, err, truncate(body))
	}
	return nil
}

// ValidateSchema checks an arbitrary JSON document against a named component
// schema, for payloads that are not plain responses (SSE event data).
func (s *Spec) ValidateSchema(component string, body []byte) error {
	loc := "spec.json#/components/schemas/" + component
	s.mu.Lock()
	sch, ok := s.compiled[loc]
	if !ok {
		var err error
		if sch, err = s.compiler.Compile(loc); err != nil {
			s.mu.Unlock()
			return err
		}
		s.compiled[loc] = sch
	}
	s.mu.Unlock()
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		return err
	}
	if err := sch.Validate(inst); err != nil {
		return fmt.Errorf("does not match %s: %v\nbody: %s", component, err, truncate(body))
	}
	return nil
}

// PathOf converts a ServeMux pattern ("GET /api/v2/plugin-runs/{id}") into the
// method and spec path it documents.
func PathOf(pattern string) (method, path string) {
	method, path, ok := strings.Cut(pattern, " ")
	if !ok {
		return "", pattern
	}
	return method, path
}

func truncate(b []byte) string {
	if len(b) > 600 {
		return string(b[:600]) + "..."
	}
	return string(b)
}
