package browserclient

// Contract tests against testdata/openapi.json, the schema exported by the
// Python service (apps/browser-service: `uv run python -m
// browser_service.openapi_export`). The service's own test fails when that file
// drifts from the code, so a change on either side breaks one of the two suites.

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

type schema = map[string]any

func loadOpenAPI(t *testing.T) schema {
	t.Helper()
	data, err := os.ReadFile("testdata/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc schema
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func component(t *testing.T, doc schema, name string) schema {
	t.Helper()
	s, ok := doc["components"].(schema)["schemas"].(schema)[name].(schema)
	if !ok {
		t.Fatalf("schema %s missing", name)
	}
	return s
}

func deref(t *testing.T, doc schema, s schema) schema {
	t.Helper()
	if ref, ok := s["$ref"].(string); ok {
		return component(t, doc, strings.TrimPrefix(ref, "#/components/schemas/"))
	}
	return s
}

func refName(s any) string {
	m, _ := s.(schema)
	ref, _ := m["$ref"].(string)
	return strings.TrimPrefix(ref, "#/components/schemas/")
}

// validate checks a JSON value against a (small) JSON-schema subset: $ref,
// anyOf, type, enum/const, required, additionalProperties=false, items,
// string/array length bounds and numeric bounds.
func validate(t *testing.T, doc schema, s schema, v any, at string) {
	t.Helper()
	s = deref(t, doc, s)
	if any_, ok := s["anyOf"].([]any); ok {
		for _, alt := range any_ {
			if probe(t, doc, alt.(schema), v) {
				validate(t, doc, alt.(schema), v, at)
				return
			}
		}
		t.Errorf("%s: %v matches no anyOf branch", at, v)
		return
	}
	if enum, ok := s["enum"].([]any); ok {
		found := false
		for _, e := range enum {
			found = found || e == v
		}
		if !found {
			t.Errorf("%s: %v not in enum %v", at, v, enum)
		}
	}
	if c, ok := s["const"]; ok && c != v {
		t.Errorf("%s: %v != const %v", at, v, c)
	}
	switch s["type"] {
	case "object":
		obj, ok := v.(map[string]any)
		if !ok {
			t.Errorf("%s: want object, got %T", at, v)
			return
		}
		props, _ := s["properties"].(schema)
		for _, r := range asList(s["required"]) {
			if _, ok := obj[r]; !ok {
				t.Errorf("%s: required %q missing", at, r)
			}
		}
		for k, val := range obj {
			ps, ok := props[k].(schema)
			if !ok {
				if s["additionalProperties"] == false {
					t.Errorf("%s: field %q not in schema", at, k)
				}
				continue
			}
			validate(t, doc, ps, val, at+"."+k)
		}
	case "array":
		arr, ok := v.([]any)
		if !ok {
			t.Errorf("%s: want array, got %T", at, v)
			return
		}
		if mx, ok := s["maxItems"].(float64); ok && float64(len(arr)) > mx {
			t.Errorf("%s: %d items > %v", at, len(arr), mx)
		}
		if mn, ok := s["minItems"].(float64); ok && float64(len(arr)) < mn {
			t.Errorf("%s: %d items < %v", at, len(arr), mn)
		}
		for i, it := range arr {
			validate(t, doc, s["items"].(schema), it, at+"["+itoa(i)+"]")
		}
	case "string":
		str, ok := v.(string)
		if !ok {
			t.Errorf("%s: want string, got %T", at, v)
			return
		}
		if mn, ok := s["minLength"].(float64); ok && float64(len(str)) < mn {
			t.Errorf("%s: shorter than %v", at, mn)
		}
	case "integer", "number":
		n, ok := v.(float64)
		if !ok {
			t.Errorf("%s: want number, got %T", at, v)
			return
		}
		if s["type"] == "integer" && n != float64(int64(n)) {
			t.Errorf("%s: %v is not an integer", at, n)
		}
		if mx, ok := s["maximum"].(float64); ok && n > mx {
			t.Errorf("%s: %v > %v", at, n, mx)
		}
		if mn, ok := s["minimum"].(float64); ok && n < mn {
			t.Errorf("%s: %v < %v", at, n, mn)
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			t.Errorf("%s: want boolean, got %T", at, v)
		}
	case "null":
		if v != nil {
			t.Errorf("%s: want null", at)
		}
	}
}

// probe is a shallow (type-only) match used to pick an anyOf branch; the
// chosen branch is then validated in full.
func probe(t *testing.T, doc schema, s schema, v any) bool {
	t.Helper()
	s = deref(t, doc, s)
	switch s["type"] {
	case "null":
		return v == nil
	case "string":
		_, ok := v.(string)
		return ok
	case "integer", "number":
		_, ok := v.(float64)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	}
	return false
}

func asList(v any) []string {
	var out []string
	list, _ := v.([]any)
	for _, x := range list {
		out = append(out, x.(string))
	}
	return out
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

// sample builds a JSON value with every property present (first enum value,
// non-null anyOf branch), used to decode responses strictly.
func sample(t *testing.T, doc schema, s schema) any {
	t.Helper()
	s = deref(t, doc, s)
	if alts, ok := s["anyOf"].([]any); ok {
		for _, a := range alts {
			if a.(schema)["type"] != "null" {
				return sample(t, doc, a.(schema))
			}
		}
	}
	if enum, ok := s["enum"].([]any); ok {
		return enum[0]
	}
	if c, ok := s["const"]; ok {
		return c
	}
	switch s["type"] {
	case "object":
		out := map[string]any{}
		for k, ps := range s["properties"].(schema) {
			out[k] = sample(t, doc, ps.(schema))
		}
		return out
	case "array":
		return []any{sample(t, doc, s["items"].(schema))}
	case "string":
		return "eA==" // valid base64 too (image_base64 decodes into []byte)
	case "integer":
		return 7
	case "number":
		return 1.5
	case "boolean":
		return true
	}
	t.Fatalf("cannot sample %v", s)
	return nil
}

// zeroFields reports struct fields (recursively) left at their zero value.
func zeroFields(v reflect.Value, at string, out *[]string) {
	switch v.Kind() {
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if f.Tag.Get("json") == "-" {
				continue
			}
			zeroFields(v.Field(i), at+"."+f.Name, out)
		}
	case reflect.Slice:
		if v.Len() == 0 {
			*out = append(*out, at)
			return
		}
		if v.Type().Elem().Kind() == reflect.Struct {
			zeroFields(v.Index(0), at+"[0]", out)
		}
	default:
		if v.IsZero() {
			*out = append(*out, at)
		}
	}
}

type endpoint struct {
	method, path string
	req          any    // fully populated request (nil for GET)
	reqSchema    string //
	resp         any    // pointer to the Go response type
	respSchema   string
}

func f64(v float64) *float64 { return &v }

func endpoints() []endpoint {
	so := SearchOptions{MaxResults: 5, DelayS: f64(1.5), Timeout: 20 * time.Second}
	return []endpoint{
		{"get", "/v1/health", nil, "", &Health{}, "Health"},
		{"post", "/v1/render", RenderRequest{URL: "https://e.com", Engine: "impersonate", WaitUntil: "load", WaitMS: 100,
			Scroll: true, MaxScrolls: 3, IncludeHTML: true, IncludeText: true, MaxBytes: 4096, UserAgent: "ua",
			Locale: "en-GB", Cookies: []Cookie{{Name: "n", Value: "v", Domain: ".e.com", Path: "/", Secure: true, HTTPOnly: true}},
			Timeout: 30 * time.Second}, "RenderRequest", &RenderResult{}, "RenderResult"},
		{"post", "/v1/screenshot", ScreenshotRequest{URL: "https://e.com", WaitUntil: "load", WaitMS: 1, FullPage: true,
			Format: "png", Quality: 50, ViewportWidth: 800, ViewportHeight: 600, Timeout: time.Second},
			"ScreenshotRequest", &ScreenshotResult{}, "ScreenshotResult"},
		{"post", "/v1/scrape/google_maps", GoogleMapsRequest{Query: "q", City: "c", MaxResults: 5, Timeout: 60 * time.Second},
			"GoogleMapsRequest", &GoogleMapsResult{}, "GoogleMapsResult"},
		{"post", "/v1/scrape/justdial_page", JustDialPageRequest{URL: "https://www.justdial.com/x", Timeout: 30 * time.Second},
			"JustDialPageRequest", &JustDialPageResult{}, "JustDialPageResult"},
		{"post", "/v1/scrape/linkedin", LinkedInRequest{Mode: "people", Queries: []string{"q"}, Cities: []string{"c"},
			CompanyName: "Acme", Titles: []string{"CEO"}, SearchOptions: so}, "LinkedInRequest", &SearchResult{}, "SearchResult"},
		{"post", "/v1/scrape/job_boards", JobBoardsRequest{Board: "indeed", Queries: []string{"q"}, Cities: []string{"c"},
			SearchOptions: so}, "JobBoardsRequest", &SearchResult{}, "SearchResult"},
		{"post", "/v1/scrape/crunchbase", CrunchbaseRequest{Queries: []string{"q"}, Cities: []string{"c"}, SearchOptions: so},
			"CrunchbaseRequest", &SearchResult{}, "SearchResult"},
		{"post", "/v1/scrape/review_directories", ReviewDirectoriesRequest{Site: "ambitionbox", Queries: []string{"q"},
			Cities: []string{"c"}, SearchOptions: so}, "ReviewDirectoriesRequest", &SearchResult{}, "SearchResult"},
		{"post", "/v1/scrape/web_directories", WebDirectoriesRequest{Queries: []string{"q"}, Cities: []string{"c"},
			Sites: []string{"sulekha"}, SearchOptions: so}, "WebDirectoriesRequest", &SearchResult{}, "SearchResult"},
		{"post", "/v1/scrape/google_search", GoogleSearchRequest{Mode: "web", Queries: []string{"q"}, Cities: []string{"c"},
			SearchOptions: so}, "GoogleSearchRequest", &SearchResult{}, "SearchResult"},
	}
}

func TestContractEveryServiceEndpointHasAClientCall(t *testing.T) {
	doc := loadOpenAPI(t)
	covered := map[string]bool{"/healthz": true}
	for _, e := range endpoints() {
		covered[e.path] = true
	}
	var missing []string
	for p := range doc["paths"].(schema) {
		if !covered[p] {
			missing = append(missing, p)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("service endpoints without a client call: %v", missing)
	}
}

func TestContractRequests(t *testing.T) {
	doc := loadOpenAPI(t)
	for _, e := range endpoints() {
		op, ok := doc["paths"].(schema)[e.path].(schema)[e.method].(schema)
		if !ok {
			t.Fatalf("%s %s not in schema", e.method, e.path)
		}
		if e.req == nil {
			continue
		}
		rb := op["requestBody"].(schema)["content"].(schema)["application/json"].(schema)["schema"]
		if refName(rb) != e.reqSchema {
			t.Fatalf("%s: request schema %s, want %s", e.path, refName(rb), e.reqSchema)
		}
		data, err := json.Marshal(e.req)
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		if err := json.Unmarshal(data, &v); err != nil {
			t.Fatal(err)
		}
		s := component(t, doc, e.reqSchema)
		validate(t, doc, s, v, e.reqSchema)
		// the Go type can express every request property
		var keys, props []string
		for k := range v {
			keys = append(keys, k)
		}
		for k := range s["properties"].(schema) {
			props = append(props, k)
		}
		sort.Strings(keys)
		sort.Strings(props)
		if !reflect.DeepEqual(keys, props) {
			t.Errorf("%s: Go sends %v, schema has %v", e.reqSchema, keys, props)
		}
	}
}

func TestContractResponses(t *testing.T) {
	doc := loadOpenAPI(t)
	for _, e := range endpoints() {
		op := doc["paths"].(schema)[e.path].(schema)[e.method].(schema)
		resps := op["responses"].(schema)
		ok := resps["200"].(schema)["content"].(schema)["application/json"].(schema)["schema"]
		if refName(ok) != e.respSchema {
			t.Fatalf("%s: response schema %s, want %s", e.path, refName(ok), e.respSchema)
		}
		if e.path != "/healthz" {
			for _, code := range []string{"400", "401", "403", "429", "502", "503", "504"} {
				r, _ := resps[code].(schema)
				if r == nil || refName(r["content"].(schema)["application/json"].(schema)["schema"]) != "ErrorEnvelope" {
					t.Errorf("%s: %s must document ErrorEnvelope", e.path, code)
				}
			}
		}
		data, _ := json.Marshal(sample(t, doc, component(t, doc, e.respSchema)))
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		out := reflect.New(reflect.TypeOf(e.resp).Elem()).Interface()
		if err := dec.Decode(out); err != nil {
			t.Errorf("%s: schema field unknown to Go: %v", e.respSchema, err)
			continue
		}
		var zero []string
		zeroFields(reflect.ValueOf(out).Elem(), e.respSchema, &zero)
		if len(zero) > 0 {
			t.Errorf("%s: Go fields not filled from the schema (not in it?): %v", e.respSchema, zero)
		}
	}
}

func TestContractErrorEnvelope(t *testing.T) {
	doc := loadOpenAPI(t)
	body := component(t, doc, "ErrorBody")["properties"].(schema)
	for _, k := range []string{"code", "message", "fields"} {
		if _, ok := body[k]; !ok {
			t.Fatalf("ErrorBody.%s missing", k)
		}
	}
}
