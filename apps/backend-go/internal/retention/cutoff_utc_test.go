package retention

import (
	"testing"
	"time"
)

func TestCutoffDateUsesUTC(t *testing.T) {
	loc := time.FixedZone("UTC+10", 10*3600)
	// 2026-06-16 05:00 local == 2026-06-15 19:00 UTC; minus 1 day => 2026-06-14 UTC.
	now := time.Date(2026, 6, 16, 5, 0, 0, 0, loc)
	var tgt Target
	for _, x := range Targets() {
		if x.Kind == KindDate {
			tgt = x
		}
	}
	if got := Cutoff(tgt, 1, now); got != "2026-06-14" {
		t.Fatalf("cutoff %v, want 2026-06-14", got)
	}
}
