package api

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// Request validation for the two JSON bodies of the LLM routes, reproducing FastAPI/pydantic v2 (lax mode):
// errors come in model field order with loc prefix "body" and the offending value echoed as input. Known gap
// (shared with the other body validators): the ctx.error text and position of a json_invalid body.

const maxLLMBody = 1 << 20

func readLLMBody(w http.ResponseWriter, r *http.Request) (*wbcrud.Obj, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxLLMBody))
	if err != nil {
		httpx.Detail(w, http.StatusRequestEntityTooLarge, "Request body too large")
		return nil, false
	}
	obj, errs := decodeLLMBody(raw)
	if errs != nil {
		httpx.ValidationError(w, errs...)
		return nil, false
	}
	return obj, true
}

// decodeLLMBody is wbcrud.DecodeBodyObject plus FastAPI's rule that a JSON null body is an absent body.
func decodeLLMBody(raw []byte) (*wbcrud.Obj, []httpx.FieldError) {
	if string(bytes.TrimSpace(raw)) == "null" {
		return nil, []httpx.FieldError{{Type: "missing", Loc: []any{"body"}, Msg: "Field required", Input: nil}}
	}
	return wbcrud.DecodeBodyObject(raw)
}

func bodyField(name string, rest ...any) []any { return append([]any{"body", name}, rest...) }

func fieldMissing(name string, whole any) httpx.FieldError {
	return httpx.FieldError{Type: "missing", Loc: bodyField(name), Msg: "Field required", Input: whole}
}

func stringType(loc []any, v any) httpx.FieldError {
	return httpx.FieldError{Type: "string_type", Loc: loc, Msg: "Input should be a valid string", Input: v}
}

func llmPlural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return llmItoa(n) + " " + word + "s"
}

func llmItoa(n int) string { return big.NewInt(int64(n)).String() }

// strField validates `name: str` (default def when optional and absent). min/max are code-point bounds (max < 0:
// none).
func strField(o *wbcrud.Obj, name string, required bool, def string, min, max int, errs *[]httpx.FieldError) string {
	v, present := o.Get(name)
	if !present {
		if required {
			*errs = append(*errs, fieldMissing(name, o))
		}
		return def
	}
	s, ok := v.(string)
	if !ok {
		*errs = append(*errs, stringType(bodyField(name), v))
		return def
	}
	n := utf8.RuneCountInString(s)
	if min > 0 && n < min {
		*errs = append(*errs, httpx.FieldError{Type: "string_too_short", Loc: bodyField(name), Msg: "String should have at least " + llmPlural(min, "character"), Input: v, Ctx: map[string]any{"min_length": min}})
		return def
	}
	if max >= 0 && n > max {
		*errs = append(*errs, httpx.FieldError{Type: "string_too_long", Loc: bodyField(name), Msg: "String should have at most " + llmPlural(max, "character"), Input: v, Ctx: map[string]any{"max_length": max}})
		return def
	}
	return s
}

var floatIntStr = regexp.MustCompile(`^[+-]?[0-9]+\.0+$`)

// laxInt validates one `int` list element the way pydantic's lax mode does for JSON-decoded input. ok=false
// reports err. An integer outside int64 is accepted (Python ints are unbounded) and returned as 0, an id that
// matches no row.
func pydLaxInt(v any, loc []any) (n int64, err *httpx.FieldError) {
	bad := func(typ, msg string) (int64, *httpx.FieldError) {
		return 0, &httpx.FieldError{Type: typ, Loc: loc, Msg: msg, Input: v}
	}
	fromBig := func(b *big.Int) (int64, *httpx.FieldError) {
		if b.IsInt64() {
			return b.Int64(), nil
		}
		return 0, nil
	}
	switch x := v.(type) {
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case json.Number:
		s := string(x)
		if !strings.ContainsAny(s, ".eE") {
			b, ok := new(big.Int).SetString(s, 10)
			if !ok {
				return bad("int_type", "Input should be a valid integer")
			}
			return fromBig(b)
		}
		f, perr := x.Float64()
		if perr != nil || math.IsInf(f, 0) {
			return bad("finite_number", "Input should be a finite number")
		}
		if f != math.Trunc(f) {
			return bad("int_from_float", "Input should be a valid integer, got a number with a fractional part")
		}
		if math.Abs(f) >= 1<<63 {
			return bad("int_parsing_size", "Unable to parse input string as an integer, exceeded maximum size")
		}
		return int64(f), nil
	case string:
		t := strings.TrimSpace(x)
		if b, ok := wbcrud.ParseIntBig(t); ok {
			return fromBig(b)
		}
		if floatIntStr.MatchString(t) {
			b, _ := new(big.Int).SetString(strings.SplitN(t, ".", 2)[0], 10)
			return fromBig(b)
		}
		return bad("int_parsing", "Input should be a valid integer, unable to parse string as an integer")
	}
	return bad("int_type", "Input should be a valid integer")
}

// intList validates `name: List[int]` (required).
func intList(o *wbcrud.Obj, name string, errs *[]httpx.FieldError) []int64 {
	v, present := o.Get(name)
	if !present {
		*errs = append(*errs, fieldMissing(name, o))
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		*errs = append(*errs, httpx.FieldError{Type: "list_type", Loc: bodyField(name), Msg: "Input should be a valid list", Input: v})
		return nil
	}
	out := make([]int64, 0, len(arr))
	for i, e := range arr {
		n, err := pydLaxInt(e, bodyField(name, i))
		if err != nil {
			*errs = append(*errs, *err)
			continue
		}
		out = append(out, n)
	}
	return out
}
