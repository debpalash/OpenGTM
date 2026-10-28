package records

import (
	"errors"
	"github.com/jackc/pgx/v5/pgconn"
)

// PublicError never exposes SQL, bound data, constraint internals or credentials.
func PublicError(err error) string {
	if errors.Is(err, ErrConflict) {
		return "identity or version conflict; refresh the record and review identifiers"
	}
	if errors.Is(err, ErrNotFound) {
		return "record not found in this workspace"
	}
	var p *pgconn.PgError
	if errors.As(err, &p) {
		switch p.Code {
		case "23505", "40001":
			return "identity or version conflict; review email, profile, domain and source IDs"
		case "P0002":
			return "linked record not found in this workspace"
		case "22023":
			return p.Message
		case "42501":
			return "workspace access denied"
		default:
			return "record operation failed"
		}
	}
	return err.Error()
}
