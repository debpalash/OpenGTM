// Package rowtmpl ports the row template resolver: _get_lead_values,
// _resolve_prompt, output._resolve and _resolve_deep. Templates are plain
// {placeholder} substitution; no template engine, no code execution.
package rowtmpl

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
)

// LeadValues is _get_lead_values: a flat {key: text} map for template
// resolution. Precedence (lowest to highest): raw lead_data copies, display-name
// aliases, column-id values.
func LeadValues(leadData map[string]any, cols []colgraph.Col) map[string]string {
	values := map[string]string{}
	aliases := map[string]string{}
	for _, col := range cols {
		colID := ""
		if v, ok := col.Lookup("id"); ok {
			colID = idText(v)
		}
		leadField := colID
		var leadFieldAny any = colID
		if v, ok := col.Lookup("lead_field"); ok {
			leadFieldAny = v
			leadField, _ = v.(string)
		}
		_, isStr := leadFieldAny.(string)
		inLead := false
		if isStr {
			_, inLead = leadData[leadField]
		}
		if t, _ := col.Get("type").(string); t == "lead_field" && inLead {
			values[colID] = CellText(leadData[leadField])
			values[leadField] = CellText(leadData[leadField])
		} else {
			values[colID] = CellText(leadData[colID])
		}
		if name, ok := col.Get("name").(string); ok && name != "" {
			aliases[name] = values[colID]
		}
	}
	out := make(map[string]string, len(leadData)+len(aliases)+len(values))
	for k, v := range leadData {
		out[k] = CellText(v)
	}
	for k, v := range aliases {
		out[k] = v
	}
	for k, v := range values {
		out[k] = v
	}
	return out
}

func idText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return PyStr(v)
}

// DecodeOrdered decodes JSON keeping object key order (objects become
// colgraph.Col, numbers json.Number), so ResolveDeep can preserve order.
func DecodeOrdered(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("rowtmpl: trailing data")
	}
	return v, nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return tok, nil
	}
	switch d {
	case '{':
		c := colgraph.Col{Vals: map[string]any{}}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			v, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			c.Set(kt.(string), v)
		}
		_, err := dec.Token()
		return c, err
	case '[':
		arr := []any{}
		for dec.More() {
			v, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		_, err := dec.Token()
		return arr, err
	}
	return nil, errors.New("rowtmpl: unexpected delimiter")
}

// MarshalOrdered encodes values produced by DecodeOrdered/ResolveDeep, keeping
// colgraph.Col key order.
func MarshalOrdered(v any) ([]byte, error) {
	var b bytes.Buffer
	if err := writeOrdered(&b, v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func writeOrdered(b *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case colgraph.Col:
		b.WriteByte('{')
		for i, k := range t.Keys {
			if i > 0 {
				b.WriteByte(',')
			}
			kb, _ := json.Marshal(k)
			b.Write(kb)
			b.WriteByte(':')
			if err := writeOrdered(b, t.Vals[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeOrdered(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	default:
		eb, err := json.Marshal(v)
		if err != nil {
			return err
		}
		b.Write(eb)
	}
	return nil
}
