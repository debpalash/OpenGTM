package signals_test

import (
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/signals"
)

func TestMomentumRoundsHalfEven(t *testing.T) {
	cases := []struct {
		total, prev int64
		want        float64
	}{{5, 2, 150}, {1, 8, -87.5}, {1, 16, -93.8}, {3, 16, -81.2}, {7, 3, 133.3}}
	for _, c := range cases {
		got := signals.Momentum(c.total, c.prev)
		if got == nil || *got != c.want {
			t.Errorf("Momentum(%d,%d) = %v want %v", c.total, c.prev, got, c.want)
		}
	}
	if signals.Momentum(5, 0) != nil {
		t.Error("prev 0 must be nil")
	}
}
