package config

import (
	"testing"
	"time"
)

func TestLLMFrom(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	s, err := LLMFrom(env(nil))
	if err != nil || s.HTTPTimeout != 60*time.Second || !s.PromptCache {
		t.Fatalf("defaults: %+v %v", s, err)
	}
	s, err = LLMFrom(env(map[string]string{"LLM_HTTP_TIMEOUT": "2.5", "LLM_PROMPT_CACHE": " Off "}))
	if err != nil || s.HTTPTimeout != 2500*time.Millisecond || s.PromptCache {
		t.Fatalf("set: %+v %v", s, err)
	}
	// Anything but 0/false/no/off keeps the cache on (Python semantics).
	if s, _ = LLMFrom(env(map[string]string{"LLM_PROMPT_CACHE": "maybe"})); !s.PromptCache {
		t.Fatal("cache must stay on")
	}
	for _, bad := range []string{"abc", "0", "-3", "99999"} {
		if _, err := LLMFrom(env(map[string]string{"LLM_HTTP_TIMEOUT": bad})); err == nil {
			t.Fatalf("LLM_HTTP_TIMEOUT=%q accepted", bad)
		}
	}
}
