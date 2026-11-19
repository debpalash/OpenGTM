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
	}
	return ""
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
