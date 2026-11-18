// Package cooldown is the one implementation of the provider cooldown policy, shared by the provider runner
// (internal/providerrun) and the workbook planner's telemetry flush (internal/workbook).
//
// Policy (planner.py COOLDOWN_SECONDS, with the timeout rule changed):
//   - a rate limit (429, quota, throttle) cools the provider and field down for RateLimitSeconds at once;
//   - a timeout cools it down for TimeoutSeconds only after TimeoutThreshold consecutive timeouts; any success or
//     miss (and a rate limit) resets the run. A timeout is as often our own queueing as the provider being slow, so
//     one of them is not evidence that the provider is down.
package cooldown

import (
	"context"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

const (
	RateLimitSeconds = 300
	TimeoutSeconds   = 180
	TimeoutThreshold = 3
)

// Attempt is one finished provider call.
type Attempt struct {
	Provider, Field string
	Success         bool
	Confidence      float64
	LatencyMS       float64
	CostUSD         float64
	RateLimited     bool
	TimedOut        bool
}

// Record writes the attempt to provider_stats in one atomic upsert, applying the policy above. q must run in the
// workspace's transaction (tenancy.InWorkspace).
func Record(ctx context.Context, q *gen.Queries, a Attempt) error {
	return q.RecordProviderAttempt(ctx, gen.RecordProviderAttemptParams{
		Provider: a.Provider, Field: a.Field, Success: a.Success, Confidence: a.Confidence,
		LatencyMs: a.LatencyMS, CostUsd: a.CostUSD,
		RateLimited: a.RateLimited, TimedOut: a.TimedOut,
		RateLimitSeconds: RateLimitSeconds, TimeoutSeconds: TimeoutSeconds, TimeoutThreshold: TimeoutThreshold,
	})
}

// LooksRateLimited ports planner.looks_rate_limited: does a provider error text read like a rate limit?
func LooksRateLimited(err string) bool {
	e := strings.ToLower(err)
	for _, s := range []string{"429", "rate limit", "too many requests", "quota", "throttle"} {
		if strings.Contains(e, s) {
			return true
		}
	}
	return false
}
