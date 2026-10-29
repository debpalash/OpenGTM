package rowtmpl

import (
	"strings"
	"testing"
)

func TestOrderedDepthGuard(t *testing.T) {
	deep := func(n int) string { return strings.Repeat("[", n) + strings.Repeat("]", n) }
	if _, err := DecodeOrdered([]byte(deep(200))); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeOrdered([]byte(deep(201))); err == nil {
		t.Fatal("expected depth error")
	}
	var v any = "x"
	for i := 0; i < 201; i++ {
		v = []any{v}
	}
	if _, err := MarshalOrdered(v); err == nil {
		t.Fatal("expected marshal depth error")
	}
}
