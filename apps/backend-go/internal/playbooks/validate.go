package playbooks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// Request validation reproducing FastAPI/pydantic v2 (lax mode, JSON input):
// errors are reported in model field order with loc prefix "body", numbers and
// strings coerce the way pydantic coerces them, and the two model validators
// run only when every field is valid. Known gap: the "ctx.error" text and
// position of a json_invalid body are best-effort, Python's decoder messages
// are not reproduced.

// CreateInput is a validated PlaybookCreate (JSON order = model_dump order).
type CreateInput struct {
	Name           string  `json:"name"`
	Description    string  `json:"description"`
	PromptTemplate string  `json:"prompt_template"`
	OutputFormat   string  `json:"output_format"`
	MaxSteps       int     `json:"max_steps"`
	CellBudgetUSD  float64 `json:"cell_budget_usd"`
	Steps          []Step  `json:"steps"`
}

// PatchInput is a validated PlaybookPatch: nil pointers are absent fields. For
// the two nullable columns the *Set flag says the key was present and a nil
// value means explicit null (clear). An explicit null on a NOT NULL field is a
// 422 (decision D3; Python answers 500 at commit).
type PatchInput struct {
	Name                    *string
	Description             *string
	PromptTemplate          *string
	OutputFormat            *string
	MaxSteps                *int
	CellBudgetUSD           *float64
	Enabled                 *bool
	ScheduleAudienceSet     bool
	ScheduleAudienceID      *string
	ScheduleIntervalSet     bool
	ScheduleIntervalMinutes *int
	Steps                   *[]Step
}

// RunCreateInput is a validated RunCreate.
type RunCreateInput struct {
	AudienceID string
	MaxMembers int
}

// ValidOutputFormat is the router-level check `in {"text", "json"}`.
func ValidOutputFormat(s string) bool { return s == "text" || s == "json" }

const stepKeyPattern = `^[a-z][a-z0-9_]{0,39}$`

var stepKeyRE = regexp.MustCompile(stepKeyPattern)

func loc(rest ...any) []any { return append([]any{"body"}, rest...) }

func fe(typ string, l []any, msg string, in any, ctx map[string]any) httpx.FieldError {
	return httpx.FieldError{Type: typ, Loc: l, Msg: msg, Input: raw(in), Ctx: ctx}
}

func missing(l []any, parent *obj) httpx.FieldError {
	return fe("missing", l, "Field required", parent, nil)
}

// decodeBody turns raw into an object or the FastAPI body errors.
func decodeBody(body []byte) (*obj, []httpx.FieldError) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, []httpx.FieldError{missing(loc(), nil)}
	}
	doc, err := decodeDoc(body)
	if err != nil {
		pos := 0
		msg := "Expecting value"
		var se *json.SyntaxError
		if asSyntax(err, &se) {
			pos = int(se.Offset)
			if pos > 0 {
				pos--
			}
			if strings.Contains(se.Error(), "object key string") {
				msg = "Expecting property name enclosed in double quotes"
			}
		}
		return nil, []httpx.FieldError{fe("json_invalid", loc(pos), "JSON decode error", newObj(), map[string]any{"error": msg})}
	}
	if doc == nil { // JSON null is an absent body for FastAPI
		return nil, []httpx.FieldError{missing(loc(), nil)}
	}
	o, ok := doc.(*obj)
	if !ok {
		return nil, []httpx.FieldError{fe("model_attributes_type", loc(), "Input should be a valid dictionary or object to extract fields from", doc, nil)}
	}
	return o, nil
}

func asSyntax(err error, target **json.SyntaxError) bool {
	se, ok := err.(*json.SyntaxError)
	if ok {
		*target = se
	}
	return ok
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// strVal validates a string with length bounds in code points (max < 0: none).
func strVal(v any, l []any, min, max int) (string, *httpx.FieldError) {
	s, ok := v.(string)
	if !ok {
		e := fe("string_type", l, "Input should be a valid string", v, nil)
		return "", &e
	}
	n := utf8.RuneCountInString(s)
	if min > 0 && n < min {
		e := fe("string_too_short", l, "String should have at least "+plural(min, "character"), v, map[string]any{"min_length": min})
		return "", &e
	}
	if max >= 0 && n > max {
		e := fe("string_too_long", l, "String should have at most "+plural(max, "character"), v, map[string]any{"max_length": max})
		return "", &e
	}
	return s, nil
}

var (
	intStrRE   = regexp.MustCompile(`^[+-]?\d+(_\d+)*$`)
	intFloatRE = regexp.MustCompile(`^[+-]?\d+\.0*$`)
	floatStrRE = regexp.MustCompile(`^[+-]?(?:\d+(?:_\d+)*(?:\.(?:\d+(?:_\d+)*)?)?|\.\d+(?:_\d+)*)(?:[eE][+-]?\d+(?:_\d+)*)?$`)
	infNanRE   = regexp.MustCompile(`(?i)^[+-]?(inf|infinity|nan)$`)
)

func ctxInt(k string, n int) map[string]any { return map[string]any{k: n} }

// intVal validates an int field with ge/le bounds (use math.MinInt/MaxInt for
// none). Lax coercions: bool, integral float, numeric string.
func intVal(v any, l []any, ge, le int) (int, *httpx.FieldError) {
	var n *big.Int
	switch t := v.(type) {
	case json.Number:
		s := string(t)
		if !strings.ContainsAny(s, ".eE") {
			n, _ = new(big.Int).SetString(s, 10)
		} else {
			f, err := strconv.ParseFloat(s, 64)
			switch {
			case err != nil || math.IsInf(f, 0) || math.IsNaN(f) || math.Abs(f) >= 1<<63:
				e := fe("int_parsing_size", l, "Unable to parse input string as an integer, exceeded maximum size", v, nil)
				return 0, &e
			case f != math.Trunc(f):
				e := fe("int_from_float", l, "Input should be a valid integer, got a number with a fractional part", v, nil)
				return 0, &e
			}
			n = big.NewInt(int64(f))
		}
	case bool:
		n = big.NewInt(0)
		if t {
			n = big.NewInt(1)
		}
	case string:
		s := strings.TrimSpace(t)
		switch {
		case intStrRE.MatchString(s):
			n, _ = new(big.Int).SetString(strings.ReplaceAll(s, "_", ""), 10)
		case intFloatRE.MatchString(s):
			n, _ = new(big.Int).SetString(s[:strings.IndexByte(s, '.')], 10)
		}
		if n == nil {
			e := fe("int_parsing", l, "Input should be a valid integer, unable to parse string as an integer", v, nil)
			return 0, &e
		}
	default:
		e := fe("int_type", l, "Input should be a valid integer", v, nil)
		return 0, &e
	}
	if le != math.MaxInt && n.Cmp(big.NewInt(int64(le))) > 0 {
		e := fe("less_than_equal", l, fmt.Sprintf("Input should be less than or equal to %d", le), v, ctxInt("le", le))
		return 0, &e
	}
	if ge != math.MinInt && n.Cmp(big.NewInt(int64(ge))) < 0 {
		e := fe("greater_than_equal", l, fmt.Sprintf("Input should be greater than or equal to %d", ge), v, ctxInt("ge", ge))
		return 0, &e
	}
	return int(n.Int64()), nil
}

// floatVal validates a float with 0 < x <= le (gt is always 0 here).
func floatVal(v any, l []any, gt, le float64) (float64, *httpx.FieldError) {
	var f float64
	switch t := v.(type) {
	case json.Number:
		f, _ = strconv.ParseFloat(string(t), 64) // overflow -> +-Inf like Python's float()
	case bool:
		if t {
			f = 1
		}
	case string:
		s := strings.TrimSpace(t)
		switch {
		case infNanRE.MatchString(s):
			switch {
			case strings.EqualFold(strings.TrimLeft(s, "+-"), "nan"):
				f = math.NaN()
			case strings.HasPrefix(s, "-"):
				f = math.Inf(-1)
			default:
				f = math.Inf(1)
			}
		case floatStrRE.MatchString(s):
			f, _ = strconv.ParseFloat(strings.ReplaceAll(s, "_", ""), 64)
		default:
			e := fe("float_parsing", l, "Input should be a valid number, unable to parse string as a number", v, nil)
			return 0, &e
		}
	default:
		e := fe("float_type", l, "Input should be a valid number", v, nil)
		return 0, &e
	}
	// pydantic-core checks le before gt (NaN fails le).
	if !(f <= le) {
		e := fe("less_than_equal", l, "Input should be less than or equal to "+trimFloat(le), v, map[string]any{"le": json.Number(pyFloat(le))})
		return 0, &e
	}
	if !(f > gt) {
		e := fe("greater_than", l, "Input should be greater than "+trimFloat(gt), v, map[string]any{"gt": json.Number(pyFloat(gt))})
		return 0, &e
	}
	return f, nil
}

func trimFloat(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

var (
	boolTrue  = map[string]bool{"1": true, "on": true, "t": true, "true": true, "y": true, "yes": true}
	boolFalse = map[string]bool{"0": true, "off": true, "f": true, "false": true, "n": true, "no": true}
)

func boolVal(v any, l []any) (bool, *httpx.FieldError) {
	bad := func(typ, msg string) (bool, *httpx.FieldError) {
		e := fe(typ, l, msg, v, nil)
		return false, &e
	}
	switch t := v.(type) {
	case bool:
		return t, nil
	case json.Number:
		f, err := strconv.ParseFloat(string(t), 64)
		if err != nil || f != math.Trunc(f) {
			if err == nil {
				return bad("bool_type", "Input should be a valid boolean")
			}
			return bad("bool_parsing", "Input should be a valid boolean, unable to interpret input")
		}
		if f == 0 || f == 1 {
			return f == 1, nil
		}
		return bad("bool_parsing", "Input should be a valid boolean, unable to interpret input")
	case string:
		s := strings.ToLower(t)
		if boolTrue[s] {
			return true, nil
		}
		if boolFalse[s] {
			return false, nil
		}
		return bad("bool_parsing", "Input should be a valid boolean, unable to interpret input")
	}
	return bad("bool_type", "Input should be a valid boolean")
}

// fields collects errors for one model.
type fields struct {
	o    *obj
	errs *[]httpx.FieldError
	base []any // loc prefix inside the body
}

func (f fields) at(name string) []any { return append(append([]any{}, f.base...), name) }

func (f fields) str(name string, min, max int, required bool, def string) (string, bool) {
	v, ok := f.o.get(name)
	if !ok {
		if required {
			*f.errs = append(*f.errs, missing(f.at(name), f.o))
		}
		return def, false
	}
	s, e := strVal(v, f.at(name), min, max)
	if e != nil {
		*f.errs = append(*f.errs, *e)
		return def, true
	}
	return s, true
}

func (f fields) int(name string, ge, le, def int) (int, bool) {
	v, ok := f.o.get(name)
	if !ok {
		return def, false
	}
	n, e := intVal(v, f.at(name), ge, le)
	if e != nil {
		*f.errs = append(*f.errs, *e)
		return def, true
	}
	return n, true
}

func (f fields) float(name string, gt, le, def float64) (float64, bool) {
	v, ok := f.o.get(name)
	if !ok {
		return def, false
	}
	x, e := floatVal(v, f.at(name), gt, le)
	if e != nil {
		*f.errs = append(*f.errs, *e)
		return def, true
	}
	return x, true
}

func parseSteps(v any, l []any, errs *[]httpx.FieldError) []Step {
	list, ok := v.([]any)
	if !ok {
		*errs = append(*errs, fe("list_type", l, "Input should be a valid list", v, nil))
		return nil
	}
	before := len(*errs)
	steps := make([]Step, 0, len(list))
	for i, item := range list {
		il := append(append([]any{}, l...), i)
		so, ok := item.(*obj)
		if !ok {
			*errs = append(*errs, fe("model_attributes_type", il, "Input should be a valid dictionary or object to extract fields from", item, nil))
			continue
		}
		f := fields{o: so, errs: errs, base: il}
		var st Step
		if v, ok := so.get("key"); !ok {
			*errs = append(*errs, missing(f.at("key"), so))
		} else if s, isStr := v.(string); !isStr {
			*errs = append(*errs, fe("string_type", f.at("key"), "Input should be a valid string", v, nil))
		} else if !stepKeyRE.MatchString(s) {
			*errs = append(*errs, fe("string_pattern_mismatch", f.at("key"), "String should match pattern '"+stepKeyPattern+"'", v, map[string]any{"pattern": stepKeyPattern}))
		} else {
			st.Key = s
		}
		st.Name, _ = f.str("name", 1, 100, true, "")
		st.PromptTemplate, _ = f.str("prompt_template", 10, 20000, true, "")
		st.OutputFormat, _ = f.str("output_format", 0, -1, false, "text")
		steps = append(steps, st)
	}
	if len(*errs) == before && len(list) > 8 {
		*errs = append(*errs, fe("too_long", l, fmt.Sprintf("List should have at most 8 items after validation, not %d", len(list)), v,
			map[string]any{"field_type": "List", "max_length": 8, "actual_length": len(list)}))
	}
	return steps
}

// stepsModelCheck is the model_validator(mode="after") of Create and Patch.
func stepsModelCheck(steps []Step, body *obj) []httpx.FieldError {
	seen := map[string]bool{}
	for _, s := range steps {
		seen[s.Key] = true
	}
	valueErr := func(msg string) []httpx.FieldError {
		return []httpx.FieldError{fe("value_error", loc(), "Value error, "+msg, body, map[string]any{"error": map[string]any{}})}
	}
	if len(seen) != len(steps) {
		return valueErr("playbook step keys must be unique")
	}
	for _, s := range steps {
		if !ValidOutputFormat(s.OutputFormat) {
			return valueErr("step output_format must be text or json")
		}
	}
	return nil
}

// ParseCreate validates a PlaybookCreate body.
func ParseCreate(body []byte) (CreateInput, []httpx.FieldError) {
	o, errs := decodeBody(body)
	if errs != nil {
		return CreateInput{}, errs
	}
	f := fields{o: o, errs: &errs, base: loc()}
	var in CreateInput
	in.Name, _ = f.str("name", 1, 200, true, "")
	in.Description, _ = f.str("description", 0, 2000, false, "")
	in.PromptTemplate, _ = f.str("prompt_template", 10, 20000, true, "")
	in.OutputFormat, _ = f.str("output_format", 0, -1, false, "text")
	in.MaxSteps, _ = f.int("max_steps", 1, 8, 4)
	in.CellBudgetUSD, _ = f.float("cell_budget_usd", 0, 5, 0.10)
	in.Steps = []Step{}
	if v, ok := o.get("steps"); ok {
		in.Steps = parseSteps(v, loc("steps"), &errs)
	}
	if len(errs) == 0 {
		errs = stepsModelCheck(in.Steps, o)
	}
	if len(errs) > 0 {
		return CreateInput{}, errs
	}
	return in, nil
}

// ParsePatch validates a PlaybookPatch body.
func ParsePatch(body []byte) (PatchInput, []httpx.FieldError) {
	o, errs := decodeBody(body)
	if errs != nil {
		return PatchInput{}, errs
	}
	f := fields{o: o, errs: &errs, base: loc()}
	var in PatchInput
	if s, ok := f.str("name", 1, 200, false, ""); ok {
		in.Name = &s
	}
	if s, ok := f.str("description", 0, 2000, false, ""); ok {
		in.Description = &s
	}
	if s, ok := f.str("prompt_template", 10, 20000, false, ""); ok {
		in.PromptTemplate = &s
	}
	if s, ok := f.str("output_format", 0, -1, false, ""); ok {
		in.OutputFormat = &s
	}
	if n, ok := f.int("max_steps", 1, 8, 0); ok {
		in.MaxSteps = &n
	}
	if x, ok := f.float("cell_budget_usd", 0, 5, 0); ok {
		in.CellBudgetUSD = &x
	}
	if v, ok := o.get("enabled"); ok {
		if b, e := boolVal(v, loc("enabled")); e != nil {
			errs = append(errs, *e)
		} else {
			in.Enabled = &b
		}
	}
	if v, ok := o.get("schedule_audience_id"); ok {
		in.ScheduleAudienceSet = true
		if v != nil {
			if s, e := strVal(v, loc("schedule_audience_id"), 0, -1); e != nil {
				errs = append(errs, *e)
			} else {
				in.ScheduleAudienceID = &s
			}
		}
	}
	if v, ok := o.get("schedule_interval_minutes"); ok {
		in.ScheduleIntervalSet = true
		if v != nil {
			if n, e := intVal(v, loc("schedule_interval_minutes"), 15, 10080); e != nil {
				errs = append(errs, *e)
			} else {
				in.ScheduleIntervalMinutes = &n
			}
		}
	}
	if v, ok := o.get("steps"); ok {
		steps := parseSteps(v, loc("steps"), &errs)
		if steps != nil {
			in.Steps = &steps
		}
	}
	if len(errs) == 0 && in.Steps != nil {
		errs = stepsModelCheck(*in.Steps, o)
	}
	if len(errs) > 0 {
		return PatchInput{}, errs
	}
	return in, nil
}

// ParseRunCreate validates a RunCreate body.
func ParseRunCreate(body []byte) (RunCreateInput, []httpx.FieldError) {
	o, errs := decodeBody(body)
	if errs != nil {
		return RunCreateInput{}, errs
	}
	f := fields{o: o, errs: &errs, base: loc()}
	var in RunCreateInput
	in.AudienceID, _ = f.str("audience_id", 0, -1, true, "")
	in.MaxMembers, _ = f.int("max_members", 1, 1000, 100)
	if len(errs) > 0 {
		return RunCreateInput{}, errs
	}
	return in, nil
}
