package wbcrud

import (
	"encoding/json"
	"strings"
	"testing"
)

// Cases verified against pydantic:
//
//	ColumnConfig.model_validate({"name": "c", **d}, strict=True).model_dump(exclude_unset=True)
//
// run in legacy/python (want "" = ValidationError).
func TestValidateColumnStrictMatchesPydantic(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"budget int becomes float", `{"id":"c","name":"c","cell_budget_usd":5}`, `{"id":"c","name":"c","cell_budget_usd":5.0}`},
		{"budget float", `{"id":"c","name":"c","cell_budget_usd":5.5}`, `{"id":"c","name":"c","cell_budget_usd":5.5}`},
		{"budget zero", `{"id":"c","name":"c","cell_budget_usd":0}`, `{"id":"c","name":"c","cell_budget_usd":0.0}`},
		{"budget bool", `{"id":"c","name":"c","cell_budget_usd":true}`, ``},
		{"budget string", `{"id":"c","name":"c","cell_budget_usd":"5"}`, ``},
		{"budget negative", `{"id":"c","name":"c","cell_budget_usd":-1}`, ``},
		{"budget overflow", `{"id":"c","name":"c","cell_budget_usd":1e999}`, ``},
		{"width float", `{"id":"c","name":"c","width":200.0}`, ``},
		{"width bool", `{"id":"c","name":"c","width":true}`, ``},
		{"width null", `{"id":"c","name":"c","width":null}`, ``},
		{"width int kept", `{"id":"c","name":"c","width":200}`, `{"id":"c","name":"c","width":200}`},
		{"target_rows float", `{"id":"c","name":"c","target_rows":3.0}`, ``},
		{"max_steps string", `{"id":"c","name":"c","max_steps":"3"}`, ``},
		{"reactive int", `{"id":"c","name":"c","reactive":1}`, ``},
		{"reactive null kept", `{"id":"c","name":"c","reactive":null}`, `{"id":"c","name":"c","reactive":null}`},
		{"policy partial", `{"id":"c","name":"c","policy":{"max_steps":3}}`, `{"id":"c","name":"c","policy":{"max_steps":3}}`},
		{"policy full", `{"id":"c","name":"c","policy":{"prefer":null,"max_cost_usd":2,"max_steps":3}}`, `{"id":"c","name":"c","policy":{"max_steps":3,"max_cost_usd":2.0,"prefer":null}}`},
		{"policy null", `{"id":"c","name":"c","policy":null}`, `{"id":"c","name":"c","policy":null}`},
		{"policy unknown key dropped", `{"id":"c","name":"c","policy":{"x":1}}`, `{"id":"c","name":"c","policy":{}}`},
		{"policy float steps", `{"id":"c","name":"c","policy":{"max_steps":3.0}}`, ``},
		{"policy negative steps", `{"id":"c","name":"c","policy":{"max_steps":-1}}`, ``},
		{"policy not object", `{"id":"c","name":"c","policy":5}`, ``},
		{"model field order", `{"id":"c","name":"c","waterfall":["a"],"http_body":{"a":1},"icp":{"a":1}}`, `{"id":"c","name":"c","waterfall":["a"],"icp":{"a":1},"http_body":{"a":1}}`},
		{"waterfall string", `{"id":"c","name":"c","waterfall":"a"}`, ``},
		{"waterfall item not string", `{"id":"c","name":"c","waterfall":["a",1]}`, ``},
		{"name null", `{"id":"c","name":null}`, ``},
		{"name blank is valid here", `{"id":"c","name":" "}`, `{"id":"c","name":" "}`},
		{"unknown key dropped", `{"id":"c","name":"c","extra":1}`, `{"id":"c","name":"c"}`},
		{"max_tokens zero", `{"id":"c","name":"c","max_tokens":0}`, ``},
		{"max_tokens ok", `{"id":"c","name":"c","max_tokens":100}`, `{"id":"c","name":"c","max_tokens":100}`},
		{"id not string", `{"id":5,"name":"c"}`, ``},
		{"id missing", `{"name":"c"}`, ``},
		{"type null", `{"id":"c","name":"c","type":null}`, ``},
		{"dict ok", `{"id":"c","name":"c","icp":{"z":1,"a":{"y":2.5}}}`, `{"id":"c","name":"c","icp":{"z":1,"a":{"y":2.5}}}`},
		{"dict not object", `{"id":"c","name":"c","icp":[1]}`, ``},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in, err := ParseObject([]byte(c.in))
			if err != nil {
				t.Fatal(err)
			}
			got, ok := ValidateColumnStrict(in)
			if c.want == "" {
				if ok {
					b, _ := json.Marshal(got)
					t.Fatalf("expected invalid, got %s", b)
				}
				return
			}
			if !ok {
				t.Fatalf("expected valid, got invalid")
			}
			b, err := json.Marshal(got)
			if err != nil || string(b) != c.want {
				t.Fatalf("got %s want %s (%v)", b, c.want, err)
			}
		})
	}
}

func TestIsBlankMatchesPythonStrip(t *testing.T) {
	for s, want := range map[string]bool{"": true, "  ": true, "\t\n": true, "\x1c": true, "\u00a0": true, "\u3000": true, "a": false, " a ": false, "\u200b": false} {
		if got := IsBlank(s); got != want {
			t.Errorf("IsBlank(%q) = %v want %v", s, got, want)
		}
	}
	if IsBlank(strings.Repeat(" ", 3) + "x") {
		t.Error("non-blank reported blank")
	}
}

func TestDumpsSortedMirrorsPythonJSONEquality(t *testing.T) {
	parse := func(s string) any {
		v, err := decodeDocument([]byte(s), false)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	same := [][2]string{
		{`{"a":1,"b":[1,2]}`, `{"b":[1,2],"a":1}`},
		{`{"a":1.0}`, `{"a":1.0}`},
		{`1e2`, `100.0`},
		{`-0`, `0`},
	}
	for _, p := range same {
		if DumpsSorted(parse(p[0])) != DumpsSorted(parse(p[1])) {
			t.Errorf("%s != %s", p[0], p[1])
		}
	}
	diff := [][2]string{
		{`1`, `1.0`},
		{`true`, `1`},
		{`null`, `0`},
		{`"1"`, `1`},
		{`[1,2]`, `[2,1]`},
		{`{"a":1}`, `{"a":"1"}`},
	}
	for _, p := range diff {
		if DumpsSorted(parse(p[0])) == DumpsSorted(parse(p[1])) {
			t.Errorf("%s == %s", p[0], p[1])
		}
	}
}
