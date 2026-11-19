package workbook

import (
	"strings"
	"testing"
)

func TestClassifyProviderError(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"http_429", "rate_limited"},
		{"Rate limit exceeded", "rate_limited"},
		{"browserclient: browser service unreachable", "browser_unavailable"},
		{"browserclient: browser service not configured", "browser_unavailable"},
		{"browserclient: browser service at capacity", "browser_unavailable"},
		{"http_500", ""},
		{"no_data", ""},
		{"", ""},
	} {
		if got := classifyProviderError(tc.in); got != tc.want {
			t.Errorf("%q: got %q want %q", tc.in, got, tc.want)
		}
	}
}

func TestFailureReasonTexts(t *testing.T) {
	if got := failureReason("rate_limited", []string{"a", "b"}); !strings.HasPrefix(got, "rate_limited:") || !strings.Contains(got, "a, b") {
		t.Fatal(got)
	}
	if got := failureReason("browser_unavailable", []string{"deep_scraper"}); !strings.HasPrefix(got, "browser_service_unavailable:") {
		t.Fatal(got)
	}
}
