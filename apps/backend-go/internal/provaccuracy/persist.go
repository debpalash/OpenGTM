package provaccuracy

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Persist is persist_scores: write each provider's correctness score onto its provider_stats rows (every
// (provider, field) row, plus a provider-level row keyed field='*' so the prior exists for fields the provider
// has not been called on yet). Returns the number of rows touched. provider_stats is a global table (no
// workspace scope); the caller owns the transaction.
func Persist(ctx context.Context, tx pgx.Tx, scores []*ProviderScore, now time.Time) (int, error) {
	at := now.UTC()
	touched := 0
	for _, ps := range scores {
		// The Python column defaults are client-side, so the wildcard row is inserted with explicit zeros.
		if _, err := tx.Exec(ctx, `INSERT INTO provider_stats
  (provider, field, attempts, hits, total_confidence, total_latency_ms, total_cost_usd, accuracy_samples)
VALUES ($1, '*', 0, 0, 0.0, 0.0, 0.0, 0)
ON CONFLICT (provider, field) DO NOTHING`, ps.Provider); err != nil {
			return touched, fmt.Errorf("provaccuracy: wildcard row: %w", err)
		}
		tag, err := tx.Exec(ctx, `UPDATE provider_stats
SET accuracy_score = $2, accuracy_samples = $3, accuracy_updated_at = $4, updated_at = now()
WHERE provider = $1`, ps.Provider, ps.Score(), int32(ps.Asserted), at)
		if err != nil {
			return touched, fmt.Errorf("provaccuracy: update: %w", err)
		}
		touched += int(tag.RowsAffected())
	}
	return touched, nil
}
