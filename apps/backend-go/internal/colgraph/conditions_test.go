package colgraph

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

type condFile struct {
	Columns  []json.RawMessage          `json:"columns"`
	Cells    map[string]json.RawMessage `json:"cells"`
	Evaluate []struct {
		Condition string `json:"condition"`
		Result    struct {
			Value bool `json:"value"`
		} `json:"result"`
	} `json:"evaluate"`
	Shapes []struct {
		Name      string                     `json:"name"`
		Cells     map[string]json.RawMessage `json:"cells"`
		Columns   []json.RawMessage          `json:"columns"`
		Condition string                     `json:"condition"`
		Result    struct {
			Value bool `json:"value"`
		} `json:"result"`
	} `json:"shapes"`
	Split []struct {
		Expr     string   `json:"expr"`
		Pattern  string   `json:"pattern"`
		Maxsplit int      `json:"maxsplit"`
		Parts    []string `json:"parts"`
	} `json:"split_unquoted"`
	Compare []struct {
		Left, Right, Op string
		Result          bool
	} `json:"compare"`
	Clean []struct {
		Input  string `json:"input"`
		Result string `json:"result"`
	} `json:"clean_value"`
}

func loadConds(t testing.TB) condFile {
	t.Helper()
	raw, err := os.ReadFile("testdata/conditions_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var f condFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func decodeCells(t testing.TB, m map[string]json.RawMessage) map[string]any {
	t.Helper()
	out := map[string]any{}
	for k, raw := range m {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			t.Fatal(err)
		}
		out[k] = v
	}
	return out
}

func TestConditionVectors(t *testing.T) {
	f := loadConds(t)
	cols := parseCols(t, f.Columns)
	cells := decodeCells(t, f.Cells)
	for _, e := range f.Evaluate {
		if got := EvalConditionCols(e.Condition, cells, cols); got != e.Result.Value {
			t.Errorf("%q: got %v want %v", e.Condition, got, e.Result.Value)
		}
	}
	if len(f.Evaluate) < 100 {
		t.Fatalf("only %d vectors", len(f.Evaluate))
	}
}

func TestConditionShapes(t *testing.T) {
	f := loadConds(t)
	for _, s := range f.Shapes {
		var cols []Col
		if s.Name != "no_columns_config" {
			cols = parseCols(t, s.Columns)
		}
		if got := EvalConditionCols(s.Condition, decodeCells(t, s.Cells), cols); got != s.Result.Value {
			t.Errorf("%s %q: got %v want %v", s.Name, s.Condition, got, s.Result.Value)
		}
	}
}

func TestConditionEmptyAndFalsyLiterals(t *testing.T) {
	row := map[string]any{"zero": 0, "empty": "", "na": "N/A", "none": nil, "nan": "nan"}
	cases := []struct {
		cond any
		want bool
	}{
		{nil, true}, {"", true}, {"{zero} == 0", true}, {"{zero} == \"\"", false},
		{"is_empty({empty})", true}, {"is_empty({na})", true}, {"not_empty({na})", false},
		{"is_empty({none})", true}, {"{na} == \"N/A\"", true}, {"{nan} == {nan}", false}, {"{nan} != {nan}", true},
	}
	for _, c := range cases {
		if got := EvalCondition(c.cond, row); got != c.want {
			t.Errorf("%#v: got %v want %v", c.cond, got, c.want)
		}
	}
}

func TestConditionUnknownOperatorFalseLikePython(t *testing.T) {
	for _, c := range []string{"{a} => 5", "no operator here", "{a} ~= 5", "a AND b", "AND", "{a} === 5 5"} {
		if EvalCondition(c, map[string]any{"a": 5}) {
			t.Errorf("%q should fail closed", c)
		}
	}
}

func TestSplitUnquotedVectors(t *testing.T) {
	for _, s := range loadConds(t).Split {
		got := splitUnquoted(s.Expr, literalMatcher(s.Pattern), s.Maxsplit)
		if len(got) != len(s.Parts) {
			t.Errorf("%q: got %q want %q", s.Expr, got, s.Parts)
			continue
		}
		for i := range got {
			if got[i] != s.Parts[i] {
				t.Errorf("%q: got %q want %q", s.Expr, got, s.Parts)
				break
			}
		}
	}
}

func TestCompareVectors(t *testing.T) {
	f := loadConds(t)
	if len(f.Compare) < 100 {
		t.Fatalf("only %d", len(f.Compare))
	}
	for _, c := range f.Compare {
		if got := compareValues(c.Left, c.Right, c.Op); got != c.Result {
			t.Errorf("%q %s %q: got %v want %v", c.Left, c.Op, c.Right, got, c.Result)
		}
	}
}

func TestCleanValueVectors(t *testing.T) {
	for _, c := range loadConds(t).Clean {
		if got := cleanValue(c.Input); got != c.Result {
			t.Errorf("%q: got %q want %q", c.Input, got, c.Result)
		}
	}
}

func TestPyFloatRepr(t *testing.T) {
	for in, want := range map[string]string{"75": "75", "75.0": "75.0", "75.00": "75.0", "1e16": "1e+16", "1e15": "1000000000000000.0", "1.5e-5": "1.5e-05", "0.0001": "0.0001", "-0.0": "-0.0", "0.1": "0.1", "123456789012345678": "123456789012345678", "-0": "0"} {
		if got := pyStrValue(json.Number(in)); got != want {
			t.Errorf("%s: got %s want %s", in, got, want)
		}
	}
}

func TestConditionBoundedInput(t *testing.T) {
	long := make([]byte, 200000)
	for i := range long {
		long[i] = '{'
	}
	// Must terminate quickly (no quadratic blowup).
	_ = EvalCondition(string(long)+" == 1", map[string]any{})
}

func TestDuplicateDisplayNamesFollowColumnConfigOrder(t *testing.T) {
	// Python iterates row cells in lead_data order, so the alias "Dup" ends up
	// with the last column's value. Go follows column config order first.
	cols := []Col{
		MakeCol("id", "x2", "name", "Dup"),
		MakeCol("id", "x1", "name", "Dup"),
	}
	row := map[string]any{"x1": "one", "x2": "two"}
	if !EvalConditionCols(`{Dup} == "one"`, row, cols) {
		t.Error("last column in config order should win the shared display name")
	}
	// Case-only collisions: lower-case lookup keeps the first key.
	cols = []Col{MakeCol("id", "b", "name", "Name"), MakeCol("id", "a", "name", "NAME")}
	row = map[string]any{"a": "A", "b": "B"}
	if !EvalConditionCols(`{nAmE} == "B"`, row, cols) {
		t.Error("first key in config order should win the case-insensitive match")
	}
}
