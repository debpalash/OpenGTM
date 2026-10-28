package queue

import (
	"testing"
	"time"
)

// A claim counts as dead after five minutes without a heartbeat, or after three missed heartbeats when the
// worker heartbeats less often than every 100 seconds.
func TestReaperStaleAfter(t *testing.T) {
	for _, c := range []struct{ hb, want time.Duration }{
		{0, DefaultStaleAfter},
		{10 * time.Second, DefaultStaleAfter},
		{100 * time.Second, DefaultStaleAfter},
		{200 * time.Second, 600 * time.Second},
		{time.Hour, 3 * time.Hour},
	} {
		if got := ReaperStaleAfter(c.hb); got != c.want {
			t.Errorf("ReaperStaleAfter(%v) = %v, want %v", c.hb, got, c.want)
		}
	}
}
