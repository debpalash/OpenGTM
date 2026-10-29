package provaccuracy

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

//go:embed data/golden.json data/recorded_outputs.json
var dataFS embed.FS

// OMap is an insertion-ordered JSON object.
type OMap struct {
	keys []string
	vals map[string]any
}

// Keys returns the keys in file order (nil-safe).
func (m *OMap) Keys() []string {
	if m == nil {
		return nil
	}
	return m.keys
}

// Len is the number of keys (nil-safe).
func (m *OMap) Len() int {
	if m == nil {
		return 0
	}
	return len(m.keys)
}

// Get returns the value for k (nil-safe).
func (m *OMap) Get(k string) (any, bool) {
	if m == nil {
		return nil, false
	}
	v, ok := m.vals[k]
	return v, ok
}

func (m *OMap) set(k string, v any) {
	if _, ok := m.vals[k]; !ok {
		m.keys = append(m.keys, k)
	}
	m.vals[k] = v
}

// decodeOrdered decodes one JSON value with objects as *OMap and numbers as json.Number.
func decodeOrdered(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return tok, nil
	}
	if d == '[' {
		arr := []any{}
		for dec.More() {
			v, err := decodeOrdered(dec)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		_, err := dec.Token()
		return arr, err
	}
	m := &OMap{vals: map[string]any{}}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		v, err := decodeOrdered(dec)
		if err != nil {
			return nil, err
		}
		m.set(kt.(string), v)
	}
	_, err = dec.Token()
	return m, err
}

// ParseOrdered decodes a JSON document keeping object key order.
func ParseOrdered(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := decodeOrdered(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("provaccuracy: trailing data")
	}
	return v, nil
}

// ParseGolden is load_golden over raw JSON: the document's "cases".
func ParseGolden(raw []byte) ([]Case, error) {
	doc, err := ParseOrdered(raw)
	if err != nil {
		return nil, err
	}
	root, _ := doc.(*OMap)
	cv, _ := root.Get("cases")
	arr, _ := cv.([]any)
	out := make([]Case, 0, len(arr))
	for _, e := range arr {
		m, ok := e.(*OMap)
		if !ok {
			return nil, fmt.Errorf("provaccuracy: golden case is not an object")
		}
		idv, ok := m.Get("id")
		if !ok {
			return nil, fmt.Errorf("provaccuracy: golden case without id")
		}
		id, _ := idv.(string)
		ev, _ := m.Get("expected")
		exp, _ := ev.(*OMap)
		out = append(out, Case{ID: id, Expected: exp})
	}
	return out, nil
}

// ParseOutputs is load_recorded_outputs over raw JSON: the document's "outputs".
func ParseOutputs(raw []byte) (Outputs, error) {
	doc, err := ParseOrdered(raw)
	if err != nil {
		return Outputs{}, err
	}
	root, _ := doc.(*OMap)
	ov, _ := root.Get("outputs")
	cases, _ := ov.(*OMap)
	if cases == nil {
		return Outputs{Cases: &OMap{vals: map[string]any{}}}, nil
	}
	res := &OMap{vals: map[string]any{}}
	for _, id := range cases.Keys() {
		cv, _ := cases.Get(id)
		pm, _ := cv.(*OMap)
		conv := &OMap{vals: map[string]any{}}
		for _, prov := range pm.Keys() {
			pv, _ := pm.Get(prov)
			var p *Payload
			if po, ok := pv.(*OMap); ok {
				fv, _ := po.Get("fields")
				f, _ := fv.(*OMap)
				asof, _ := po.Get("asof")
				p = &Payload{Fields: f, AsOf: asof}
			}
			conv.set(prov, p)
		}
		res.set(id, conv)
	}
	return Outputs{Cases: res}, nil
}

// Dataset is the embedded golden set and recorded outputs.
func Dataset() ([]Case, Outputs, error) {
	g, err := dataFS.ReadFile("data/golden.json")
	if err != nil {
		return nil, Outputs{}, err
	}
	o, err := dataFS.ReadFile("data/recorded_outputs.json")
	if err != nil {
		return nil, Outputs{}, err
	}
	golden, err := ParseGolden(g)
	if err != nil {
		return nil, Outputs{}, err
	}
	out, err := ParseOutputs(o)
	return golden, out, err
}

// RunEval is run_eval over the embedded data: the ranked providers as of today.
func RunEval(today time.Time) ([]*ProviderScore, error) {
	golden, outputs, err := Dataset()
	if err != nil {
		return nil, err
	}
	return Rank(ScoreOutputs(golden, outputs, today)), nil
}
