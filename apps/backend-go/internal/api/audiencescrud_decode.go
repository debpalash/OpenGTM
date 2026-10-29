package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audiencevalid"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// Request bodies of routers/audiences.py: AudienceCreate and AudiencePatch with pydantic v2 lax-mode validation and
// FastAPI's 422 detail entries. Fields validate in model order and every failing field is reported.

// audOpt is an optional PATCH field: Set means the key was present (exclude_unset), Null a JSON null.
type audOpt[T any] struct {
	Set, Null bool
	Val       T
}

type audCreateIn struct {
	Name, Description string
	// Filters is the filters object with the client's key order (duplicate keys: last value, first position).
	Filters        json.RawMessage
	RefreshEnabled bool
	Interval       int
}

type audPatchIn struct {
	Name, Description audOpt[string]
	Filters           audOpt[json.RawMessage]
	RefreshEnabled    audOpt[bool]
	Interval          audOpt[int]
}

// audValidation carries the 422 detail list.
type audValidation struct{ errs []httpx.FieldError }

func (e *audValidation) Error() string { return "validation error" }

const (
	audMinInterval = 15
	audMaxInterval = 10080
)

type audBody struct {
	raw  json.RawMessage
	keys []string
	vals map[string]json.RawMessage
}

func audRawNull(r json.RawMessage) bool { return string(bytes.TrimSpace(r)) == "null" }

func audDecodeAny(r json.RawMessage) any {
	dec := json.NewDecoder(bytes.NewReader(r))
	dec.UseNumber()
	var v any
	_ = dec.Decode(&v)
	return v
}

func audVE(typ string, loc []any, msg string, input any, ctx map[string]any) httpx.FieldError {
	return httpx.FieldError{Type: typ, Loc: append([]any{"body"}, loc...), Msg: msg, Input: input, Ctx: ctx}
}

func audValueErr(field, msg string, input any) httpx.FieldError {
	return audVE("value_error", []any{field}, "Value error, "+msg, input, map[string]any{"error": map[string]any{}})
}

// audParseBody parses the body as a JSON object keeping the client's key order.
func audParseBody(raw []byte) (*audBody, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, &audValidation{[]httpx.FieldError{audVE("missing", nil, "Field required", nil, nil)}}
	}
	if !json.Valid(raw) {
		msg, pos := "Expecting value", 0
		var syn *json.SyntaxError
		if err := json.Unmarshal(raw, new(any)); errors.As(err, &syn) {
			msg, pos = syn.Error(), int(syn.Offset)
		}
		return nil, &audValidation{[]httpx.FieldError{
			audVE("json_invalid", []any{pos}, "JSON decode error", map[string]any{}, map[string]any{"error": msg})}}
	}
	trim := bytes.TrimSpace(raw)
	if trim[0] != '{' {
		return nil, &audValidation{[]httpx.FieldError{audVE("model_attributes_type", nil,
			"Input should be a valid dictionary or object to extract fields from", json.RawMessage(trim), nil)}}
	}
	b := &audBody{raw: json.RawMessage(trim), vals: map[string]json.RawMessage{}}
	dec := json.NewDecoder(bytes.NewReader(trim))
	_, _ = dec.Token()
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			break
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			break
		}
		k := kt.(string)
		if _, dup := b.vals[k]; !dup {
			b.keys = append(b.keys, k)
		}
		b.vals[k] = v
	}
	return b, nil
}

func audPlural(n int, w string) string {
	if n == 1 {
		return "1 " + w
	}
	return strconv.Itoa(n) + " " + w + "s"
}

// audStr is a str field (no coercion from other types) with length bounds counted in characters.
func audStr(k string, r json.RawMessage, min, max int) (string, *httpx.FieldError) {
	s, ok := audDecodeAny(r).(string)
	if !ok {
		e := audVE("string_type", []any{k}, "Input should be a valid string", json.RawMessage(r), nil)
		return "", &e
	}
	n := utf8.RuneCountInString(s)
	if min > 0 && n < min {
		e := audVE("string_too_short", []any{k}, "String should have at least "+audPlural(min, "character"), s, map[string]any{"min_length": min})
		return "", &e
	}
	if max > 0 && n > max {
		e := audVE("string_too_long", []any{k}, "String should have at most "+audPlural(max, "character"), s, map[string]any{"max_length": max})
		return "", &e
	}
	return s, nil
}

// audBool is pydantic's lax bool.
func audBool(k string, r json.RawMessage) (bool, *httpx.FieldError) {
	parsing := func() (bool, *httpx.FieldError) {
		e := audVE("bool_parsing", []any{k}, "Input should be a valid boolean, unable to interpret input", audDecodeAny(r), nil)
		return false, &e
	}
	switch t := audDecodeAny(r).(type) {
	case bool:
		return t, nil
	case string:
		switch strings.ToLower(t) {
		case "true", "t", "yes", "y", "on", "1":
			return true, nil
		case "false", "f", "no", "n", "off", "0":
			return false, nil
		}
		return parsing()
	case json.Number:
		switch f, _ := strconv.ParseFloat(string(t), 64); f {
		case 0:
			return false, nil
		case 1:
			return true, nil
		}
		return parsing()
	}
	e := audVE("bool_type", []any{k}, "Input should be a valid boolean", json.RawMessage(r), nil)
	return false, &e
}

// audIntString is a str that pydantic lax int accepts: optional sign, digits with single underscores, an all-zero fraction.
var audIntString = regexp.MustCompile(`^([+-]?\d+(?:_\d+)*)(?:\.(\d+))?$`)

// audInt is pydantic's lax int bounded by ge and le.
func audInt(k string, r json.RawMessage, ge, le int) (int, *httpx.FieldError) {
	v := audDecodeAny(r)
	bad := func(typ, msg string) (int, *httpx.FieldError) {
		e := audVE(typ, []any{k}, msg, v, nil)
		return 0, &e
	}
	var n *big.Int
	switch t := v.(type) {
	case bool:
		n = big.NewInt(0)
		if t {
			n = big.NewInt(1)
		}
	case json.Number:
		if i, ok := new(big.Int).SetString(string(t), 10); ok {
			n = i
		} else if f, err := strconv.ParseFloat(string(t), 64); err == nil && !math.IsInf(f, 0) && f == math.Trunc(f) {
			n, _ = new(big.Float).SetFloat64(f).Int(nil)
		} else if math.IsInf(f, 0) {
			return bad("finite_number", "Input should be a finite number")
		} else if err == nil {
			return bad("int_from_float", "Input should be a valid integer, got a number with a fractional part")
		} else {
			return bad("int_parsing", "Input should be a valid integer, unable to parse string as an integer")
		}
	case string:
		m := audIntString.FindStringSubmatch(strings.TrimSpace(t))
		if m == nil || strings.Trim(m[2], "0") != "" {
			return bad("int_parsing", "Input should be a valid integer, unable to parse string as an integer")
		}
		n, _ = new(big.Int).SetString(strings.ReplaceAll(m[1], "_", ""), 10)
	default:
		return bad("int_type", "Input should be a valid integer")
	}
	if n.Cmp(big.NewInt(int64(ge))) < 0 {
		e := audVE("greater_than_equal", []any{k}, "Input should be greater than or equal to "+strconv.Itoa(ge), v, map[string]any{"ge": ge})
		return 0, &e
	}
	if n.Cmp(big.NewInt(int64(le))) > 0 {
		e := audVE("less_than_equal", []any{k}, "Input should be less than or equal to "+strconv.Itoa(le), v, map[string]any{"le": le})
		return 0, &e
	}
	return int(n.Int64()), nil
}

// audFilters is a dict field validated by _validate_filters; it returns the object with the client's key order.
func audFilters(r json.RawMessage) (json.RawMessage, *httpx.FieldError) {
	if t := bytes.TrimSpace(r); len(t) == 0 || t[0] != '{' {
		e := audVE("dict_type", []any{"filters"}, "Input should be a valid dictionary", json.RawMessage(r), nil)
		return nil, &e
	}
	b, _ := audParseBody(r)
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range b.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		buf.Write(kb)
		buf.WriteByte(':')
		var c bytes.Buffer
		if err := json.Compact(&c, b.vals[k]); err != nil {
			c.Reset()
			c.Write(b.vals[k])
		}
		buf.Write(c.Bytes())
	}
	buf.WriteByte('}')
	ordered := json.RawMessage(buf.Bytes())
	m, _ := audDecodeAny(ordered).(map[string]any)
	if _, err := audiencevalid.ValidateFilters(m); err != nil {
		var ve *audiencevalid.ValueError
		msg := err.Error()
		if errors.As(err, &ve) {
			msg = ve.Msg
		}
		e := audValueErr("filters", msg, ordered)
		return nil, &e
	}
	return ordered, nil
}

func audName(r json.RawMessage) (string, *httpx.FieldError) {
	s, e := audStr("name", r, 1, 200)
	if e != nil {
		return "", e
	}
	if s = colgraph.PyStrip(s); s == "" {
		e := audValueErr("name", "name cannot be blank", audDecodeAny(r))
		return "", &e
	}
	return s, nil
}

func audFinish(errs []httpx.FieldError) error {
	if len(errs) == 0 {
		return nil
	}
	return &audValidation{errs}
}

// decodeAudCreate validates an AudienceCreate body.
func decodeAudCreate(raw []byte) (audCreateIn, error) {
	in := audCreateIn{Filters: json.RawMessage(`{}`), RefreshEnabled: true, Interval: 60}
	b, err := audParseBody(raw)
	if err != nil {
		return in, err
	}
	var errs []httpx.FieldError
	if r, ok := b.vals["name"]; !ok {
		errs = append(errs, audVE("missing", []any{"name"}, "Field required", b.raw, nil))
	} else {
		var e *httpx.FieldError
		if in.Name, e = audName(r); e != nil {
			errs = append(errs, *e)
		}
	}
	if r, ok := b.vals["description"]; ok {
		var e *httpx.FieldError
		if in.Description, e = audStr("description", r, 0, 2000); e != nil {
			errs = append(errs, *e)
		}
	}
	if r, ok := b.vals["filters"]; ok {
		var e *httpx.FieldError
		if in.Filters, e = audFilters(r); e != nil {
			errs = append(errs, *e)
		}
	}
	if r, ok := b.vals["refresh_enabled"]; ok {
		var e *httpx.FieldError
		if in.RefreshEnabled, e = audBool("refresh_enabled", r); e != nil {
			errs = append(errs, *e)
		}
	}
	if r, ok := b.vals["refresh_interval_minutes"]; ok {
		var e *httpx.FieldError
		if in.Interval, e = audInt("refresh_interval_minutes", r, audMinInterval, audMaxInterval); e != nil {
			errs = append(errs, *e)
		}
	}
	return in, audFinish(errs)
}

// decodeAudPatch validates an AudiencePatch body. A present null keeps Null set; the route turns it into the 409
// the NOT NULL column produces (name, description, refresh_enabled, refresh_interval_minutes) or stores JSON null
// (filters).
func decodeAudPatch(raw []byte) (audPatchIn, error) {
	var in audPatchIn
	b, err := audParseBody(raw)
	if err != nil {
		return in, err
	}
	var errs []httpx.FieldError
	if r, ok := b.vals["name"]; ok {
		in.Name.Set = true
		if audRawNull(r) {
			in.Name.Null = true
		} else {
			var e *httpx.FieldError
			if in.Name.Val, e = audName(r); e != nil {
				errs = append(errs, *e)
			}
		}
	}
	if r, ok := b.vals["description"]; ok {
		in.Description.Set = true
		if audRawNull(r) {
			in.Description.Null = true
		} else {
			var e *httpx.FieldError
			if in.Description.Val, e = audStr("description", r, 0, 2000); e != nil {
				errs = append(errs, *e)
			}
		}
	}
	if r, ok := b.vals["filters"]; ok {
		in.Filters.Set = true
		if audRawNull(r) {
			in.Filters.Null = true
		} else {
			var e *httpx.FieldError
			if in.Filters.Val, e = audFilters(r); e != nil {
				errs = append(errs, *e)
			}
		}
	}
	if r, ok := b.vals["refresh_enabled"]; ok {
		in.RefreshEnabled.Set = true
		if audRawNull(r) {
			in.RefreshEnabled.Null = true
		} else {
			var e *httpx.FieldError
			if in.RefreshEnabled.Val, e = audBool("refresh_enabled", r); e != nil {
				errs = append(errs, *e)
			}
		}
	}
	if r, ok := b.vals["refresh_interval_minutes"]; ok {
		in.Interval.Set = true
		if audRawNull(r) {
			in.Interval.Null = true
		} else {
			var e *httpx.FieldError
			if in.Interval.Val, e = audInt("refresh_interval_minutes", r, audMinInterval, audMaxInterval); e != nil {
				errs = append(errs, *e)
			}
		}
	}
	return in, audFinish(errs)
}
