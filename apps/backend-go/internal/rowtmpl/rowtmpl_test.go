package rowtmpl

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
)

// fixVal converts {"__float__": "nan"|"inf"} markers (the vector encoding of
// non-JSON floats) back into float64.
func fixVal(v any) any {
	switch t := v.(type) {
	case colgraph.Col:
		if len(t.Keys) == 1 && t.Keys[0] == "__float__" {
			switch t.Vals["__float__"] {
			case "nan":
				return nan()
			case "inf":
				return inf()
			}
		}
		for k, e := range t.Vals {
			t.Vals[k] = fixVal(e)
		}
		return t
	case []any:
		for i, e := range t {
			t[i] = fixVal(e)
		}
	}
	return v
}

type vecCase struct {
	Name      string
	Op        string
	Template  string
	Strict    bool
	Values    map[string]any
	LeadData  map[string]any
	Columns   []colgraph.Col
	Value     any
	Input     any
	Error     string
	HasError  bool
	ErrorType string
}

func loadCases(t *testing.T) []vecCase {
	t.Helper()
	raw, err := os.ReadFile("testdata/rowtmpl.json")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := DecodeOrdered(raw)
	if err != nil {
		t.Fatal(err)
	}
	list := doc.(colgraph.Col).Vals["cases"].([]any)
	var out []vecCase
	for _, it := range list {
		c := it.(colgraph.Col)
		vc := vecCase{Name: c.Vals["name"].(string), Op: c.Vals["op"].(string)}
		vc.Template, _ = c.Vals["template"].(string)
		vc.Strict, _ = c.Vals["strict"].(bool)
		if m, ok := c.Vals["values"].(colgraph.Col); ok {
			vc.Values = m.Vals
		}
		if m, ok := c.Vals["lead_data"].(colgraph.Col); ok {
			for k, v := range m.Vals {
				m.Vals[k] = fixVal(v)
			}
			vc.LeadData = m.Vals
		}
		if l, ok := c.Vals["columns"].([]any); ok {
			for _, e := range l {
				vc.Columns = append(vc.Columns, e.(colgraph.Col))
			}
		}
		vc.Value = fixVal(c.Vals["value"])
		vc.Input = c.Vals["input"]
		if e, ok := c.Vals["error"].(string); ok {
			vc.Error, vc.HasError = e, true
		}
		out = append(out, vc)
	}
	return out
}

func strMap(m map[string]any) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = CellText(v)
	}
	return out
}

func TestRowTmplVectors(t *testing.T) {
	cases := loadCases(t)
	if len(cases) < 60 {
		t.Fatalf("only %d vectors", len(cases))
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			switch c.Op {
			case "get_lead_values":
				got := LeadValues(c.LeadData, c.Columns)
				want := strMap(c.Value.(colgraph.Col).Vals)
				if len(got) != len(want) {
					t.Fatalf("got %v want %v", got, want)
				}
				for k, w := range want {
					if g, ok := got[k]; !ok || g != w {
						t.Fatalf("key %q: got %q want %q", k, g, w)
					}
				}
			case "resolve_prompt":
				got, err := ResolvePrompt(c.Template, strMap(c.Values), c.Strict)
				check(t, c, got, err)
			case "resolve":
				got, err := Resolve(c.Template, c.LeadData, c.Columns)
				check(t, c, got, err)
			case "resolve_deep":
				got, err := ResolveDeep(c.Input, c.LeadData, c.Columns)
				if c.HasError {
					if err == nil || err.Error() != c.Error {
						t.Fatalf("err %v want %q", err, c.Error)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				gb, _ := MarshalOrdered(got)
				wb, _ := MarshalOrdered(c.Value)
				if string(gb) != string(wb) {
					t.Fatalf("got %s want %s", gb, wb)
				}
			default:
				t.Fatalf("unknown op %s", c.Op)
			}
		})
	}
}

func check(t *testing.T, c vecCase, got string, err error) {
	t.Helper()
	if c.HasError {
		if err == nil || err.Error() != c.Error {
			t.Fatalf("err %v want %q", err, c.Error)
		}
		return
	}
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	if got != c.Value.(string) {
		t.Fatalf("got %q want %q", got, c.Value)
	}
}

func cols(t *testing.T, raw string) []colgraph.Col {
	t.Helper()
	c, err := colgraph.ParseCols([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestAmbiguousReference(t *testing.T) {
	cs := cols(t, `[{"id":"a","name":"Email"},{"id":"b","name":"email"}]`)
	_, err := Resolve("{EMAIL}", map[string]any{"a": "1", "b": "2"}, cs)
	if err == nil || err.Error() != "Ambiguous column reference: EMAIL. Use an exact column ID." {
		t.Fatalf("got %v", err)
	}
	if _, err := ResolvePrompt("{x}", map[string]string{"X": "1", "x_": "2", "Y y": "3", "y_y": "4"}, false); err != nil {
		t.Fatal(err)
	}
	_, err = ResolvePrompt("{a_b}", map[string]string{"a b": "3", "A B": "4"}, false)
	if err == nil || !strings.HasPrefix(err.Error(), "Ambiguous column reference: a_b.") {
		t.Fatalf("got %v", err)
	}
}

func TestStrictUnknown(t *testing.T) {
	_, err := ResolvePrompt("{nope}", map[string]string{}, true)
	if err == nil || err.Error() != "Unknown column reference: nope. Fix the template before sending." {
		t.Fatalf("got %v", err)
	}
	got, err := ResolvePrompt("{ nope }", nil, false)
	if err != nil || got != "[nope: not found]" {
		t.Fatalf("got %q %v", got, err)
	}
}

func TestUnicodeCaseFolding(t *testing.T) {
	// Python: "İ".lower() == "i̇" (i + U+0307); "ß".lower() == "ß"; final sigma.
	v := map[string]string{"i̇stanbul": "dotted", "straße": "sharp", "ΟΔΟΣ": "x"}
	for tmpl, want := range map[string]string{
		"{İstanbul}": "dotted", "{STRASSE}": "[STRASSE: not found]", "{STRAßE}": "sharp",
	} {
		got, err := ResolvePrompt(tmpl, v, false)
		if err != nil || got != want {
			t.Fatalf("%s: got %q %v want %q", tmpl, got, err, want)
		}
	}
	// final sigma: "ΟΔΟΣ".lower() == "οδος"
	if got, _ := ResolvePrompt("{οδος}", map[string]string{"ΟΔΟΣ": "x"}, true); got != "x" {
		t.Fatalf("final sigma: %q", got)
	}
}

func TestResolveDeepPreservesKeyOrder(t *testing.T) {
	in, err := DecodeOrdered([]byte(`{"z":"{company}","a":[{"y":"{email}","b":1}],"m":null}`))
	if err != nil {
		t.Fatal(err)
	}
	cs := cols(t, `[{"id":"company"},{"id":"email"}]`)
	got, err := ResolveDeep(in, map[string]any{"company": "Acme", "email": "a@x"}, cs)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := MarshalOrdered(got)
	if string(b) != `{"z":"Acme","a":[{"y":"a@x","b":1}],"m":null}` {
		t.Fatalf("got %s", b)
	}
}

func TestPyStrOfNonStrings(t *testing.T) {
	num := func(s string) any { return json.Number(s) }
	cases := []struct {
		in   any
		want string
	}{
		{nil, "None"}, {true, "True"}, {false, "False"}, {num("0"), "0"}, {num("-0"), "0"},
		{num("1.0"), "1.0"}, {num("1e21"), "1e+21"}, {num("0.1"), "0.1"}, {num("-0.0"), "-0.0"},
		{num("100000000000000000000"), "100000000000000000000"}, {1.5, "1.5"},
		{[]any{num("1"), "x", nil, true}, "[1, 'x', None, True]"},
		{[]any{"it's"}, `["it's"]`}, {[]any{"a\nb\\"}, `['a\nb\\']`}, {[]any{"\x01"}, `['\x01']`},
		{map[string]any{"k": num("1")}, "{'k': 1}"},
		{nan(), "nan"}, {inf(), "inf"},
	}
	for _, c := range cases {
		if got := PyStr(c.in); got != c.want {
			t.Errorf("PyStr(%#v)=%q want %q", c.in, got, c.want)
		}
	}
	if CellText(nil) != "" || CellText(false) != "False" || CellText(num("0")) != "0" {
		t.Fatal("CellText")
	}
}

func TestTemplateBoundsEnforced(t *testing.T) {
	if _, err := ResolvePrompt(strings.Repeat("a", MaxTemplateBytes+1), nil, false); err != ErrTooLarge {
		t.Fatalf("size: %v", err)
	}
	if _, err := ResolvePrompt(strings.Repeat("a", MaxTemplateBytes), nil, false); err != nil {
		t.Fatal(err)
	}
	ok := strings.Repeat("{k}", MaxPlaceholders)
	if _, err := ResolvePrompt(ok, map[string]string{"k": "v"}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolvePrompt(ok+"{k}", map[string]string{"k": "v"}, true); err != ErrTooLarge {
		t.Fatalf("placeholders: %v", err)
	}
	// 256 placeholders x 8 KiB value = 2 MiB > 1 MiB output cap.
	big := strings.Repeat("x", 8<<10)
	if _, err := ResolvePrompt(ok, map[string]string{"k": big}, true); err != ErrTooLarge {
		t.Fatalf("output: %v", err)
	}
	if _, err := Resolve(ok+"{k}", map[string]any{"k": "v"}, nil); err != ErrTooLarge {
		t.Fatalf("resolve: %v", err)
	}
}
