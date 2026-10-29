package httpx

import (
	"bytes"
	"encoding/json"
	"sort"
)

// ctxKeyRank orders pydantic's multi-key ctx objects the way pydantic-core
// emits them (field_type, min_length, max_length, actual_length); every other
// key sorts alphabetically after these. A plain map would sort all keys.
var ctxKeyRank = map[string]int{"field_type": 0, "min_length": 1, "max_length": 2, "actual_length": 3}

// MarshalJSON writes type, loc, msg, input, ctx (omitted when empty) with the
// ctx keys in pydantic order and without HTML escaping.
func (e FieldError) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	put := func(v any) error {
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(v); err != nil {
			return err
		}
		b.Truncate(b.Len() - 1) // Encode's trailing newline
		return nil
	}
	loc := e.Loc
	if loc == nil {
		loc = []any{}
	}
	b.WriteString(`{"type":`)
	if err := put(e.Type); err != nil {
		return nil, err
	}
	b.WriteString(`,"loc":`)
	if err := put(loc); err != nil {
		return nil, err
	}
	b.WriteString(`,"msg":`)
	if err := put(e.Msg); err != nil {
		return nil, err
	}
	b.WriteString(`,"input":`)
	if err := put(e.Input); err != nil {
		return nil, err
	}
	if len(e.Ctx) > 0 {
		keys := make([]string, 0, len(e.Ctx))
		for k := range e.Ctx {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			ri, iok := ctxKeyRank[keys[i]]
			rj, jok := ctxKeyRank[keys[j]]
			switch {
			case iok && jok:
				return ri < rj
			case iok != jok:
				return iok
			}
			return keys[i] < keys[j]
		})
		b.WriteString(`,"ctx":{`)
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := put(k); err != nil {
				return nil, err
			}
			b.WriteByte(':')
			if err := put(e.Ctx[k]); err != nil {
				return nil, err
			}
		}
		b.WriteByte('}')
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}
