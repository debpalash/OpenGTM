package safecond

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	// Vectors were generated with TZ=UTC; naive timestamps read in local time.
	localZone = time.UTC
	os.Exit(m.Run())
}

type vecFile[T any] struct {
	Now     float64 `json:"now"`
	Vectors []T     `json:"vectors"`
}

type evalVec struct {
	Condition *string        `json:"condition"`
	Cells     map[string]any `json:"cells"`
	Keys      []string       `json:"-"` // cell keys in JSON (Python dict) order
	Columns   []Column       `json:"columns"`
	Passed    bool           `json:"passed"`
	Error     *string        `json:"error"`
}

func (v *evalVec) UnmarshalJSON(b []byte) error {
	type plain evalVec
	var p plain
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.UseNumber()
	if err := d.Decode(&p); err != nil {
		return err
	}
	var raw struct {
		Cells json.RawMessage `json:"cells"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	kd := json.NewDecoder(strings.NewReader(string(raw.Cells)))
	kd.UseNumber()
	if _, err := kd.Token(); err != nil {
		return err
	}
	for kd.More() {
		k, err := kd.Token()
		if err != nil {
			return err
		}
		p.Keys = append(p.Keys, k.(string))
		var skip json.RawMessage
		if err := kd.Decode(&skip); err != nil {
			return err
		}
	}
	*v = evalVec(p)
	return nil
}

type validateVec struct {
	Condition *string `json:"condition"`
	OK        bool    `json:"ok"`
	Error     *string `json:"error"`
}

func loadVec[T any](t testing.TB, name string) vecFile[T] {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var f struct {
		Now     float64 `json:"now"`
		Vectors []json.RawMessage
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	out := vecFile[T]{Now: f.Now}
	for _, r := range f.Vectors {
		var v T
		d := json.NewDecoder(strings.NewReader(string(r)))
		d.UseNumber()
		if err := d.Decode(&v); err != nil {
			t.Fatal(err)
		}
		out.Vectors = append(out.Vectors, v)
	}
	return out
}

func cond(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func at(sec float64) time.Time {
	return time.Unix(0, int64(sec*1e9))
}

var stdCells = map[string]any{"email": "ada@example.com", "score": "85", "company": "Hot Pursuit Labs", "created": "2026-08-01T00:00:00Z", "notes": "", "tier": "A"}
var stdCols = []Column{{ID: "email", Name: "Email"}, {ID: "score", Name: "Score"}, {ID: "company", Name: "Company Name"}, {ID: "created", Name: "Created"}, {ID: "notes", Name: "Notes"}, {ID: "tier", Name: "Tier"}}

func eval(c string) Result { return EvaluateAt(c, stdCells, stdCols, at(1788000000)) }

func TestConditionVectors(t *testing.T) {
	f := loadVec[evalVec](t, "eval_vectors.json")
	if len(f.Vectors) < 200 {
		t.Fatalf("only %d vectors", len(f.Vectors))
	}
	for i, v := range f.Vectors {
		got := EvaluateOrdered(cond(v.Condition), v.Keys, v.Cells, v.Columns, at(f.Now))
		wantReason := ""
		if v.Error != nil {
			wantReason = *v.Error
		}
		if got.Passed != v.Passed || got.Reason != wantReason {
			t.Errorf("vector %d %q: got {%v %q} want {%v %q}", i, cond(v.Condition), got.Passed, got.Reason, v.Passed, wantReason)
		}
	}
}

func TestValidateReasonsMatchVector(t *testing.T) {
	f := loadVec[validateVec](t, "validate_vectors.json")
	for i, v := range f.Vectors {
		err := Validate(cond(v.Condition))
		if v.OK != (err == nil) {
			t.Errorf("vector %d %q: err=%v want ok=%v", i, cond(v.Condition), err, v.OK)
			continue
		}
		if err != nil {
			var ce *ConditionError
			if !asCondErr(err, &ce) {
				t.Errorf("vector %d: error type %T", i, err)
			} else if v.Error == nil || ce.Error() != *v.Error {
				t.Errorf("vector %d %q: reason %q want %v", i, cond(v.Condition), ce.Error(), v.Error)
			}
		}
	}
}

func TestInjectionValuesNeverChangeParseTree(t *testing.T) {
	evil := []string{`" AND "1"=="1`, `x" OR "1"=="1`, ` OR 1==1`, ` AND 1==1`, `{score}`, `'`, `1 == 1`, `) OR (`, `x OR y`, "x\" == \"x\" OR \"a"}
	conds := []string{`{email} == "ok"`, `{email} == "ok" AND {score} > 1`, `{email} contains "ok"`, `not_empty({email}) AND {email} == "zz"`}
	for _, e := range evil {
		cells := map[string]any{"email": e, "score": "0"}
		for _, c := range conds {
			if r := Evaluate(c, cells, nil); r.Passed {
				t.Errorf("value %q changed outcome of %q", e, c)
			}
		}
	}
	// Tokenizing never looks at values: the structure is identical whatever the cell holds.
	a, errA := tokenize(`{a} == "x" AND {b} contains "y"`)
	if errA != nil {
		t.Fatal(errA)
	}
	if len(a) != 7 {
		t.Fatalf("tokens=%d", len(a))
	}
}

func TestFailClosedOnParseError(t *testing.T) {
	for _, c := range []string{`{a`, `"abc`, `{score} ==`, `== 3`, `{score} = 3`, `not_empty(`, `{score} older_than`, `AND`, `{nope} == 1`, `{score} > 1 AND`, `(`} {
		r := eval(c)
		if r.Passed || r.Reason == "" {
			t.Errorf("%q: got %+v, want fail closed with a reason", c, r)
		}
	}
}

func TestEmptyConditionPasses(t *testing.T) {
	for _, c := range []string{"", "   ", "\t\n", "true", "ALWAYS", "1", "Yes"} {
		if r := eval(c); !r.Passed || r.Reason != "" {
			t.Errorf("%q: %+v", c, r)
		}
	}
	for _, c := range []string{"false", "never", "0", "NO"} {
		if r := eval(c); r.Passed {
			t.Errorf("%q passed", c)
		}
	}
}

func TestLazyShortCircuitLikePython(t *testing.T) {
	// OR/AND short-circuit, so a malformed later clause is never reached.
	if r := eval(`{score} > 1 OR {nope} == 1`); !r.Passed {
		t.Errorf("OR: %+v", r)
	}
	if r := eval(`{score} < 1 AND {nope} == 1`); r.Passed || r.Reason != "" {
		t.Errorf("AND: %+v", r)
	}
	if r := eval(`{score} < 1 OR {nope} == 1`); r.Passed || r.Reason != "unresolved field '{nope}'" {
		t.Errorf("OR miss: %+v", r)
	}
}

func TestOlderThanDurations(t *testing.T) {
	now := at(1788000000)
	cells := map[string]any{"t": "1787999000"} // 1000 s old
	for c, want := range map[string]bool{
		`{t} older_than 999s`: true, `{t} older_than 1000s`: false, `{t} older_than 16m`: true,
		`{t} older_than 17m`: false, `{t} older_than 0d`: true, `{t} older_than 1W`: false,
		`{t} older_than "999S"`: true, `{t} older_than 99999999999999999999999999d`: false,
	} {
		r := EvaluateAt(c, cells, nil, now)
		if r.Passed != want || r.Reason != "" {
			t.Errorf("%q: %+v want %v", c, r, want)
		}
	}
	for c, why := range map[string]string{
		`{t} older_than 30`:   "bad duration '30' (use e.g. 30d, 12h)",
		`{t} older_than 1.5h`: "bad duration '1.5h' (use e.g. 30d, 12h)",
		`{t} older_than 5ſ`:   "evaluator error: 'ſ'",
		`{t} older_than {t}`:  "older_than duration must be like '30d'",
		`{t} older_than 30 d`: "malformed 'older_than' expression",
	} {
		if r := EvaluateAt(c, cells, nil, now); r.Passed || r.Reason != why {
			t.Errorf("%q: %+v want %q", c, r, why)
		}
	}
}

func TestOlderThanTimestampFormats(t *testing.T) {
	now := at(1788000000) // 2026-08-29T10:40:00Z
	for v, wantOld := range map[string]bool{
		"1788000000": false, "1787990000": true, "1787999999999": false, "1787900000000": true, "1e9": true,
		"2026-01-01": true, "2026-01-01T00:00:00Z": true, "2026-08-29T10:39:58Z": true, "2026-08-29T10:39:59.5Z": false, "2026-08-30T10:39:59Z": false,
		"2026-08-29T10:00:00+05:30": true, "20260801": true, "2026-W35-1": true, "2026-08-01T00:00:00.123456Z": true,
		"2099-01-01": false, "-5": true,
	} {
		r := EvaluateAt(`{v} older_than 1s`, map[string]any{"v": v}, nil, now)
		if r.Reason != "" || r.Passed != wantOld {
			t.Errorf("%q: %+v want old=%v", v, r, wantOld)
		}
	}
	for _, v := range []string{"yesterday", "2026-13-45", "", "None", "2026-W54-1", "0000-01-01", "2026-08-01T25:00:00"} {
		r := EvaluateAt(`{v} older_than 1s`, map[string]any{"v": v}, nil, now)
		if r.Passed || r.Reason != "older_than left operand is not a timestamp" {
			t.Errorf("%q: %+v", v, r)
		}
	}
}

func TestContainsCaseRules(t *testing.T) {
	cells := map[string]any{"c": "Hot Pursuit LABS", "d": "İstanbul"}
	for c, want := range map[string]bool{
		`{c} contains "labs"`: true, `{c} contains "HOT p"`: true, `{c} contains ""`: true,
		`{c} contains "x"`: false, `"abc" CONTAINS "B"`: true, `{d} contains "i̇stanbul"`: true,
	} {
		if r := Evaluate(c, cells, nil); r.Passed != want || r.Reason != "" {
			t.Errorf("%q: %+v want %v", c, r, want)
		}
	}
}

func TestNumericVsStringCompare(t *testing.T) {
	cells := map[string]any{"a": "10", "b": "9", "s": "abc", "e": "", "f": 5.0, "g": json.Number("5")}
	for c, want := range map[string]bool{
		`{a} > {b}`: true, `{a} > 9`: true, `"10" < "9"`: false, `{s} > "abb"`: true, `{s} < 5`: false,
		`{f} == 5`: true, `{g} == "5.0"`: true, `{e} == ""`: true, `{a} >= 10`: true, `{a} != 10.0`: false,
		`{s} == "abc"`: true, `{s} <= "ABC"`: false,
	} {
		if r := Evaluate(c, cells, nil); r.Passed != want || r.Reason != "" {
			t.Errorf("%q: %+v want %v", c, r, want)
		}
	}
}

func TestPythonFloatAcceptance(t *testing.T) {
	cells := map[string]any{"x": "1e3", "u": "1_000", "i": "inf", "n": "nan", "w": " 7 ", "bad": "1__0", "ar": "٣"}
	for c, want := range map[string]bool{
		`{x} == 1000`: true, `{u} == 1000`: true, `{i} > 1e308`: true, `{n} == {n}`: false, `{n} != {n}`: true,
		`{w} == 7`: true, `{bad} == "1__0"`: true, `{bad} < 5`: true, `{ar} == 3`: true, `{i} == "-inf"`: false,
	} {
		if r := Evaluate(c, cells, nil); r.Passed != want || r.Reason != "" {
			t.Errorf("%q: %+v want %v", c, r, want)
		}
	}
}

func TestTokenBudgetRejected(t *testing.T) {
	ok := strings.Repeat("1 == 1 AND ", 200) + "1 == 1" // 800+ tokens, accepted
	if r := eval(ok); !r.Passed {
		t.Errorf("large expression rejected: %+v", r)
	}
	many := strings.Repeat("1 == 1 AND ", 400) + "1 == 1" // 1603 tokens, 4.4 KB
	r := eval(many)
	if r.Passed || r.Reason != "condition too complex (more than 1024 tokens)" {
		t.Errorf("tokens: %+v", r)
	}
	if err := Validate(many); err == nil || err.Error() != "condition too complex (more than 1024 tokens)" {
		t.Errorf("validate tokens: %v", err)
	}
	big := `{score} == "` + strings.Repeat("a", 16384) + `"`
	if r := eval(big); r.Passed || r.Reason != "condition too long (more than 16384 bytes)" {
		t.Errorf("size: %+v", r)
	}
	if err := Validate(big); err == nil {
		t.Error("validate accepted oversize condition")
	}
	cells := map[string]any{"v": strings.Repeat("x", maxOperandLen+1), "w": strings.Repeat("x", maxOperandLen)}
	if r := Evaluate(`{v} contains "x"`, cells, nil); r.Passed || r.Reason != "operand too long (more than 1048576 bytes)" {
		t.Errorf("operand: %+v", r)
	}
	if r := Evaluate(`{w} contains "x"`, cells, nil); !r.Passed {
		t.Errorf("operand at limit: %+v", r)
	}
	// cumulative budget: 9 x 1 MiB operands exceed the 8 MiB per-evaluation total.
	c := strings.Repeat(`{w} contains "x" AND `, 8) + `{w} contains "x"`
	if r := Evaluate(c, cells, nil); r.Passed || r.Reason != "operands too large (more than 8388608 bytes resolved)" {
		t.Errorf("cumulative: %+v", r)
	}
	c = strings.Repeat(`{w} contains "x" AND `, 7) + `{w} contains "x"` // exactly 8 MiB
	if r := Evaluate(c, cells, nil); !r.Passed {
		t.Errorf("8 operands: %+v", r)
	}
}

func TestCaseInsensitiveLookupManyKeys(t *testing.T) {
	cells := map[string]any{}
	for i := 0; i < 2000; i++ {
		cells[fmt.Sprintf("Key%d", i)] = "1"
	}
	c := strings.Repeat("{key7} == 1 AND ", 200) + "{KEY1999} == 1"
	if r := Evaluate(c, cells, nil); !r.Passed {
		t.Errorf("%+v", r)
	}
}

func TestKeywordInsidePlaceholderIsNotKeyword(t *testing.T) {
	cells := map[string]any{"a AND b": "1", "x OR y": "2", "contains": "3", "older_than": "4"}
	for c, want := range map[string]bool{
		`{a AND b} == 1`: true, `{x OR y} == 2`: true, `{contains} == 3`: true, `{older_than} == 4`: true,
		`"a AND b" == "a AND b"`: true, `"x OR y" contains "OR"`: true, `{a AND b} == 1 AND {x OR y} == 2`: true,
	} {
		if r := Evaluate(c, cells, nil); r.Passed != want || r.Reason != "" {
			t.Errorf("%q: %+v want %v", c, r, want)
		}
	}
	if err := Validate(`{a AND b} == "x OR y"`); err != nil {
		t.Errorf("validate: %v", err)
	}
}

func FuzzEvaluateNeverPanics(f *testing.F) {
	for _, s := range []string{`{a} == 1`, `not_empty({a})`, `{a} older_than 3d OR "x" contains {a}`, `{`, `"`, `))((`, `a AND OR`, "\x00\xff", `{a} == "1" AND (`} {
		f.Add(s, "v")
	}
	f.Fuzz(func(t *testing.T, c, v string) {
		cells := map[string]any{"a": v, "b": nil, "c": map[string]any{"value": v}}
		_ = Evaluate(c, cells, []Column{{ID: "a", Name: c}})
		_ = Validate(c)
	})
}

func asCondErr(err error, target **ConditionError) bool { return errors.As(err, target) }

var _ = fmt.Sprint

// TestISOTimestampParity pins Python 3.13 datetime.fromisoformat (TZ=UTC)
// quirks: week dates, one arbitrary byte before a timezone, sub-second offsets.
func TestISOTimestampParity(t *testing.T) {
	for _, c := range []struct {
		in string
		ok bool
		ts float64
	}{
		{"2026-08-01", true, 1785542400.0},
		{"20260801", true, 1785542400.0},
		{"2026-W35-1", true, 1787529600.0},
		{"2026-W35", true, 1787529600.0},
		{"2026W351", true, 1787529600.0},
		{"2026W35123+0530", true, 1787592600.0},
		{"2026W351210", true, 1787565600.0},
		{"2026W351234", false, 0},
		{"2026-W35-11", true, 1787569200.0},
		{"10:30", false, 0},
		{"2026-08-01T10:30:15 +05:30", true, 1785560415.0},
		{"2026-08-01T10:30:15X+05:30", true, 1785560415.0},
		{"2026-08-01T10:30:15  +05:30", false, 0},
		{"2026-08-01T10:30:15.5+05:30", true, 1785560415.5},
		{"2026-08-01T10:30:15,25", true, 1785580215.25},
		{"2026-08-01T10:30:15.1234567", true, 1785580215.123456},
		{"2026-08-01T10:30:15+00:00,5", true, 1785580215.0},
		{"2026-08-01T10:30:15-05:30:30.5", true, 1785600045.5},
		{"2026-08-01T24:00", false, 0},
		{"2026-08-01T10:30:15+24:00", false, 0},
		{"2026-08-01T10:30:15z", false, 0},
		{"0001-01-01", false, 0},
		{"0001-01-01T10:00:00+00:00", true, -62135560800.0},
		{"2026-W53-1", true, 1798416000.0},
		{"2020-W53-7", true, 1609632000.0},
		{"2026-08-01T", false, 0},
		{"2026-08-01T1030", true, 1785580200.0},
		{"2026-08-01T10:3015", false, 0},
		{"2026-02-29", false, 0},
	} {
		got, ok := parseISO(strings.ReplaceAll(c.in, "Z", "+00:00"))
		if ok != c.ok || (ok && got != c.ts) {
			t.Errorf("%q: got %v/%v want %v/%v", c.in, got, ok, c.ts, c.ok)
		}
	}
}
