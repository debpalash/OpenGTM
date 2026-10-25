package manifest_test

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
)

type vecFile struct {
	Env          map[string]string `json:"env"`
	Render       []vecRender       `json:"render"`
	ProjectValue []vecProject      `json:"project_value"`
	ProjectResp  []vecProjectResp  `json:"project_response"`
	Normalize    []vecNormalize    `json:"normalize"`
	InputCtx     []vecInputCtx     `json:"input_ctx"`
}

type vecRender struct {
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
	Ctx      json.RawMessage `json:"ctx"`
	Expected json.RawMessage `json:"expected"`
}

type vecProject struct {
	Name     string          `json:"name"`
	Data     json.RawMessage `json:"data"`
	Expr     string          `json:"expr"`
	Expected json.RawMessage `json:"expected"`
}

type vecProjectResp struct {
	Name     string            `json:"name"`
	Data     json.RawMessage   `json:"data"`
	Mappings map[string]string `json:"mappings"`
	Expected json.RawMessage   `json:"expected"`
}

type vecNormalize struct {
	Name     string          `json:"name"`
	Fields   json.RawMessage `json:"fields"`
	Expected json.RawMessage `json:"expected"`
}

type vecInputCtx struct {
	Name     string            `json:"name"`
	Lead     map[string]string `json:"lead"`
	Expected json.RawMessage   `json:"expected"`
}

func loadVectors(t *testing.T) vecFile {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/manifest_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vecFile
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func dec(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	v, err := canonjson.Decode(raw)
	if err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return v
}

func sameValue(t *testing.T, name string, got any, want json.RawMessage) {
	t.Helper()
	w := dec(t, want)
	if !reflect.DeepEqual(got, w) {
		g, _ := canonjson.Marshal(got, true)
		t.Errorf("%s:\n got  %s (%T)\n want %s", name, g, got, want)
	}
}
