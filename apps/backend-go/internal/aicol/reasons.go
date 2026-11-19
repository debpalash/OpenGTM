package aicol

import (
	"errors"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/llm"
)

// Cell errors for an LLM that cannot answer. They are stored in the cell's error field and shown to the user, so
// they say what to do and never echo provider text (it can carry request details).
const (
	reasonNoLLMProvider = "No LLM provider is configured: add a key in Settings"
	reasonLLMFailed     = "The LLM provider call failed: check the key and quota in Settings"
)

// LLMFailureReason is the user-readable cell error for an LLM failure that retrying will not fix, or "" when
// the failure has no such cause (an empty answer, a spend stop, ...) and the caller keeps its generic result.
func LLMFailureReason(err error) string {
	switch {
	case errors.Is(err, llm.ErrNoProvider):
		return reasonNoLLMProvider
	case errors.Is(err, llm.ErrAllFailed):
		return reasonLLMFailed
	}
	return ""
}
