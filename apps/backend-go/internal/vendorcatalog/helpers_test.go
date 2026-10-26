package vendorcatalog

import (
	"bytes"
	"encoding/json"
	"testing"
)

type ordered struct {
	keys []string
	m    map[string][]string
}

// orderedProviders decodes a JSON object keeping key order.
func orderedProviders(t *testing.T, raw json.RawMessage) ordered {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil {
		t.Fatal(err)
	}
	o := ordered{m: map[string][]string{}}
	for dec.More() {
		k, _ := dec.Token()
		var v []string
		if err := dec.Decode(&v); err != nil {
			t.Fatal(err)
		}
		o.keys = append(o.keys, k.(string))
		o.m[k.(string)] = v
	}
	return o
}

// jsonTextEqual compares compact JSON text byte for byte after stripping
// insignificant whitespace (the vectors are written by json.dumps).
func jsonTextEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y bytes.Buffer
	if err := json.Compact(&x, a); err != nil {
		t.Fatal(err)
	}
	if err := json.Compact(&y, b); err != nil {
		t.Fatal(err)
	}
	return bytes.Equal(x.Bytes(), y.Bytes())
}
