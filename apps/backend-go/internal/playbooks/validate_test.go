package playbooks

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// python_vectors.json is generated from FastAPI (testdata/gen_vectors.py.txt).
type vector struct {
	Name     string          `json:"name"`
	Body     json.RawMessage `json:"body"`
	Status   int             `json:"status"`
	Response json.RawMessage `json:"response"`
}

type vectorFile struct {
	RunCreate []vector `json:"run_create"`
	Create    []vector `json:"create"`
	Patch     []vector `json:"patch"`
	Raw       []struct {
		Name     string          `json:"name"`
		Path     string          `json:"path"`
		Content  string          `json:"content"`
		Status   int             `json:"status"`
		Response json.RawMessage `json:"response"`
	} `json:"raw"`
	ToAPIKeys map[string][]string `json:"to_api_keys"`
}

func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	b, err := os.ReadFile("testdata/python_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectorFile
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func compact(t *testing.T, b []byte) string {
	t.Helper()
	var out bytes.Buffer
	if err := json.Compact(&out, b); err != nil {
		t.Fatalf("compact %s: %v", b, err)
	}
	return out.String()
}

// detailJSON renders errs exactly as the 422 response writer does and returns
// the compact "detail" array text.
func detailJSON(t *testing.T, errs []httpx.FieldError) string {
	t.Helper()
	rec := httptest.NewRecorder()
	httpx.ValidationError(rec, errs...)
	body := rec.Body.String()
	const pre = `{"detail":`
	if !strings.HasPrefix(body, pre) || !strings.HasSuffix(body, "}") {
		t.Fatalf("unexpected 422 envelope %s", body)
	}
	return body[len(pre) : len(body)-1]
}

func wantDetail(t *testing.T, resp json.RawMessage) string {
	t.Helper()
	var env struct {
		Detail json.RawMessage `json:"detail"`
	}
	if err := json.Unmarshal(resp, &env); err != nil {
		t.Fatal(err)
	}
	return compact(t, env.Detail)
}

func decodeAny(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return v
}

func TestParseCreateMatchesPydantic422Vectors(t *testing.T) {
	vf := loadVectors(t)
	if len(vf.Create) < 80 {
		t.Fatalf("expected the full create vector set, got %d", len(vf.Create))
	}
	for _, v := range vf.Create {
		t.Run(v.Name, func(t *testing.T) {
			in, errs := ParseCreate(v.Body)
			if v.Status == 200 {
				if len(errs) != 0 {
					t.Fatalf("want ok, got %s", detailJSON(t, errs))
				}
				got, err := json.Marshal(in)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(decodeAny(t, got), decodeAny(t, v.Response)) {
					t.Fatalf("normalized input\n got %s\nwant %s", got, v.Response)
				}
				return
			}
			if len(errs) == 0 {
				t.Fatalf("want 422 %s, got none", v.Response)
			}
			if got, want := detailJSON(t, errs), wantDetail(t, v.Response); got != want {
				t.Fatalf("422 body\n got %s\nwant %s", got, want)
			}
		})
	}
}

func TestParseCreateInvalidAndEmptyBody(t *testing.T) {
	vf := loadVectors(t)
	if len(vf.Raw) == 0 {
		t.Fatal("no raw vectors")
	}
	for _, v := range vf.Raw {
		t.Run(v.Name+v.Path, func(t *testing.T) {
			var errs []httpx.FieldError
			if v.Path == "/c" {
				_, errs = ParseCreate([]byte(v.Content))
			} else {
				_, errs = ParsePatch([]byte(v.Content))
			}
			if len(errs) != 1 {
				t.Fatalf("want one error, got %d", len(errs))
			}
			var want []struct {
				Type string `json:"type"`
				Loc  []any  `json:"loc"`
				Msg  string `json:"msg"`
			}
			var env struct {
				Detail json.RawMessage `json:"detail"`
			}
			_ = json.Unmarshal(v.Response, &env)
			_ = json.Unmarshal(env.Detail, &want)
			if errs[0].Type != want[0].Type || errs[0].Msg != want[0].Msg || errs[0].Loc[0] != "body" {
				t.Fatalf("got %+v want %+v", errs[0], want[0])
			}
			if v.Name == "empty_body" { // exact body (no position/ctx to approximate)
				if got, w := detailJSON(t, errs), wantDetail(t, v.Response); got != w {
					t.Fatalf("got %s want %s", got, w)
				}
			}
		})
	}
}

// patchFlat renders the present fields of in the way pydantic's
// model_dump(exclude_unset=True) does (null for an explicit clear).
func patchFlat(in PatchInput) map[string]any {
	m := map[string]any{}
	if in.Name != nil {
		m["name"] = *in.Name
	}
	if in.Description != nil {
		m["description"] = *in.Description
	}
	if in.PromptTemplate != nil {
		m["prompt_template"] = *in.PromptTemplate
	}
	if in.OutputFormat != nil {
		m["output_format"] = *in.OutputFormat
	}
	if in.MaxSteps != nil {
		m["max_steps"] = *in.MaxSteps
	}
	if in.CellBudgetUSD != nil {
		m["cell_budget_usd"] = *in.CellBudgetUSD
	}
	if in.Enabled != nil {
		m["enabled"] = *in.Enabled
	}
	if in.ScheduleAudienceSet {
		if in.ScheduleAudienceID == nil {
			m["schedule_audience_id"] = nil
		} else {
			m["schedule_audience_id"] = *in.ScheduleAudienceID
		}
	}
	if in.ScheduleIntervalSet {
		if in.ScheduleIntervalMinutes == nil {
			m["schedule_interval_minutes"] = nil
		} else {
			m["schedule_interval_minutes"] = *in.ScheduleIntervalMinutes
		}
	}
	if in.Steps != nil {
		b, _ := json.Marshal(*in.Steps)
		m["steps"] = decodeAny2(b)
	}
	return m
}

func decodeAny2(b []byte) any { var v any; _ = json.Unmarshal(b, &v); return v }

// nonNullable are the PatchInput fields whose column is NOT NULL. Python
// accepts an explicit null and 500s at commit; the Go port answers the error
// pydantic gives a non-Optional field (decision D3), copied from the create
// vectors name_null / desc_null / ... below.
var nonNullable = map[string]string{
	"name": "name_null", "description": "desc_null", "prompt_template": "", "output_format": "fmt_null",
	"max_steps": "max_steps_null", "cell_budget_usd": "budget_null", "steps": "steps_null", "enabled": "",
}

func TestParsePatchMatchesPydanticVectors(t *testing.T) {
	vf := loadVectors(t)
	for _, v := range vf.Patch {
		t.Run(v.Name, func(t *testing.T) {
			in, errs := ParsePatch(v.Body)
			if v.Status == 200 {
				var want map[string]any
				if err := json.Unmarshal(v.Response, &want); err != nil {
					t.Fatal(err)
				}
				for k := range want {
					if _, ok := nonNullable[k]; ok && want[k] == nil {
						return // covered by TestParsePatchNullNonNullableIs422
					}
				}
				if len(errs) != 0 {
					t.Fatalf("want ok, got %s", detailJSON(t, errs))
				}
				got := patchFlat(in)
				if !reflect.DeepEqual(decodeAny2(mustJSON(t, got)), any(want)) {
					t.Fatalf("present fields\n got %s\nwant %s", mustJSON(t, got), v.Response)
				}
				return
			}
			if len(errs) == 0 {
				t.Fatalf("want 422 %s, got none", v.Response)
			}
			if got, want := detailJSON(t, errs), wantDetail(t, v.Response); got != want {
				t.Fatalf("422 body\n got %s\nwant %s", got, want)
			}
		})
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParsePatchNullNonNullableIs422(t *testing.T) {
	vf := loadVectors(t)
	byName := map[string]vector{}
	for _, v := range vf.Create {
		byName[v.Name] = v
	}
	types := map[string][2]string{
		"name":            {"string_type", "Input should be a valid string"},
		"description":     {"string_type", "Input should be a valid string"},
		"prompt_template": {"string_type", "Input should be a valid string"},
		"output_format":   {"string_type", "Input should be a valid string"},
		"max_steps":       {"int_type", "Input should be a valid integer"},
		"cell_budget_usd": {"float_type", "Input should be a valid number"},
		"enabled":         {"bool_type", "Input should be a valid boolean"},
		"steps":           {"list_type", "Input should be a valid list"},
	}
	// The create vectors Python recorded must agree with the table.
	for field, vecName := range nonNullable {
		if vecName == "" {
			continue
		}
		var env struct{ Detail []struct{ Type, Msg string } }
		_ = json.Unmarshal(byName[vecName].Response, &env)
		if len(env.Detail) != 1 || env.Detail[0].Type != types[field][0] || env.Detail[0].Msg != types[field][1] {
			t.Fatalf("%s: create vector %s disagrees: %+v", field, vecName, env.Detail)
		}
	}
	for field, tm := range types {
		_, errs := ParsePatch([]byte(`{"` + field + `": null}`))
		want := `[{"type":"` + tm[0] + `","loc":["body","` + field + `"],"msg":"` + tm[1] + `","input":null}]`
		if len(errs) != 1 || detailJSON(t, errs) != want {
			t.Errorf("%s: got %s want %s", field, detailJSON(t, errs), want)
		}
	}
	// Nullable columns: explicit null clears (the schedule), no error.
	in, errs := ParsePatch([]byte(`{"schedule_audience_id": null, "schedule_interval_minutes": null}`))
	if len(errs) != 0 || !in.ScheduleAudienceSet || in.ScheduleAudienceID != nil || !in.ScheduleIntervalSet || in.ScheduleIntervalMinutes != nil {
		t.Fatalf("null schedule: %+v %v", in, errs)
	}
	// Absent keys are not set.
	in, _ = ParsePatch([]byte(`{}`))
	if in.ScheduleAudienceSet || in.ScheduleIntervalSet || in.Name != nil || in.Steps != nil {
		t.Fatalf("empty patch has set fields: %+v", in)
	}
}

func TestParseRunCreateMatchesPydanticVectors(t *testing.T) {
	vf := loadVectors(t)
	for _, v := range vf.RunCreate {
		t.Run(v.Name, func(t *testing.T) {
			in, errs := ParseRunCreate(v.Body)
			if v.Status == 200 {
				if len(errs) != 0 {
					t.Fatalf("want ok, got %s", detailJSON(t, errs))
				}
				got := mustJSON(t, map[string]any{"audience_id": in.AudienceID, "max_members": in.MaxMembers})
				if !reflect.DeepEqual(decodeAny2(got), decodeAny2(v.Response)) {
					t.Fatalf("got %s want %s", got, v.Response)
				}
				return
			}
			if got, want := detailJSON(t, errs), wantDetail(t, v.Response); got != want {
				t.Fatalf("422 body\n got %s\nwant %s", got, want)
			}
		})
	}
}

func TestValidOutputFormat(t *testing.T) {
	for in, want := range map[string]bool{"text": true, "json": true, "xml": false, "": false, "TEXT": false} {
		if ValidOutputFormat(in) != want {
			t.Errorf("%q", in)
		}
	}
}
