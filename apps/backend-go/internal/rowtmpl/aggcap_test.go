package rowtmpl

import (
	"strings"
	"testing"
)

func TestResolveDeepAggregateCap(t *testing.T) {
	big := strings.Repeat("x", 8<<10)
	items := make([]any, 200) // 200 x 8 KiB = 1.6 MiB resolved output > 1 MiB
	for i := range items {
		items[i] = "{k}"
	}
	if _, err := ResolveDeep(items, map[string]any{"k": big}, nil); err != ErrTooLarge {
		t.Fatalf("aggregate output: %v", err)
	}
	many := make([]any, MaxDeepNodes+1)
	for i := range many {
		many[i] = "s"
	}
	if _, err := ResolveDeep(many, nil, nil); err != ErrTooLarge {
		t.Fatalf("nodes: %v", err)
	}
	if _, err := ResolveDeep(items[:50], map[string]any{"k": big}, nil); err != nil {
		t.Fatal(err)
	}
}
