// Package cooldown is the one implementation of the provider cooldown policy, shared by the provider runner
// (internal/providerrun) and the workbook planner's telemetry flush (internal/workbook).
//
// Policy (planner.py COOLDOWN_SECONDS, with the timeout rule changed):
//   - a rate limit (429, quota, throttle) cools the provider and field down for RateLimitSeconds at once;
//   - a timeout cools it down for TimeoutSeconds only after TimeoutThreshold consecutive timeouts; any success or
//     miss (and a rate limit) resets the run. A timeout is as often our own queueing as the provider being slow, so
//     one of them is not evidence that the provider is down;
//   - the run counts events, not calls: a timeout extends it only when the call started at or after the finish of
//     the previously counted timeout. Twelve rows launched together that all time out started before any of them
//     failed, so they are one event. Three successive rounds of failure with no success in between are three. This
//     is the simplest rule that has the property "at least one success opportunity between counted timeouts": a
//     later round's calls were made knowing the earlier round had already failed;
//   - a provider that fetches the lead's own site (providers.ClassTargetSite) never starts a timeout cooldown: its
//     timeout is a property of that lead. Attempt.TargetSite carries that class from the caller, which reads it from
//     the provider registry, so this package keeps no list of provider names.
package cooldown

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

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
	// TargetSite is true when the provider's class is providers.ClassTargetSite.
	TargetSite bool
	// StartedAt and FinishedAt bracket the call. FinishedAt zero means now; StartedAt zero means "unknown", which
	// counts as a fresh call.
	StartedAt, FinishedAt time.Time
}

// Record writes the attempt to provider_stats in one atomic upsert, applying the policy above. q must run in the
// workspace's transaction (tenancy.InWorkspace).
func Record(ctx context.Context, q *gen.Queries, a Attempt) error {
	if a.TargetSite && a.TimedOut && !a.RateLimited {
		a.TimedOut = false // a property of one lead's site, not evidence about the provider
	}
	fin := a.FinishedAt
	if fin.IsZero() {
		fin = time.Now()
	}
	start := a.StartedAt
	if start.IsZero() {
		start = fin
	}
	return q.RecordProviderAttempt(ctx, gen.RecordProviderAttemptParams{
		Provider: a.Provider, Field: a.Field, Success: a.Success, Confidence: a.Confidence,
		LatencyMs: a.LatencyMS, CostUsd: a.CostUSD,
		RateLimited: a.RateLimited, TimedOut: a.TimedOut,
		StartedAt: ts(start), FinishedAt: ts(fin),
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

func ts(t time.Time) pgtype.Timestamp { return pgtype.Timestamp{Time: t.UTC(), Valid: true} }
