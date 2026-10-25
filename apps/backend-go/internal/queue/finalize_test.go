package queue

import "testing"

func TestBackoffMinutes(t *testing.T) {
	cases := map[int]int32{0: 1, 1: 2, 5: 32, 30: 1 << 30, 31: 1 << 30, 1000: 1 << 30, -1: 1}
	for in, want := range cases {
		if got := backoffMinutes(in); got != want {
			t.Errorf("backoffMinutes(%d)=%d want %d", in, got, want)
		}
	}
}
