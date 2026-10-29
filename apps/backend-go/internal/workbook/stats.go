package workbook

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/vendorcatalog"
)

// Planner constants from planner.py.
const (
	cooldownSeconds        = 300
	timeoutCooldownSeconds = 180
	correctnessWeight      = 0.6
)

// skippedProvider is a selected provider that never ran, with the reason.
type skippedProvider struct{ Provider, Reason string }

// planner ports the read side of planner.py. Provider economics come from the
// payload (paid <=> cost > 0), not a vendor catalog.
type planner struct {
	costs map[string]float64
	// catalog falls back to the vendor catalog for providers the payload carries
	// no cost for (non-declarative providers behind the provider runner).
	catalog bool
}

func (p planner) cost(name string) float64 {
	if c, ok := p.costs[name]; ok || !p.catalog {
		return c
	}
	return vendorcatalog.BaseCost(name)
}
func (p planner) paid(name string) bool { return p.cost(name) > 0 }

// knownCost ports vendor_catalog.has_known_cost: the generator emits a cost
// for every manifest it routes here, so presence is the signal.
func (p planner) knownCost(name string) bool {
	_, ok := p.costs[name]
	return ok || (p.catalog && vendorcatalog.HasKnownCost(name))
}

func (p planner) stat(ctx context.Context, q *gen.Queries, provider, field string) (*gen.GetProviderStatRow, error) {
	st, err := q.GetProviderStat(ctx, gen.GetProviderStatParams{Provider: provider, Field: field})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// filterChain ports planner.filter_chain: cooldown and budget checks without
// changing the caller's provider order.
func (p planner) filterChain(ctx context.Context, q *gen.Queries, field string, chain []string,
	budgetRemaining *float64, skipped *[]skippedProvider) ([]string, error) {
	var usable []string
	for _, name := range chain {
		st, err := p.stat(ctx, q, name, field)
		if err != nil {
			return nil, err
		}
		if st != nil && st.InCooldown {
			*skipped = append(*skipped, skippedProvider{name, "cooldown"})
			continue
		}
		if budgetRemaining != nil && p.paid(name) && p.cost(name) > *budgetRemaining {
			*skipped = append(*skipped, skippedProvider{name, "over_budget"})
			continue
		}
		usable = append(usable, name)
	}
	return usable, nil
}

// correctnessPrior ports planner.correctness_prior.
func (p planner) correctnessPrior(ctx context.Context, q *gen.Queries, st *gen.GetProviderStatRow, provider string) (float64, error) {
	var score *float64
	if st != nil && st.AccuracyScore.Valid {
		score = &st.AccuracyScore.Float64
	} else {
		other, err := q.GetAnyProviderAccuracy(ctx, provider)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return 0, err
		}
		if err == nil && other.Valid {
			score = &other.Float64
		}
	}
	if score == nil {
		return 1.0, nil
	}
	return (1.0 - correctnessWeight) + correctnessWeight*max(0.0, min(1.0, *score)), nil
}

func (p planner) score(ctx context.Context, q *gen.Queries, provider, field string) (float64, error) {
	st, err := p.stat(ctx, q, provider, field)
	if err != nil {
		return 0, err
	}
	hitRate := 0.5
	if st != nil && st.Attempts.Valid && st.Attempts.Int32 >= 3 {
		hitRate = float64(st.Hits.Int32) / float64(st.Attempts.Int32)
	}
	prior, err := p.correctnessPrior(ctx, q, st, provider)
	if err != nil {
		return 0, err
	}
	return (hitRate / (p.cost(provider) + 0.001)) * prior, nil
}

// orderChain ports planner.order_chain: filter, then highest yield/cost first
// with ties keeping the original order.
func (p planner) orderChain(ctx context.Context, q *gen.Queries, field string, chain []string,
	budgetRemaining *float64, skipped *[]skippedProvider) ([]string, error) {
	usable, err := p.filterChain(ctx, q, field, chain, budgetRemaining, skipped)
	if err != nil {
		return nil, err
	}
	scores := make(map[string]float64, len(usable))
	for _, name := range usable {
		if scores[name], err = p.score(ctx, q, name, field); err != nil {
			return nil, err
		}
	}
	sort.SliceStable(usable, func(i, j int) bool { return scores[usable[i]] > scores[usable[j]] })
	return usable, nil
}

// plan runs the planner reads in one short workspace transaction.
func (p planner) plan(ctx context.Context, pool *pgxpool.Pool, workspaceID, field string, chain []string,
	explicit bool, budgetRemaining *float64) (usable []string, skipped []skippedProvider, err error) {
	err = tenancy.InWorkspace(ctx, pool, workspaceID, func(tx pgx.Tx) error {
		q := gen.New(tx)
		var e error
		if explicit {
			usable, e = p.filterChain(ctx, q, field, chain, budgetRemaining, &skipped)
		} else {
			usable, e = p.orderChain(ctx, q, field, chain, budgetRemaining, &skipped)
		}
		return e
	})
	return usable, skipped, err
}

// attemptRecord is one buffered provider attempt (provider_attempts in
// enrich_cell), flushed after the cell commits.
type attemptRecord struct {
	provider, field string
	success         bool
	confidence      float64
	latencyMS       float64
	rateLimited     bool
	timedOut        bool
}

// looksRateLimited ports planner.looks_rate_limited.
func looksRateLimited(err string) bool {
	e := strings.ToLower(err)
	for _, s := range []string{"429", "rate limit", "too many requests", "quota", "throttle"} {
		if strings.Contains(e, s) {
			return true
		}
	}
	return false
}

// recordAttempts ports the telemetry flush in enrich_cell: one short
// transaction after the cell commit; any failure is logged and swallowed so it
// can never erase a committed result.
func (p planner) recordAttempts(ctx context.Context, pool *pgxpool.Pool, workspaceID string, attempts []attemptRecord, log *slog.Logger) {
	if len(attempts) == 0 {
		return
	}
	err := tenancy.InWorkspace(ctx, pool, workspaceID, func(tx pgx.Tx) error {
		q := gen.New(tx)
		for _, a := range attempts {
			cd := int32(0)
			if a.rateLimited {
				cd = cooldownSeconds
			} else if a.timedOut {
				cd = timeoutCooldownSeconds
			}
			if err := q.RecordProviderAttempt(ctx, gen.RecordProviderAttemptParams{
				Provider: a.provider, Field: a.field, Success: a.success, Confidence: a.confidence,
				LatencyMs: a.latencyMS, CostUsd: p.cost(a.provider), CooldownSeconds: cd,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		log.Warn("provider telemetry write failed", "err", err)
	}
}
