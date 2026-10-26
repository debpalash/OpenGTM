package destvalidate

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// ValidationError carries the FastAPI 422 detail list for a request body.
type ValidationError struct{ Errors []httpx.FieldError }

func (e *ValidationError) Error() string {
	if len(e.Errors) == 0 {
		return "validation error"
	}
	return "validation error: " + e.Errors[0].Msg
}

// Opt is an optional PATCH field: Set means the key was present (pydantic's
// exclude_unset), Null means it was present with a JSON null.
type Opt[T any] struct {
	Set, Null bool
	Val       T
}

// Create is DestinationCreate. ConfigKeys/FieldMapKeys keep the request's key
// order (the config column is JSON, which keeps order).
type Create struct {
	AudienceID      string
	Name            string
	DestinationType string
	Enabled         bool
	Config          map[string]any
	ConfigKeys      []string
	FieldMap        map[string]string
	FieldMapKeys    []string
}

// Patch is DestinationPatch with exclude_unset semantics.
type Patch struct {
	Name         Opt[string]
	Enabled      Opt[bool]
	Config       Opt[map[string]any]
	ConfigKeys   []string
	FieldMap     Opt[map[string]string]
	FieldMapKeys []string
}

// Dump is model_dump(exclude_unset=True): only the keys the client sent.
func (p Patch) Dump() map[string]any {
	out := map[string]any{}
	put := func(set, null bool, key string, v any) {
		if !set {
			return
		}
		if null {
			out[key] = nil
			return
		}
		out[key] = v
	}
	put(p.Name.Set, p.Name.Null, "name", p.Name.Val)
	put(p.Enabled.Set, p.Enabled.Null, "enabled", p.Enabled.Val)
	put(p.Config.Set, p.Config.Null, "config", p.Config.Val)
	put(p.FieldMap.Set, p.FieldMap.Null, "field_map", p.FieldMap.Val)
	return out
}

// Inbound is InboundEvent.
type Inbound struct {
	ExternalEventID  string
	ExternalRecordID *string
	LeadID           *int
	Fields           map[string]any
	FieldKeys        []string
}

// body is a parsed top-level JSON object: raw values in the client's key order.
type body struct {
	raw  json.RawMessage
	keys []string
	vals map[string]json.RawMessage
}

func (b *body) has(k string) bool { _, ok := b.vals[k]; return ok }

func rawIsNull(r json.RawMessage) bool { return string(bytes.TrimSpace(r)) == "null" }

func decodeAny(r json.RawMessage) any {
	dec := json.NewDecoder(bytes.NewReader(r))
	dec.UseNumber()
	var v any
	_ = dec.Decode(&v)
	return v
}

func ve(typ string, loc []any, msg string, input any, ctx map[string]any) httpx.FieldError {
	return httpx.FieldError{Type: typ, Loc: append([]any{"body"}, loc...), Msg: msg, Input: input, Ctx: ctx}
}

func valueErr(loc string, msg string, input any) httpx.FieldError {
	return ve("value_error", []any{loc}, "Value error, "+msg, input, map[string]any{"error": map[string]any{}})
}

// parseBody parses the request body as a JSON object (last duplicate key wins,
// first position kept), returning FastAPI-shaped errors otherwise.
func parseBody(raw []byte) (*body, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, &ValidationError{Errors: []httpx.FieldError{ve("missing", nil, "Field required", nil, nil)}}
	}
	if !json.Valid(raw) {
		msg := "Expecting value"
		var syn *json.SyntaxError
		pos := 0
		if err := json.Unmarshal(raw, new(any)); errors.As(err, &syn) {
			pos = int(syn.Offset)
			msg = syn.Error()
		}
		return nil, &ValidationError{Errors: []httpx.FieldError{
			ve("json_invalid", []any{pos}, "JSON decode error", map[string]any{}, map[string]any{"error": msg}),
		}}
	}
	trim := bytes.TrimSpace(raw)
	if trim[0] != '{' {
		return nil, &ValidationError{Errors: []httpx.FieldError{
			ve("model_attributes_type", nil, "Input should be a valid dictionary or object to extract fields from", json.RawMessage(trim), nil),
		}}
	}
	b := &body{raw: json.RawMessage(trim), vals: map[string]json.RawMessage{}}
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

func (b *body) missing(k string) httpx.FieldError {
	return ve("missing", []any{k}, "Field required", b.raw, nil)
}

// strField validates a str field (lax mode: no coercion from other types).
func strField(k string, r json.RawMessage, min, max int) (string, *httpx.FieldError) {
	v := decodeAny(r)
	s, ok := v.(string)
	if !ok {
		e := ve("string_type", []any{k}, "Input should be a valid string", json.RawMessage(r), nil)
		return "", &e
	}
	n := utf8.RuneCountInString(s)
	if min > 0 && n < min {
		e := ve("string_too_short", []any{k}, "String should have at least "+plural(min, "character"), s, map[string]any{"min_length": min})
		return "", &e
	}
	if max > 0 && n > max {
		e := ve("string_too_long", []any{k}, "String should have at most "+plural(max, "character"), s, map[string]any{"max_length": max})
		return "", &e
	}
	return s, nil
}

func plural(n int, w string) string {
	if n == 1 {
		return "1 " + w
	}
	return strconv.Itoa(n) + " " + w + "s"
}

// boolField is pydantic's lax bool.
func boolField(k string, r json.RawMessage) (bool, *httpx.FieldError) {
	parsing := func() (bool, *httpx.FieldError) {
		e := ve("bool_parsing", []any{k}, "Input should be a valid boolean, unable to interpret input", decodeAny(r), nil)
		return false, &e
	}
	switch t := decodeAny(r).(type) {
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
	e := ve("bool_type", []any{k}, "Input should be a valid boolean", json.RawMessage(r), nil)
	return false, &e
}

// dictField decodes an object value, returning its keys in order.
func dictField(k string, r json.RawMessage) (map[string]any, []string, *httpx.FieldError) {
	if t := bytes.TrimSpace(r); len(t) == 0 || t[0] != '{' {
		e := ve("dict_type", []any{k}, "Input should be a valid dictionary", json.RawMessage(r), nil)
		return nil, nil, &e
	}
	b, _ := parseBody(r)
	out := make(map[string]any, len(b.vals))
	for kk, v := range b.vals {
		out[kk] = decodeAny(v)
	}
	return out, b.keys, nil
}

// strDictField is dict[str, str].
func strDictField(k string, r json.RawMessage) (map[string]string, []string, []httpx.FieldError) {
	m, keys, e := dictField(k, r)
	if e != nil {
		return nil, nil, []httpx.FieldError{*e}
	}
	out := make(map[string]string, len(m))
	var errs []httpx.FieldError
	b, _ := parseBody(r)
	for _, kk := range keys {
		s, ok := m[kk].(string)
		if !ok {
			errs = append(errs, ve("string_type", []any{k, kk}, "Input should be a valid string", json.RawMessage(b.vals[kk]), nil))
			continue
		}
		out[kk] = s
	}
	if len(errs) > 0 {
		return nil, nil, errs
	}
	return out, keys, nil
}

func cleanName(s string, raw json.RawMessage) (string, *httpx.FieldError) {
	s = colgraph.PyStrip(s)
	if s == "" {
		e := valueErr("name", "name cannot be blank", decodeAny(raw))
		return "", &e
	}
	return s, nil
}

func finish(errs []httpx.FieldError) error {
	if len(errs) == 0 {
		return nil
	}
	return &ValidationError{Errors: errs}
}

// DecodeCreate parses and validates a DestinationCreate body. The destination
// type is lower-cased; config is not validated here (see ValidateConfig).
func DecodeCreate(raw []byte) (Create, error) {
	var c Create
	b, err := parseBody(raw)
	if err != nil {
		return c, err
	}
	var errs []httpx.FieldError
	add := func(e *httpx.FieldError) bool {
		if e != nil {
			errs = append(errs, *e)
		}
		return e != nil
	}
	if r, ok := b.vals["audience_id"]; !ok {
		errs = append(errs, b.missing("audience_id"))
	} else {
		var e *httpx.FieldError
		if c.AudienceID, e = strField("audience_id", r, 0, 0); e != nil {
			errs = append(errs, *e)
		}
	}
	if r, ok := b.vals["name"]; !ok {
		errs = append(errs, b.missing("name"))
	} else if s, e := strField("name", r, 1, 200); !add(e) {
		var e2 *httpx.FieldError
		if c.Name, e2 = cleanName(s, r); e2 != nil {
			errs = append(errs, *e2)
		}
	}
	if r, ok := b.vals["destination_type"]; !ok {
		errs = append(errs, b.missing("destination_type"))
	} else if s, e := strField("destination_type", r, 0, 0); !add(e) {
		c.DestinationType = colgraph.PyLower(s)
		if !IsType(c.DestinationType) {
			errs = append(errs, valueErr("destination_type", "unsupported destination type '"+c.DestinationType+"'", s))
		}
	}
	c.Enabled = true
	if r, ok := b.vals["enabled"]; ok {
		var e *httpx.FieldError
		if c.Enabled, e = boolField("enabled", r); e != nil {
			errs = append(errs, *e)
		}
	}
	c.Config, c.ConfigKeys = map[string]any{}, nil
	if r, ok := b.vals["config"]; ok {
		var e *httpx.FieldError
		if c.Config, c.ConfigKeys, e = dictField("config", r); e != nil {
			errs = append(errs, *e)
		}
	}
	c.FieldMap = map[string]string{}
	if r, ok := b.vals["field_map"]; ok {
		var es []httpx.FieldError
		if c.FieldMap, c.FieldMapKeys, es = strDictField("field_map", r); es != nil {
			errs = append(errs, es...)
		}
	}
	if err := finish(errs); err != nil {
		return Create{}, err
	}
	return c, nil
}

// DecodePatch parses a DestinationPatch body. Absent keys stay unset; a present
// null is Set+Null (pydantic's exclude_unset keeps it in the dump).
func DecodePatch(raw []byte) (Patch, error) {
	var p Patch
	b, err := parseBody(raw)
	if err != nil {
		return p, err
	}
	var errs []httpx.FieldError
	if r, ok := b.vals["name"]; ok {
		p.Name.Set = true
		if rawIsNull(r) {
			p.Name.Null = true
		} else if s, e := strField("name", r, 1, 200); e != nil {
			errs = append(errs, *e)
		} else if p.Name.Val, e = cleanName(s, r); e != nil {
			errs = append(errs, *e)
		}
	}
	if r, ok := b.vals["enabled"]; ok {
		p.Enabled.Set = true
		if rawIsNull(r) {
			p.Enabled.Null = true
		} else {
			var e *httpx.FieldError
			if p.Enabled.Val, e = boolField("enabled", r); e != nil {
				errs = append(errs, *e)
			}
		}
	}
	if r, ok := b.vals["config"]; ok {
		p.Config.Set = true
		if rawIsNull(r) {
			p.Config.Null = true
		} else {
			var e *httpx.FieldError
			if p.Config.Val, p.ConfigKeys, e = dictField("config", r); e != nil {
				errs = append(errs, *e)
			}
		}
	}
	if r, ok := b.vals["field_map"]; ok {
		p.FieldMap.Set = true
		if rawIsNull(r) {
			p.FieldMap.Null = true
		} else {
			var es []httpx.FieldError
			if p.FieldMap.Val, p.FieldMapKeys, es = strDictField("field_map", r); es != nil {
				errs = append(errs, es...)
			}
		}
	}
	if err := finish(errs); err != nil {
		return Patch{}, err
	}
	return p, nil
}

// DecodeInbound parses an InboundEvent body.
func DecodeInbound(raw []byte) (Inbound, error) {
	var in Inbound
	b, err := parseBody(raw)
	if err != nil {
		return in, err
	}
	var errs []httpx.FieldError
	if r, ok := b.vals["external_event_id"]; !ok {
		errs = append(errs, b.missing("external_event_id"))
	} else if s, e := strField("external_event_id", r, 1, 255); e != nil {
		errs = append(errs, *e)
	} else {
		in.ExternalEventID = s
	}
	if r, ok := b.vals["external_record_id"]; ok && !rawIsNull(r) {
		if s, e := strField("external_record_id", r, 0, 255); e != nil {
			errs = append(errs, *e)
		} else {
			in.ExternalRecordID = &s
		}
	}
	if r, ok := b.vals["lead_id"]; ok && !rawIsNull(r) {
		if n, e := intField("lead_id", r, 1); e != nil {
			errs = append(errs, *e)
		} else {
			in.LeadID = &n
		}
	}
	in.Fields = map[string]any{}
	if r, ok := b.vals["fields"]; ok {
		m, keys, e := dictField("fields", r)
		if e != nil {
			errs = append(errs, *e)
		} else if msg := checkInboundFields(m); msg != "" {
			errs = append(errs, valueErr("fields", msg, json.RawMessage(r)))
		} else {
			in.Fields, in.FieldKeys = m, keys
		}
	}
	if err := finish(errs); err != nil {
		return Inbound{}, err
	}
	return in, nil
}

func checkInboundFields(m map[string]any) string {
	if len(m) > 50 {
		return "fields may contain at most 50 entries"
	}
	for _, v := range m {
		switch v.(type) {
		case map[string]any, []any:
			return "field values must be scalar"
		}
	}
	for _, v := range m {
		if v != nil && utf8.RuneCountInString(colgraph.PyStr(v)) > 10000 {
			return "field values may not exceed 10000 characters"
		}
	}
	return ""
}

// intField is pydantic's lax int with a ge bound.
func intField(k string, r json.RawMessage, ge int) (int, *httpx.FieldError) {
	v := decodeAny(r)
	var n int64
	switch t := v.(type) {
	case json.Number:
		if i, err := strconv.ParseInt(string(t), 10, 64); err == nil {
			n = i
		} else if f, err := strconv.ParseFloat(string(t), 64); err == nil && !math.IsInf(f, 0) && f == math.Trunc(f) && math.Abs(f) < 1<<62 {
			n = int64(f)
		} else if err == nil && !math.IsInf(f, 0) {
			e := ve("int_from_float", []any{k}, "Input should be a valid integer, got a number with a fractional part", v, nil)
			return 0, &e
		} else {
			e := ve("int_parsing", []any{k}, "Input should be a valid integer, unable to parse string as an integer", v, nil)
			return 0, &e
		}
	case string:
		i, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		if err != nil {
			e := ve("int_parsing", []any{k}, "Input should be a valid integer, unable to parse string as an integer", v, nil)
			return 0, &e
		}
		n = i
	default:
		e := ve("int_type", []any{k}, "Input should be a valid integer", v, nil)
		return 0, &e
	}
	if n < int64(ge) {
		e := ve("greater_than_equal", []any{k}, "Input should be greater than or equal to "+strconv.Itoa(ge), v, map[string]any{"ge": ge})
		return 0, &e
	}
	return int(n), nil
}
