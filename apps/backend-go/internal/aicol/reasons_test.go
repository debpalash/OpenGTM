package aicol

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/llm"
)

// failingLLM fails every call with err, like a client with no key (llm.ErrNoProvider) or with only failing keys.
type failingLLM struct{ err error }

func (f failingLLM) Complete(context.Context, llm.CompleteRequest) (string, error) { return "", f.err }
func (f failingLLM) ExtractJSON(context.Context, string, string, int) (map[string]any, error) {
	return nil, f.err
}
func (f failingLLM) AnthropicProvider(context.Context) (llm.Provider, bool) { return llm.Provider{}, false }
func (f failingLLM) AnthropicTurn(context.Context, llm.AnthropicRequest) (*llm.AnthropicResponse, error) {
	return nil, f.err
}
func (f failingLLM) BatchComplete(context.Context, []llm.BatchRequest, llm.BatchOptions) (map[string]string, error) {
	return nil, f.err
}

const noKeyReason = "No LLM provider is configured: add a key in Settings"

func TestResearchWithoutLLMProviderSaysSo(t *testing.T) {
	d := Deps{LLM: failingLLM{llm.ErrNoProvider}, Settings: mapSettings(nil)}
	res, err := Research(context.Background(), d, ResearchInput{Template: "Where is Acme?", Row: map[string]any{}, MaxSteps: 3})
	if err != nil {
		t.Fatal(err)
	}
	if res.Error != noKeyReason {
		t.Fatalf("error %q, want %q", res.Error, noKeyReason)
	}
}

func TestAIFormulaWithoutLLMProviderSaysSo(t *testing.T) {
	d := Deps{LLM: failingLLM{llm.ErrNoProvider}, Settings: mapSettings(nil)}
	res, err := AIFormula(context.Background(), d, "Describe it", map[string]any{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Error != noKeyReason {
		t.Fatalf("error %q, want %q", res.Error, noKeyReason)
	}
}

func TestLLMFailureReasonWhenEveryProviderFailed(t *testing.T) {
	err := fmt.Errorf("%w: %w", llm.ErrAllFailed, fmt.Errorf("openai: HTTP 401 secret-detail"))
	d := Deps{LLM: failingLLM{err}, Settings: mapSettings(nil)}
	res, _ := AIFormula(context.Background(), d, "Describe it", map[string]any{}, nil)
	if !strings.HasPrefix(res.Error, "The LLM provider call failed") || strings.Contains(res.Error, "secret-detail") {
		t.Fatalf("error %q: must say the call failed without echoing provider text", res.Error)
	}
}
