package template

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/pycompat"
)

type parityFile struct {
	Env          map[string]string `json:"env"`
	CtxJSON      string            `json:"ctx_json"`
	DataJSON     string            `json:"data_json"`
	RenderString []struct {
		Template string `json:"template"`
		Result   string `json:"result"`
	} `json:"render_string"`
	RenderTemplate []struct {
		ValueJSON string `json:"value_json"`
		Result    string `json:"result"`
	} `json:"render_template"`
	ProjectValue []struct {
		Expr   string `json:"expr"`
		Result string `json:"result"`
	} `json:"project_value"`
	ProjectResponse []struct {
		MappingsJSON string `json:"mappings_json"`
		Result       string `json:"result"`
	} `json:"project_response"`
	ErrorEnvelope struct {
		Flag    string `json:"flag"`
		Message string `json:"message"`
	} `json:"error_envelope"`
}

func mustLoad(t *testing.T, s string) any {
	t.Helper()
	v, err := pycompat.LoadJSON([]byte(s))
	if err != nil {
		t.Fatalf("load %q: %v", s, err)
	}
	return v
}

func canon(t *testing.T, v any) string {
	t.Helper()
	b, err := pycompat.Canonical(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestPythonParity replays testdata/parity.json, generated from template.py
// by testdata/gen_template_parity.py.
func TestPythonParity(t *testing.T) {
	raw, err := os.ReadFile("testdata/parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx parityFile
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	env := func(name string) string { return fx.Env[name] }
	ctx := mustLoad(t, fx.CtxJSON)
	data := mustLoad(t, fx.DataJSON)

	for _, c := range fx.RenderString {
		if got := RenderString(c.Template, ctx, env); got != c.Result {
			t.Errorf("RenderString(%q)\n go: %q\n py: %q", c.Template, got, c.Result)
		}
	}
	for _, c := range fx.RenderTemplate {
		got := canon(t, RenderTemplate(mustLoad(t, c.ValueJSON), ctx, env))
		if got != c.Result {
			t.Errorf("RenderTemplate(%s)\n go: %s\n py: %s", c.ValueJSON, got, c.Result)
		}
	}
	for _, c := range fx.ProjectValue {
		got := canon(t, ProjectValue(data, c.Expr))
		if got != c.Result {
			t.Errorf("ProjectValue(%q)\n go: %s\n py: %s", c.Expr, got, c.Result)
		}
	}
	for _, c := range fx.ProjectResponse {
		mv := mustLoad(t, c.MappingsJSON).(*pycompat.Map)
		var ms []Mapping
		for _, e := range mv.Entries() {
			ms = append(ms, Mapping{Field: e.Key.(string), Expr: e.Value.(string)})
		}
		got := canon(t, ProjectResponse(data, ms))
		if got != c.Result {
			t.Errorf("ProjectResponse(%s)\n go: %s\n py: %s", c.MappingsJSON, got, c.Result)
		}
	}
	if got := canon(t, ProjectValue(data, "$.error")); got != fx.ErrorEnvelope.Flag {
		t.Errorf("error flag: %s vs %s", got, fx.ErrorEnvelope.Flag)
	}
	if got := Truncate(pycompat.Str(ProjectValue(data, "$.message")), 120); got != fx.ErrorEnvelope.Message {
		t.Errorf("error message: %q vs %q", got, fx.ErrorEnvelope.Message)
	}
	if n := len(fx.RenderString) + len(fx.ProjectValue); n < 80 {
		t.Fatalf("parity corpus too small: %d", n)
	}
}
