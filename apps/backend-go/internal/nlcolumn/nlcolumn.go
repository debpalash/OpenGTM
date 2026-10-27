// Package nlcolumn is the Go port of workbook/nl_column.py: the "AI formula generator". It turns a natural
// language instruction into a validated, ready-to-add column config, picking the cheapest sufficient kind
// (formula, ai_formula, http). Every candidate is validated before it is returned: formulas must evaluate on a
// dummy row (one repair retry with the error fed back, then a fall back to an ai_formula column), the other
// kinds must pass the same ColumnConfig schema the add-column route uses (wbcrud.ParseAddBody).
//
// The LLM is an interface; llm.Client satisfies it. Prompts are recorded from Python (testdata/nl_column.json).
package nlcolumn

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colcompute"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// LLM is the slice of internal/llm.Client the generator uses. The answer is an order-preserving object: key order
// of http_headers and http_body must survive as the model wrote it (llm.Client returns an unordered map; the api
// package adapts it, see api/llmorder.go).
type LLM interface {
	ExtractJSON(ctx context.Context, prompt, system string, maxTokens int) (*wbcrud.Obj, error)
}

// Error is NLColumnError: generation failed in a way that cannot be repaired (bad or empty LLM output). The
// route answers 502 "Column generation failed: <msg>".
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

// Result is {"column", "kind", "explanation"}.
type Result struct {
	Column      wbcrud.Column
	Kind        string
	Explanation string
	// Dropped is set when the model's http column failed validation: Column is then the zero value and Warnings says
	// why (without echoing the model's text). The route returns column null plus the warnings.
	Dropped  bool
	Warnings []string
}

var kinds = map[string]bool{"formula": true, "ai_formula": true, "http": true}

var httpMethods = map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true}

// The DSL reference is derived from the evaluator's whitelists in Python. colcompute does not export them; the
// lists are pinned by the recorded vectors (TestDSLReference) and by TestWhitelistsAccepted.
var (
	formulaFuncs      = []string{"concat", "default", "float", "int", "len", "lower", "replace", "round", "str", "strip", "title", "trim", "upper"}
	formulaStrMethods = []string{"endswith", "find", "join", "lower", "lstrip", "replace", "rstrip", "split", "startswith", "strip", "title", "upper", "zfill"}
)

// DSLReference is _DSL_REFERENCE.
func DSLReference() string {
	return "FORMULA DSL (safe expression evaluator — NOT Python, NOT JavaScript):\n" +
		"- Reference other columns as {Column Name} (curly braces, exact or case-insensitive name).\n" +
		"- Allowed bare functions: " + strings.Join(formulaFuncs, ", ") + ".\n" +
		"- Allowed string methods on a value: ." + strings.Join(formulaStrMethods, ", .") + ".\n" +
		"- Operators: + - * / // %  |  comparisons == != < <= > >=  |  and / or  |  ternary \"x if cond else y\" | indexing/slicing [0], [0:4].\n" +
		"- Literals: strings, numbers, lists. NOTHING else: no imports, no attribute access, no regex, no other functions or methods.\n" +
		"- Missing columns resolve to \"\" and out-of-range indexes return \"\" (safe)."
}

// System is _SYSTEM.
const System = "You translate a user's natural-language request into a spreadsheet column config " +
	"for a lead-enrichment workbook. Pick the CHEAPEST kind that can do the job:\n" +
	"1. \"formula\" — deterministic string/number transforms of existing columns (free, instant).\n" +
	"2. \"ai_formula\" — requires semantic judgment, classification, summarization or writing (costs an LLM call per row).\n" +
	"3. \"http\" — requires calling an external HTTP API (give a config skeleton the user finishes).\n" +
	"Never pick ai_formula for something a formula can do. Respond with JSON only."

const fewShots = `Examples:
Instruction: extract the domain from the email
{"kind": "formula", "name": "Email Domain", "formula": "{Email}.split(\"@\")[1]", "explanation": "Splits the email on @ and keeps the domain part. Free — no LLM or API calls."}

Instruction: full name in caps
{"kind": "formula", "name": "Full Name", "formula": "upper(default({First Name}, \"\")) + \" \" + upper({Last Name})", "explanation": "Uppercases and joins the name columns."}

Instruction: classify each company as B2B or B2C
{"kind": "ai_formula", "name": "B2B or B2C", "prompt": "Classify {Company} ({Description}) as exactly one of: B2B, B2C, Both. Reply with only the label.", "explanation": "Semantic judgment about the business model — needs an LLM per row."}

Instruction: get the company logo from clearbit
{"kind": "http", "name": "Logo URL", "http_url": "https://logo.clearbit.com/{Website}", "http_method": "GET", "explanation": "Calls Clearbit's logo API per row using the Website column. Add auth headers if your plan needs them."}`

// pyFalsy reports whether Python's `not v` holds for a JSON-decoded value.
func pyFalsy(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case bool:
		return !x
	case string:
		return x == ""
	case float64:
		return x == 0
	case json.Number:
		f, _ := x.Float64()
		return f == 0
	case []any:
		return len(x) == 0
	case *wbcrud.Obj:
		return x.Len() == 0
	}
	return false
}

// pyStr is str(v) for the shapes a JSON object holds (containers use compact JSON, Python uses repr).
func pyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case string:
		return x
	case float64:
		if x == float64(int64(x)) && x > -1e15 && x < 1e15 {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	case json.Number:
		return string(x)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// strOr is str(data.get(key) or "").
func strOr(m *wbcrud.Obj, key string) string {
	v, _ := m.Get(key)
	if pyFalsy(v) {
		return ""
	}
	return pyStr(v)
}

// pyIsSpace is str.isspace for one rune (Unicode whitespace plus the ASCII separators 0x1c-0x1f).
func pyIsSpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

func pyStrip(s string) string { return strings.TrimFunc(s, pyIsSpace) }

// pyTitle is str.title.
func pyTitle(s string) string {
	var b strings.Builder
	prevCased := false
	for _, r := range s {
		cased := unicode.IsUpper(r) || unicode.IsLower(r) || unicode.IsTitle(r)
		switch {
		case cased && prevCased:
			b.WriteRune(unicode.ToLower(r))
		case cased:
			b.WriteRune(unicode.ToTitle(r))
		default:
			b.WriteRune(r)
		}
		prevCased = cased
	}
	return b.String()
}

// pyLower is str.lower; the one multi-rune lowering Go lacks is U+0130.
func pyLower(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "İ", "i̇"))
}

func colStr(c wbcrud.Column, key string) string {
	v := c.Get(key)
	if pyFalsy(v) {
		return ""
	}
	return pyStr(v)
}

func columnsSummary(cols []wbcrud.Column) string {
	if len(cols) == 0 {
		return "(no columns yet)"
	}
	if len(cols) > 60 {
		cols = cols[:60]
	}
	lines := make([]string, 0, len(cols))
	for _, c := range cols {
		name := colStr(c, "name")
		if name == "" {
			name = colStr(c, "id")
		}
		if name == "" {
			name = "?"
		}
		typ := "lead_field"
		if v, ok := c.Lookup("type"); ok {
			typ = pyStr(v)
		}
		lines = append(lines, "- {"+name+"} (type: "+typ+")")
	}
	return strings.Join(lines, "\n")
}

// BuildPrompt is _build_prompt.
func BuildPrompt(instruction string, cols []wbcrud.Column, errorFeedback string) string {
	parts := []string{
		"Convert this instruction into ONE workbook column config.",
		"\nInstruction: " + instruction,
		"\nExisting columns in the workbook (reference them as {Name}):",
		columnsSummary(cols),
		"",
		DSLReference(),
		"",
		fewShots,
		"",
		"Return a single JSON object with keys:",
		`  kind: "formula" | "ai_formula" | "http"`,
		"  name: short column title",
		"  formula: (kind=formula only) DSL expression",
		"  prompt: (kind=ai_formula only) per-row LLM prompt using {Column} placeholders",
		"  http_url, http_method, http_headers, http_body, http_extract: (kind=http only; http_extract is a JSONPath like $.data.email)",
		"  explanation: one or two sentences for the user about what the column does and why this kind",
	}
	if errorFeedback != "" {
		parts = append(parts,
			"\nYour previous formula FAILED validation with this error:\n"+
				"  "+errorFeedback+"\n"+
				"Fix it using ONLY the DSL above, or switch kind to ai_formula if a formula cannot express it.")
	}
	return strings.Join(parts, "\n")
}

func dummyValue(name string) string {
	n := pyLower(name)
	has := func(ks ...string) bool {
		for _, k := range ks {
			if strings.Contains(n, k) {
				return true
			}
		}
		return false
	}
	switch {
	case has("email"):
		return "jane.doe@acme-widgets.com"
	case has("website", "url", "domain", "linkedin", "link"):
		return "https://www.acme-widgets.com/about"
	case has("phone"):
		return "+1 555 010 4477"
	case has("size", "score", "count", "employee", "revenue", "number", "year"):
		return "42"
	case has("name", "person", "contact", "founder"):
		return "Jane Doe"
	}
	return "Acme Widgets Inc"
}

var refRE = regexp.MustCompile(`\{([^}]+)\}`)

// dummyRow is _dummy_row: plausible values for every known column and every {ref} in the formula.
func dummyRow(cols []wbcrud.Column, formula string) map[string]any {
	row := map[string]any{}
	for _, c := range cols {
		for _, k := range []string{"name", "id"} {
			if key := colStr(c, k); key != "" {
				row[key] = dummyValue(key)
			}
		}
	}
	for _, m := range refRE.FindAllStringSubmatch(formula, -1) {
		key := pyStrip(m[1])
		if _, ok := row[key]; !ok {
			row[key] = dummyValue(m[1])
		}
	}
	return row
}

// validateFormula parses and evaluates on a dummy row. "" means OK, else the error text (cut at 300 characters).
func validateFormula(formula string, cols []wbcrud.Column) string {
	if pyStrip(formula) == "" {
		return "empty formula"
	}
	if _, err := colcompute.EvalFormula(formula, dummyRow(cols, formula)); err != nil {
		r := []rune(err.Error())
		if len(r) > 300 {
			r = r[:300]
		}
		return string(r)
	}
	return ""
}

var slugRE = regexp.MustCompile(`[^a-z0-9]+`)

func makeID(name string, cols []wbcrud.Column) string {
	if name == "" {
		name = "generated_column"
	}
	base := strings.Trim(slugRE.ReplaceAllString(pyLower(name), "_"), "_")
	if base == "" {
		base = "generated_column"
	}
	existing := map[string]bool{}
	for _, c := range cols {
		existing[pyStr(c.Get("id"))] = true
	}
	id, n := base, 2
	for existing[id] {
		id, n = base+"_"+strconv.Itoa(n), n+1
	}
	return id
}

// finalize runs the candidate through the schema of the add-column path (ColumnConfig, exclude_none).
func finalize(col *wbcrud.Obj) (wbcrud.Column, error) {
	raw, err := json.Marshal(map[string]any{"column": col})
	if err != nil {
		return wbcrud.Column{}, err
	}
	c, errs := wbcrud.ParseAddBody(raw)
	if len(errs) > 0 {
		return wbcrud.Column{}, fmt.Errorf("nlcolumn: column failed ColumnConfig validation: %s", errs[0].Msg)
	}
	return c, nil
}

func aiFormulaFallback(instruction string, cols []wbcrud.Column, name string) *wbcrud.Obj {
	var refs []string
	for _, c := range cols {
		r := colStr(c, "name")
		if r == "" {
			r = colStr(c, "id")
		}
		if r != "" {
			refs = append(refs, r)
		}
	}
	if len(refs) > 8 {
		refs = refs[:8]
	}
	lines := make([]string, len(refs))
	for i, r := range refs {
		lines[i] = r + ": {" + r + "}"
	}
	prompt := instruction
	if len(lines) > 0 {
		prompt = instruction + "\n\nRow data:\n" + strings.Join(lines, "\n")
	}
	o := wbcrud.NewObj()
	o.Set("id", makeID(name, cols))
	o.Set("name", name)
	o.Set("type", "ai_formula")
	o.Set("width", 300)
	o.Set("prompt", prompt)
	return o
}

func runeHead(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		r = r[:n]
	}
	return string(r)
}

// buildCandidate turns one LLM answer into (column, kind, explanation, formulaErr). formulaErr is set (and column
// is zero) only when kind is formula and the expression failed the evaluator; the caller retries or falls back.
// Any other invalid answer returns *Error.
func buildCandidate(data *wbcrud.Obj, instruction string, cols []wbcrud.Column) (col wbcrud.Column, kind, explanation, formulaErr string, err error) {
	if data.Len() == 0 {
		return col, "", "", "", &Error{"empty LLM response"}
	}
	kind = pyStrip(strOr(data, "kind"))
	if !kinds[kind] {
		return col, "", "", "", &Error{"unknown kind: " + pyRepr(kind)}
	}
	name := pyStrip(strOr(data, "name"))
	if name == "" {
		name = pyTitle(pyStrip(runeHead(instruction, 40)))
	}
	if name == "" {
		name = "Generated Column"
	}
	explanation = pyStrip(strOr(data, "explanation"))
	width := 200
	if kind == "ai_formula" {
		width = 300
	}
	c := wbcrud.NewObj()
	c.Set("id", makeID(name, cols))
	c.Set("name", name)
	c.Set("type", kind)
	c.Set("width", width)

	switch kind {
	case "formula":
		formula := pyStrip(strOr(data, "formula"))
		if e := validateFormula(formula, cols); e != "" {
			return col, kind, explanation, e, nil
		}
		c.Set("formula", formula)
	case "ai_formula":
		prompt := pyStrip(strOr(data, "prompt"))
		if prompt == "" {
			return col, "", "", "", &Error{"ai_formula answer missing prompt"}
		}
		c.Set("prompt", prompt)
	default: // http
		url := pyStrip(strOr(data, "http_url"))
		if url == "" {
			return col, "", "", "", &Error{"http answer missing http_url"}
		}
		method := strings.ToUpper(pyStrip(strOr(data, "http_method")))
		if method == "" {
			method = "GET"
		}
		if !httpMethods[method] {
			method = "GET"
		}
		c.Set("http_url", url)
		c.Set("http_method", method)
		if hv, _ := data.Get("http_headers"); hv != nil {
			if h, ok := hv.(*wbcrud.Obj); ok && h.Len() > 0 {
				ho := wbcrud.NewObj()
				for _, k := range h.Keys() {
					v, _ := h.Get(k)
					ho.Set(k, pyStr(v))
				}
				c.Set("http_headers", ho)
			}
		}
		if b, present := data.Get("http_body"); present && b != nil && method != "GET" {
			c.Set("http_body", b)
		}
		if ex := pyStrip(strOr(data, "http_extract")); ex != "" {
			c.Set("http_extract", ex)
		}
	}
	col, ferr := finalize(c)
	if ferr != nil {
		return col, "", "", "", ferr
	}
	return col, kind, explanation, "", nil
}

// pyRepr is repr(str) for the unknown-kind message (single quotes unless the text holds one and no double quote).
func pyRepr(s string) string {
	q := "'"
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		q = `"`
	}
	var b strings.Builder
	b.WriteString(q)
	for _, r := range s {
		switch {
		case string(r) == q || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteString(q)
	return b.String()
}

// Generate is generate_column: instruction -> {column, kind, explanation}. Formula candidates are validated
// against the evaluator, with one repair retry (the error fed back) and then a fall back to an ai_formula column.
// An LLM failure is the same as an empty answer (Python's client returned {}), so it yields *Error.
func Generate(ctx context.Context, l LLM, instruction string, cols []wbcrud.Column) (Result, error) {
	instruction = pyStrip(instruction)
	if instruction == "" {
		return Result{}, &Error{"empty instruction"}
	}
	ask := func(feedback string) (*wbcrud.Obj, error) {
		data, err := l.ExtractJSON(ctx, BuildPrompt(instruction, cols, feedback), System, 1024)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return wbcrud.NewObj(), nil
		}
		if data == nil {
			data = wbcrud.NewObj()
		}
		return data, nil
	}
	data, err := ask("")
	if err != nil {
		return Result{}, err
	}
	column, kind, explanation, ferr, err := buildCandidate(data, instruction, cols)
	if err != nil {
		return Result{}, err
	}
	if ferr != "" {
		retry, err := ask(ferr)
		if err != nil {
			return Result{}, err
		}
		var cerr error
		column, kind, explanation, ferr, cerr = buildCandidate(retry, instruction, cols)
		var nlErr *Error
		if cerr != nil {
			if !asError(cerr, &nlErr) {
				return Result{}, cerr
			}
			column, ferr = wbcrud.Column{}, "retry produced an invalid answer"
		}
		if ferr != "" {
			src := retry
			if src.Len() == 0 {
				src = data
			}
			name := pyStrip(strOr(src, "name"))
			if name == "" {
				name = pyTitle(pyStrip(runeHead(instruction, 40)))
			}
			column, err = finalize(aiFormulaFallback(instruction, cols, name))
			if err != nil {
				return Result{}, err
			}
			kind = "ai_formula"
			explanation = "A safe formula for this couldn't be validated, so this column uses an AI prompt per row instead."
		}
	}
	if explanation == "" {
		explanation = "Adds a " + kind + " column for: " + instruction
	}
	if kind == "http" {
		if why := httpColumnProblem(column); why != "" {
			return Result{Kind: kind, Explanation: explanation, Dropped: true,
				Warnings: []string{"The generated HTTP column was discarded because " + why + ". Describe the request again or configure the HTTP column by hand."}}, nil
		}
	}
	return Result{Column: column, Kind: kind, Explanation: explanation}, nil
}

func asError(err error, target **Error) bool {
	e, ok := err.(*Error)
	if ok {
		*target = e
	}
	return ok
}
