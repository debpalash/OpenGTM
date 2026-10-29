package audit_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
)

func TestSafeMetadataDropsSecretKeysAndBoundsDepth(t *testing.T) {
	in := map[string]any{
		"name":          "x",
		"API_Token":     "t",
		"clientSecret":  "s",
		"Password":      "p",
		"Authorization": "a",
		"set-cookie":    "c",
		"credentials":   "c",
		"long":          strings.Repeat("a", 600),
		"n":             3,
		"nil":           nil,
		"d1":            map[string]any{"d2": map[string]any{"d3": map[string]any{"d4": map[string]any{"d5": 1}}}},
	}
	out := audit.SafeMetadata(in, 0).(map[string]any)
	for _, k := range []string{"API_Token", "clientSecret", "Password", "Authorization", "set-cookie", "credentials"} {
		if _, ok := out[k]; ok {
			t.Errorf("key %q not dropped", k)
		}
	}
	if len(out["long"].(string)) != 500 {
		t.Errorf("string not cut to 500")
	}
	if out["name"] != "x" || out["n"] != 3 || out["nil"] != nil {
		t.Errorf("plain values changed: %v", out)
	}
	// depth 0 map -> d1 (1) -> d2 (2) -> d3 (3) -> d4 (4) is truncated.
	d3 := out["d1"].(map[string]any)["d2"].(map[string]any)["d3"].(map[string]any)
	if d3["d4"] != "[truncated]" {
		t.Errorf("depth cap: got %v", d3["d4"])
	}
}

func TestSafeMetadataBoundsCounts(t *testing.T) {
	m := map[string]any{}
	for i := 0; i < 80; i++ {
		m[strings.Repeat("k", 70)+string(rune('A'+i%26))+string(rune('a'+i/26))] = i
	}
	out := audit.SafeMetadata(m, 0).(map[string]any)
	if len(out) > 50 {
		t.Errorf("keys not capped: %d", len(out))
	}
	for k := range out {
		if len(k) > 64 {
			t.Errorf("key not cut to 64: %d", len(k))
		}
	}
	l := make([]any, 150)
	if got := audit.SafeMetadata(l, 0).([]any); len(got) != 100 {
		t.Errorf("list cap: %d", len(got))
	}
	if got := audit.SafeMetadata([]string{"a", "b"}, 0); !reflect.DeepEqual(got, []any{"a", "b"}) {
		t.Errorf("[]string: %v", got)
	}
}

type inner struct {
	APIToken string
	Name     string
}

type outer struct {
	Child inner
	Note  string `json:"note"`
}

func TestSafeMetadataTypedContainersAreRedacted(t *testing.T) {
	in := map[string]any{
		"a": map[string]string{"api_token": "SECRETVAL", "ok": "1"},
		"b": []map[string]any{{"password": "SECRETVAL", "x": 1}},
		"c": map[string][]string{"k": {"v"}, "cookie": {"SECRETVAL"}},
		"d": outer{Child: inner{APIToken: "SECRETVAL", Name: "n"}, Note: "hi"},
		"e": &inner{APIToken: "SECRETVAL", Name: "p"},
		"f": map[int]string{1: "one"},
		"g": [2]map[string]int{{"secret": 1, "keep": 2}},
	}
	out := audit.SafeMetadata(in, 0)
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "SECRETVAL") || strings.Contains(strings.ToLower(string(raw)), "token") {
		t.Fatalf("secret survived: %s", raw)
	}
	m := out.(map[string]any)
	if m["a"].(map[string]any)["ok"] != "1" {
		t.Errorf("a: %v", m["a"])
	}
	if m["b"].([]any)[0].(map[string]any)["x"] != 1 {
		t.Errorf("b: %v", m["b"])
	}
	items := m["d"].(map[string]any)["Child"].(map[string]any)
	if items["Name"] != "n" || m["d"].(map[string]any)["note"] != "hi" {
		t.Errorf("d: %v", m["d"])
	}
	if m["e"].(map[string]any)["Name"] != "p" || m["f"].(map[string]any)["1"] != "one" {
		t.Errorf("e/f: %v %v", m["e"], m["f"])
	}
	if m["g"].([]any)[0].(map[string]any)["keep"] != 2 {
		t.Errorf("g: %v", m["g"])
	}
}

func TestSafeMetadataTypedDepthCap(t *testing.T) {
	deep := map[string]map[string]map[string]map[string]map[string]string{"a": {"b": {"c": {"d": {"e": "x"}}}}}
	raw, _ := json.Marshal(audit.SafeMetadata(deep, 0))
	if !strings.Contains(string(raw), "[truncated]") || strings.Contains(string(raw), `"e"`) {
		t.Fatalf("%s", raw)
	}
}
