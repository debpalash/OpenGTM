package runplan

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// topLevelKeyOrder returns the key order of the object stored under top-level key.
func topLevelKeyOrder(t testing.TB, dec *json.Decoder, key string) []string {
	t.Helper()
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("not an object: %v %v", tok, err)
	}
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		if k.(string) != key {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
			t.Fatalf("%s is not an object", key)
		}
		var order []string
		for dec.More() {
			name, _ := dec.Token()
			order = append(order, name.(string))
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				t.Fatal(err)
			}
		}
		return order
	}
	t.Fatalf("no key %s", key)
	return nil
}
