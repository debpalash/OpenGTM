package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// LLMSettings are the knobs of the Go LLM client (internal/llm), named as in
// leadgen/llm.py. The client is a library: there is no enabling flag, its
// consumers are gated individually.
type LLMSettings struct {
	HTTPTimeout time.Duration // LLM_HTTP_TIMEOUT seconds per provider request (default 60)
	PromptCache bool          // LLM_PROMPT_CACHE: Anthropic cache_control on the system prompt (default on)
}

// LLMFrom reads the LLM client settings. LLM_HTTP_TIMEOUT is a positive
// number of seconds (fractions allowed, Python's float()). LLM_PROMPT_CACHE is
// on unless it is 0/false/no/off, exactly as the Python client reads it.
func LLMFrom(get func(string) string) (LLMSettings, error) {
	s := LLMSettings{HTTPTimeout: 60 * time.Second, PromptCache: true}
	if v := strings.TrimSpace(get("LLM_HTTP_TIMEOUT")); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f <= 0 || f > 3600 {
			return s, fmt.Errorf("config: LLM_HTTP_TIMEOUT=%q: want seconds in (0, 3600]", v)
		}
		s.HTTPTimeout = time.Duration(f * float64(time.Second))
	}
	switch strings.ToLower(strings.TrimSpace(get("LLM_PROMPT_CACHE"))) {
	case "0", "false", "no", "off":
		s.PromptCache = false
	}
	return s, nil
}
