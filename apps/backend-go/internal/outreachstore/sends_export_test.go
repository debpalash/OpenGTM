package outreachstore

import "time"

// SetSendsClockForTest swaps the clock used by the sent-count windows and
// returns a restore func.
func SetSendsClockForTest(f func() time.Time) func() {
	prev := sendsNow
	sendsNow = f
	return func() { sendsNow = prev }
}
