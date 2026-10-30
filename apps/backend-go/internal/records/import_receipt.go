package records

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/jackc/pgx/v5"
)

// ImportReceipt recovers a committed batch after the client loses its response.
// Missing receipts are unknown, never evidence that an import failed.
func (s Store) ImportReceipt(ctx context.Context, ws, key string) (json.RawMessage, error) {
	var result json.RawMessage
	err := tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT result FROM record_imports WHERE workspace_id=$1 AND idempotency_key=$2 AND result?'imported'`, ws, key).Scan(&result)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return result, err
}
