package api

import (
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ratelimit"
)

// frozenLimiter is the shared limiter with a clock that never advances: a test that spends a window's budget gets its
// 429 however long the requests took. On a slow runner under -race, 20 bcrypt verifications alone outlast a one-minute
// window, and a wall-clock window that reopens mid-test turns the expected 429 into a 401.
func frozenLimiter() *ratelimit.Limiter {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return ratelimit.New(func() time.Time { return t0 })
}
