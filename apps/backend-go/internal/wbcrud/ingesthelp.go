package wbcrud

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Exported helpers for the inbound-rows route (internal/api/ingestroutes.go); they expose the add-rows
// identity scan and the Python truthiness/str() rules without duplicating them.

// ExistingIdentities returns the dedupe identities of every row already in the workbook.
func ExistingIdentities(ctx context.Context, tx pgx.Tx, workbookID string) (map[string]struct{}, error) {
	return existingIdentities(ctx, tx, workbookID)
}

// Truthy is Python truthiness of a decoded JSON value.
func Truthy(v any) bool { return truthy(v) }

// PyStr is Python str() of a decoded JSON value.
func PyStr(v any) string { return pyStr(v) }
