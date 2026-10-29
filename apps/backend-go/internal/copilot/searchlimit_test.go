package copilot

import "testing"

// search_leads is a safe tool any member (viewer included) can drive, so the model-supplied limit is
// clamped to MaxSearchLeadsLimit (a Go-only hardening; Python is uncapped).
func TestSearchLeadsLimitIsClamped(t *testing.T) {
	for in, want := range map[int64]int{0: 0, 1: 1, 10: 10, 100: 100, 101: 100, 1 << 40: 100} {
		if got := clampSearchLimit(in); got != want {
			t.Errorf("clampSearchLimit(%d)=%d want %d", in, got, want)
		}
	}
}
