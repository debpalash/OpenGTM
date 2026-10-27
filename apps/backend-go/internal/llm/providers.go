package llm

import (
	"context"
	"strings"
)

// Settings reads one setting (workspace secret, global setting or environment,
// whichever the caller's resolver implements); empty or unset yields def.
// settings.App satisfies it (global settings, as Python's _db_get did);
// WorkspaceSettings adds the per-workspace level.
type Settings interface {
	Get(ctx context.Context, key, def string) (string, error)
}

// providerSpec is one entry of leadgen/llm.py PROVIDER_CONFIG.
type providerSpec struct {
	ID           string
	EnvKey       string
	EnvURL       string
	EnvModel     string
	DefaultURL   string
	DefaultModel string
	TokenParam   string
	Native       bool // true: Anthropic Messages API instead of OpenAI chat completions
}

// ProviderOrder is the failover priority. Anthropic is last so the free tier is
// never displaced as a fallback; it only leads when it is the default.
var ProviderOrder = []string{
	"cerebras", "groq", "sambanova", "nvidia",
	"mistral", "openrouter", "github_models", "siliconflow",
	"anthropic",
}

const (
	// AnthropicID is the provider id handled by the native Messages API.
	AnthropicID = "anthropic"
	// AnthropicDefaultModel is used when ANTHROPIC_MODEL is unset.
	AnthropicDefaultModel = "claude-opus-4-8"
	anthropicDefaultURL   = "https://api.anthropic.com"
	anthropicVersion      = "2023-06-01"
)

var providerSpecs = map[string]providerSpec{
	"anthropic":     {"anthropic", "ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL", anthropicDefaultURL, AnthropicDefaultModel, "max_tokens", true},
	"cerebras":      {"cerebras", "CEREBRAS_API_KEY", "CEREBRAS_BASE_URL", "CEREBRAS_MODEL", "https://api.cerebras.ai/v1", "llama-3.3-70b", "max_completion_tokens", false},
	"groq":          {"groq", "GROQ_API_KEY", "GROQ_BASE_URL", "GROQ_MODEL", "https://api.groq.com/openai/v1", "llama-3.3-70b-versatile", "max_tokens", false},
	"sambanova":     {"sambanova", "SAMBANOVA_API_KEY", "SAMBANOVA_BASE_URL", "SAMBANOVA_MODEL", "https://api.sambanova.ai/v1", "Meta-Llama-3.3-70B-Instruct", "max_tokens", false},
	"nvidia":        {"nvidia", "NVIDIA_API_KEY", "NVIDIA_BASE_URL", "NVIDIA_MODEL", "https://integrate.api.nvidia.com/v1", "meta/llama-3.3-70b-instruct", "max_tokens", false},
	"mistral":       {"mistral", "MISTRAL_API_KEY", "MISTRAL_BASE_URL", "MISTRAL_MODEL", "https://api.mistral.ai/v1", "mistral-small-latest", "max_tokens", false},
	"openrouter":    {"openrouter", "OPENROUTER_API_KEY", "OPENROUTER_BASE_URL", "OPENROUTER_MODEL", "https://openrouter.ai/api/v1", "meta-llama/llama-3.3-70b-instruct:free", "max_tokens", false},
	"github_models": {"github_models", "GITHUB_MODELS_API_KEY", "GITHUB_MODELS_BASE_URL", "GITHUB_MODELS_MODEL", "https://models.inference.ai.azure.com", "gpt-4o", "max_tokens", false},
	"siliconflow":   {"siliconflow", "SILICONFLOW_API_KEY", "SILICONFLOW_BASE_URL", "SILICONFLOW_MODEL", "https://api.siliconflow.cn/v1", "Qwen/Qwen3-8B", "max_tokens", false},
}

// Provider is a resolved completion provider (key, URL and model looked up).
type Provider struct {
	ID         string
	APIKey     string
	BaseURL    string
	Model      string
	TokenParam string
	Native     bool
}

// String never prints the key.
func (p Provider) String() string { return "llm.Provider{" + p.ID + "}" }

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func isTruthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
