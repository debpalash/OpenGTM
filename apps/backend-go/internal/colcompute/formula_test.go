package colcompute

import (
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/rowtmpl"

	"errors"
	"sort"
	"strings"
	"testing"
	"time"
)

type formulaVectors struct {
	Evaluate []struct {
		Name      string         `json:"name"`
		Expr      string         `json:"expr"`
		RowValues map[string]any `json:"row_values"`
		Result    struct {
			Value   tagged `json:"value"`
			Error   string `json:"error"`
			Message string `json:"message"`
		} `json:"result"`
	} `json:"evaluate"`
	References []struct {
		Expr     string   `json:"expr"`
		Refs     []string `json:"refs"`
		Replaced string   `json:"replaced"`
	} `json:"references"`
	Execute []struct {
		Name          string           `json:"name"`
		Column        map[string]any   `json:"column"`
		LeadData      map[string]any   `json:"lead_data"`
		ColumnsConfig []map[string]any `json:"columns_config"`
		Result        struct {
			Success bool   `json:"success"`
			Value   tagged `json:"value"`
			Error   any    `json:"error"`
		} `json:"result"`
	} `json:"execute"`
}

// TestFormulaVectors replays every Python-recorded evaluate case (the plan asks for 60; the
// recording has 271) and demands the identical value type, value and error text.
func TestFormulaVectors(t *testing.T) {
	var v formulaVectors
	loadVectors(t, "formula_vectors.json", &v)
	if len(v.Evaluate) < 271 {
		t.Fatalf("vector file shrank: %d evaluate cases", len(v.Evaluate))
	}
	for _, c := range v.Evaluate {
		t.Run(c.Name, func(t *testing.T) {
			got, err := EvalFormula(c.Expr, c.RowValues)
			if c.Result.Error != "" {
				var fe *FormulaError
				if !errors.As(err, &fe) {
					t.Fatalf("want FormulaError %q, got value %v err %v", c.Result.Message, got, err)
				}
				if fe.Error() != c.Result.Message {
					t.Fatalf("error text\n got: %q\nwant: %q", fe.Error(), c.Result.Message)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error %v", err)
			}
			if m := matchTag(c.Result.Value, got); m != "" {
				t.Fatal(m)
			}
		})
	}
}

func TestFormulaReferences(t *testing.T) {
	var v formulaVectors
	loadVectors(t, "formula_vectors.json", &v)
	for _, c := range v.References {
		refs := colgraph.FormulaReferences(c.Expr)
		sort.Strings(refs)
		want := append([]string{}, c.Refs...)
		sort.Strings(want)
		if strings.Join(refs, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("colgraph.FormulaReferences(%q) = %q, want %q", c.Expr, refs, want)
		}
		if got := replaceFormulaReferences(c.Expr, func(raw string) string { return "<" + raw + ">" }); got != c.Replaced {
			t.Errorf("replace(%q) = %q, want %q", c.Expr, got, c.Replaced)
		}
	}
}

func TestExecuteFormulaColumnVectors(t *testing.T) {
	var v formulaVectors
	loadVectors(t, "formula_vectors.json", &v)
	for _, c := range v.Execute {
		t.Run(c.Name, func(t *testing.T) {
			cols := make([]ColumnInfo, 0, len(c.ColumnsConfig))
			for _, cc := range c.ColumnsConfig {
				id, _ := cc["id"].(string)
				name, _ := cc["name"].(string)
				cols = append(cols, ColumnInfo{ID: id, Name: name})
			}
			got := ExecuteFormulaColumn(c.Column, c.LeadData, cols)
			if got.Success != c.Result.Success {
				t.Fatalf("success = %v, want %v (error %q)", got.Success, c.Result.Success, got.Error)
			}
			wantErr, _ := c.Result.Error.(string)
			if got.Error != wantErr {
				t.Fatalf("error = %q, want %q", got.Error, wantErr)
			}
			if m := matchTag(c.Result.Value, got.Value); m != "" {
				t.Fatal(m)
			}
		})
	}
}

// Everything Python's whitelist rejects, and a set of classic sandbox escapes, must be a
// FormulaError and never execute or panic.
func TestFormulaRejectsWhatPythonRejects(t *testing.T) {
	var v formulaVectors
	loadVectors(t, "formula_vectors.json", &v)
	rejected := 0
	for _, c := range v.Evaluate {
		if c.Result.Error == "" {
			continue
		}
		rejected++
		if _, err := EvalFormula(c.Expr, c.RowValues); err == nil {
			t.Errorf("%s: %q accepted, Python rejects it with %q", c.Name, c.Expr, c.Result.Message)
		}
	}
	if rejected < 80 {
		t.Fatalf("only %d rejection vectors", rejected)
	}
	hostile := []string{
		`__import__("os").system("id")`,
		`open("/etc/passwd").read()`,
		`().__class__.__bases__[0].__subclasses__()`,
		`"".__class__.__mro__`,
		`[].__class__`,
		`{a}.__dict__`,
		`getattr({a}, "upper")()`,
		`exec("1")`, `eval("1")`, `compile("1", "x", "eval")`,
		`{a}.format()`, `"{0.__class__}".format({a})`,
		`(lambda: __import__("os"))()`,
		`[c for c in ().__class__.__bases__]`,
		`globals()`, `locals()`, `vars()`, `dir()`, `type({a})`,
		`{a}.upper.__self__`, `len.__call__({a})`,
		`1 if 1 else __import__("os")`,
		`f"{__import__('os')}"`, `{a}.encode().decode()`,
		`(x := __import__)`, `await __import__("os")`,
		`{a}[0].__class__`, `upper.__globals__`,
	}
	for _, e := range hostile {
		got, err := EvalFormula(e, map[string]any{"a": "x"})
		var fe *FormulaError
		if !errors.As(err, &fe) {
			t.Errorf("%q: want FormulaError, got %v / %v", e, got, err)
		}
	}
}

func TestFormulaNodeBudget(t *testing.T) {
	// 10,000 nodes evaluate; one more is refused up front, before anything runs.
	ok := "1" + strings.Repeat("+1", 4999) // 5000 constants + 4999 BinOp nodes = 9999 nodes (+ Expression)
	if _, err := EvalFormula(ok, nil); err != nil {
		t.Fatalf("formula within the node budget refused: %v", err)
	}
	big := "1" + strings.Repeat("+1", 5100)
	_, err := EvalFormula(big, nil)
	var fe *FormulaError
	if !errors.As(err, &fe) || !strings.Contains(fe.Error(), "too complex") {
		t.Fatalf("over-budget formula: %v", err)
	}
	// Absurd source is refused without parsing it.
	if _, err := EvalFormula(strings.Repeat("(", 1_000_000), nil); err == nil {
		t.Fatal("million-character formula accepted")
	}
	// Repetition cannot allocate unbounded memory.
	for _, e := range []string{`"abc" * 1000000000`, `[1, 2, 3] * 1000000000`, `{a} * 99999999999`} {
		start := time.Now()
		if _, err := EvalFormula(e, map[string]any{"a": "x"}); err == nil {
			t.Errorf("%q accepted", e)
		}
		if time.Since(start) > time.Second {
			t.Errorf("%q took %v", e, time.Since(start))
		}
	}
	// A spent deadline stops evaluation with an error rather than a result.
	old := evalTimeout
	evalTimeout = -time.Second
	defer func() { evalTimeout = old }()
	if _, err := EvalFormula("1 + 1", nil); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expired deadline: %v", err)
	}
}

// Node kinds whose children the parser does not rebuild must be rejected outright (fail
// closed), exactly as Python rejects them, even when a forbidden call/attribute/name hides
// inside. Python's BFS walk reports the container before anything nested in it.
func TestFormulaFailsClosedOnOpaqueNodes(t *testing.T) {
	cases := map[string]string{
		`{}`:                                  "disallowed expression element: Dict",
		`[x for x in ().__class__.__bases__]`: "disallowed expression element: ListComp",
		`(__import__("os") for _ in [1])`:     "disallowed expression element: GeneratorExp",
		`(lambda: __import__("os"))`:          "disallowed expression element: Lambda",
		`f"{__import__('os')}"`:               "disallowed expression element: JoinedStr",
		`[*__import__("os").listdir()]`:       "disallowed expression element: Starred",
		`len(*[__import__("os")])`:            "disallowed expression element: Starred",
		`(x := __import__("os"))`:             "disallowed expression element: NamedExpr",
		`await __import__("os")`:              "disallowed expression element: Await",
		`(yield __import__("os"))`:            "disallowed expression element: Yield",
		`len(a=__import__("os"))`:             "disallowed expression element: keyword",
		`1 if 1 else (lambda: 1)`:             "disallowed expression element: Lambda",
	}
	for expr, want := range cases {
		_, err := EvalFormula(expr, nil)
		if err == nil || err.Error() != want {
			t.Errorf("%q: got %v, want %q", expr, err, want)
		}
	}
	// {...} with content is a column reference, so dict/set literals with names inside are
	// plain references and never reach the parser as containers.
	for _, e := range []string{`{__import__}`, `{1, __import__}`, `{"a": eval}`} {
		if v, err := EvalFormula(e, nil); err != nil || v != "" {
			t.Errorf("%q: %v %v", e, v, err)
		}
	}
	// Guard: every kind the parser builds without children is outside the whitelist.
	for _, k := range []string{"JoinedStr", "Dict", "Set", "Starred", "ListComp", "SetComp", "DictComp", "GeneratorExp", "Lambda", "NamedExpr", "Await", "Yield", "keyword"} {
		if allowedKinds[k] {
			t.Errorf("%s must not be whitelisted", k)
		}
	}
}

func TestValidateTemplateReferencesVectors(t *testing.T) {
	var v struct {
		V []struct {
			Template string           `json:"template"`
			Columns  []map[string]any `json:"columns"`
			Result   struct {
				Value   any    `json:"value"`
				Message string `json:"message"`
			} `json:"result"`
		} `json:"validate_template_references"`
	}
	loadVectors(t, "template_vectors.json", &v)
	if len(v.V) == 0 {
		t.Fatal("no vectors")
	}
	for _, c := range v.V {
		var cols []colgraph.Col
		for _, m := range c.Columns {
			col := colgraph.MakeCol()
			for k, val := range m {
				col.Set(k, val)
			}
			cols = append(cols, col)
		}
		err := rowtmpl.ValidateTemplateReferences(c.Template, cols)
		if c.Result.Message == "" && err != nil || c.Result.Message != "" && (err == nil || err.Error() != c.Result.Message) {
			t.Errorf("%q: err %v, want %q", c.Template, err, c.Result.Message)
		}
	}
}

func TestFormulaResourceCaps(t *testing.T) {
	hex := "0x" + strings.Repeat("f", 20000) // 80k bits
	cases := []string{
		`"a" * 10**9`, `"a" * 1000000000`, `{a} * 1000000000`,
		`("a" * 1000) * 1000 * 1000`, `([1] * 1000) * 1000 * 1000`, `b"ab" * 999999999`,
		hex + "*" + hex + "*" + hex, hex + "*" + hex + "*" + hex + "*" + hex,
		`"a" * 99999999999999999999999`,
		strings.TrimSuffix(strings.Repeat(`("a" * 1000000) + `, 40), " + "), // cumulative allocation
		strings.TrimSuffix(strings.Repeat(`len("a" * 1000000) + `, 5), " + ") + ` + len(("a" * 1000000) * 100)`,
	}
	for _, e := range cases {
		start := time.Now()
		_, err := EvalFormula(e, map[string]any{"a": "x"})
		var fe *FormulaError
		if !errors.As(err, &fe) {
			t.Errorf("%.60q accepted (%v)", e, err)
		}
		if time.Since(start) > 500*time.Millisecond {
			t.Errorf("%.60q took %v", e, time.Since(start))
		}
	}
}
