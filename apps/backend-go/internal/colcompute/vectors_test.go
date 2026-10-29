package colcompute

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"testing"
)

// loadVectors decodes a vector file keeping numbers exact (json.Number).
func loadVectors(t *testing.T, name string, into any) {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(into); err != nil {
		t.Fatal(err)
	}
}

// tagged is the generator's type-tagged value: {"t": "int", "v": "5"} etc.
type tagged = map[string]any

// matchTag reports a mismatch between a Python-recorded tagged value and a Go value.
func matchTag(want tagged, got any) string {
	t, _ := want["t"].(string)
	switch t {
	case "none":
		if got != nil {
			return fmt.Sprintf("want None, got %T %v", got, got)
		}
	case "bool":
		if b, ok := got.(bool); !ok || b != want["v"].(bool) {
			return fmt.Sprintf("want bool %v, got %T %v", want["v"], got, got)
		}
	case "int":
		i, ok := got.(*big.Int)
		if !ok || i.String() != want["v"].(string) {
			return fmt.Sprintf("want int %v, got %T %v", want["v"], got, got)
		}
	case "float":
		f, ok := got.(float64)
		if !ok || floatRepr(f) != want["repr"].(string) {
			return fmt.Sprintf("want float %v, got %T %v", want["repr"], got, got)
		}
	case "str":
		if s, ok := got.(string); !ok || s != want["v"].(string) {
			return fmt.Sprintf("want str %q, got %T %v", want["v"], got, got)
		}
	case "bytes":
		b, ok := got.([]byte)
		if !ok || bytesRepr(b) != want["repr"].(string) {
			return fmt.Sprintf("want bytes %v, got %T %v", want["repr"], got, got)
		}
	case "complex":
		c, ok := got.(complex128)
		if !ok || complexRepr(c) != want["repr"].(string) {
			return fmt.Sprintf("want complex %v, got %T %v", want["repr"], got, got)
		}
	case "ellipsis":
		if _, ok := got.(Ellipsis); !ok {
			return fmt.Sprintf("want Ellipsis, got %T", got)
		}
	case "list":
		l, ok := got.([]any)
		items, _ := want["v"].([]any)
		if !ok || len(l) != len(items) {
			return fmt.Sprintf("want list of %d, got %T %v", len(items), got, got)
		}
		for i, it := range items {
			if m := matchTag(it.(map[string]any), l[i]); m != "" {
				return fmt.Sprintf("[%d]: %s", i, m)
			}
		}
	default:
		return fmt.Sprintf("unknown tag %q", t)
	}
	return ""
}
