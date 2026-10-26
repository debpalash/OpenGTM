//go:build integration

package wbcrud_test

import "github.com/jackc/pgx/v5/pgtype"

func text(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }
