package entities

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseJSONDepthLimit(t *testing.T) {
	deep := bytes.Repeat([]byte("["), 1<<20)
	if _, err := ParseJSON(deep); err == nil || !strings.Contains(err.Error(), "deeper") {
		t.Fatalf("1 MiB of '[': err = %v, want depth error", err)
	}
	obj := []byte(strings.Repeat(`{"a":`, 201) + "1" + strings.Repeat("}", 201))
	if _, err := ParseJSON(obj); err == nil {
		t.Fatal("201 nested objects accepted")
	}
	ok := []byte(strings.Repeat("[", 200) + strings.Repeat("]", 200))
	if _, err := ParseJSON(ok); err != nil {
		t.Fatalf("200 levels rejected: %v", err)
	}
}
