package chatmemory_test

import (
	"encoding/json"
	"os"
	"sort"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/chatmemory"
)

func TestTokensVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/memory_tokens.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Cases []struct {
			Text   string   `json:"text"`
			Tokens []string `json:"tokens"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Cases {
		got := []string{}
		for k := range chatmemory.Tokens(c.Text) {
			got = append(got, k)
		}
		sort.Strings(got)
		g, _ := json.Marshal(got)
		w, _ := json.Marshal(c.Tokens)
		if string(g) != string(w) {
			t.Errorf("Tokens(%q) = %s, want %s", c.Text, g, w)
		}
	}
}

// Python str.lower() uses full case mapping: U+0130 becomes "i" + U+0307, so
// the combining dot splits the token.
func TestTokensFullCaseMapping(t *testing.T) {
	got := chatmemory.Tokens("İstanbul")
	if _, ok := got["stanbul"]; !ok || len(got) != 1 {
		t.Fatalf("got %v, want only stanbul", got)
	}
}

func TestNamespace(t *testing.T) {
	seven := int32(7)
	if g, err := chatmemory.Namespace("ws-a", nil); err != nil || g != "ws-a:self-host" {
		t.Fatal(g, err)
	}
	if g, err := chatmemory.Namespace("ws-a", &seven); err != nil || g != "ws-a:7" {
		t.Fatal(g, err)
	}
	if g, err := chatmemory.Namespace("", &seven); err == nil || g != "" {
		t.Fatal("empty workspace must error", g, err)
	}
}

func TestAvailable(t *testing.T) {
	if !chatmemory.Available() {
		t.Fatal("must always be available")
	}
}
