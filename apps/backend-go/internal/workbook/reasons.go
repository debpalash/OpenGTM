package workbook

import (
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/cooldown"
)

// What a user reads in a cell that stayed empty. The cell's error field carries the reason (the same field the
// engine already uses for no_data, timeout, providers_unavailable); cell_metadata.attempts lists, per provider, what
// was tried and how it ended.

// attemptNote is one provider call that produced no value.
type attemptNote struct {
	Provider string `json:"provider"`
	Outcome  string `json:"outcome"` // miss | error | timeout
	Error    string `json:"error,omitempty"`
}

// classifyProviderError names the cause a provider error text points at: rate_limited, browser_unavailable, or ""
// when it is an ordinary failure.
func classifyProviderError(errText string) string {
	if errText == "" {
		return ""
	}
	if cooldown.LooksRateLimited(errText) {
		return "rate_limited"
	}
	if strings.Contains(strings.ToLower(errText), "browser service") || strings.Contains(strings.ToLower(errText), "browser not available") {
		return "browser_unavailable"
	}
	if lower := strings.ToLower(errText); strings.Contains(lower, "key configured") || strings.Contains(lower, "no api key") {
		return "needs_key"
	}
	return ""
}

// failureReason is the cell error for a classified cause and the providers it hit.
func failureReason(cause string, providers []string) string {
	list := strings.Join(providers, ", ")
	switch cause {
	case "rate_limited":
		return "rate_limited: " + list + " answered with a rate limit and is paused for a few minutes"
	case "browser_unavailable":
		return "browser_service_unavailable: " + list + " needs the browser service, which could not be reached; check that it is running"
	case "needs_key":
		return "provider_key_missing: " + list + " needs an API key; add it in Settings"
	}
	return ""
}

// emptyCellReason is the error of a cell that no provider filled, from what the attempts say: "" when every
// provider simply found nothing (the cell is then plain no_data), a needs-setup reason when each provider that ran
// needed a key, and provider_error when a provider failed (an HTTP 5xx, an auth failure, a crash), because then
// "nothing found" would be a guess.
func emptyCellReason(notes []attemptNote) string {
	var failedProviders []string
	allNeedKey := true
	first := ""
	for _, n := range notes {
		if n.Outcome != "error" {
			allNeedKey = false
			continue
		}
		failedProviders = append(failedProviders, n.Provider)
		if classifyProviderError(n.Error) != "needs_key" {
			allNeedKey = false
			if first == "" {
				first = n.Provider + ": " + n.Error
			}
		}
	}
	if len(failedProviders) == 0 {
		return ""
	}
	if allNeedKey {
		return failureReason("needs_key", failedProviders)
	}
	return "provider_error: " + first
}

// coolingDownReason is the cell error when every selected provider was skipped.
func coolingDownReason(parts []string, allCooldown bool) string {
	msg := "providers_unavailable: " + strings.Join(parts, ", ")
	if allCooldown {
		msg += " - provider cooling down after a rate limit or repeated timeouts; it is retried automatically in a few minutes"
	}
	return msg
}

// hasNonMiss reports whether any attempt ended in an error or a timeout rather than a plain miss.
func hasNonMiss(notes []attemptNote) bool {
	for _, n := range notes {
		if n.Outcome != "miss" {
			return true
		}
	}
	return false
}
