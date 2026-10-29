package chathistory

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestAutoTitleVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/auto_title.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Cases []struct{ Input, Title string } `json:"cases"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Cases) == 0 {
		t.Fatal("no vectors")
	}
	for i, c := range f.Cases {
		if got := AutoTitle(c.Input); got != c.Title {
			t.Errorf("case %d (%q): got %q want %q", i, c.Input, got, c.Title)
		}
	}
}

func TestAutoTitleRuneSafe(t *testing.T) {
	in := ""
	for i := 0; i < 70; i++ {
		in += "é"
	}
	got := AutoTitle(in)
	if got != strings.Repeat("é", 60)+"..." {
		t.Fatalf("rune-safe cut at 60 then ellipsis: %q", got)
	}
}
