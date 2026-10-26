package outreachstore

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/emailnorm"
)

// Outcome is remove_suppression's result string.
type Outcome string

const (
	OutcomeOK       Outcome = "ok"
	OutcomeNotFound Outcome = "not_found"
	OutcomeLocked   Outcome = "locked"
)

// Suppression is one list_suppressions entry (Python dict key order).
type Suppression struct {
	ID        int     `json:"id"`
	Email     string  `json:"email"`
	Reason    string  `json:"reason"`
	Source    string  `json:"source"`
	Locked    bool    `json:"locked"`
	CreatedAt *string `json:"created_at"`
}

// IsSuppressed reports whether any suppression match key of email (the canonical
// form, plus the dot/plus-collapsed form for gmail) is on this workspace's list.
func IsSuppressed(ctx context.Context, tx pgx.Tx, wsID, email string) (bool, error) {
	keys := emailnorm.MatchKeys(email)
	if len(keys) == 0 {
		return false, nil
	}
	ok, err := gen.New(tx).OutreachSuppressionExists(ctx, gen.OutreachSuppressionExistsParams{WorkspaceID: wsID, Emails: keys})
	if err != nil {
		return false, fmt.Errorf("outreachstore: is suppressed: %w", err)
	}
	return ok, nil
}

// AddSuppression inserts the email idempotently under its store key (the collapsed form for gmail addresses, so every
// dotted or plus variant is blocked, whichever spelling was suppressed) and reports whether a row was added. If any
// match key is already suppressed (including rows stored dotted before collapsing) nothing is written, so a locked
// suppression is never downgraded; an email that does not normalize is not added.
func AddSuppression(ctx context.Context, tx pgx.Tx, wsID, email, reason, source string, locked bool) (bool, error) {
	key := emailnorm.StoreKey(email)
	if key == "" {
		return false, nil
	}
	if dup, err := IsSuppressed(ctx, tx, wsID, email); err != nil || dup {
		return false, err
	}
	added, err := gen.New(tx).OutreachSuppressionAdd(ctx, gen.OutreachSuppressionAddParams{
		WorkspaceID: wsID, Email: key, Reason: reason, Source: source, Locked: locked,
	})
	if err != nil {
		return false, fmt.Errorf("outreachstore: add suppression: %w", err)
	}
	return added, nil
}

// ListSuppressions returns this workspace's suppressions, newest first.
func ListSuppressions(ctx context.Context, tx pgx.Tx, limit int) ([]Suppression, error) {
	rows, err := gen.New(tx).OutreachSuppressionList(ctx, gen.OutreachSuppressionListParams{
		WorkspaceID: currentWorkspace(ctx, tx), RowLimit: int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("outreachstore: list suppressions: %w", err)
	}
	out := make([]Suppression, 0, len(rows))
	for _, r := range rows {
		out = append(out, Suppression{
			ID: int(r.ID), Email: r.Email, Reason: r.Reason.String, Source: r.Source.String,
			Locked: r.Locked.Bool, CreatedAt: isoPy(r.CreatedAt),
		})
	}
	return out, nil
}

// RemoveSuppression deletes an unlocked suppression: ok, not_found or locked. Every match key of the address is
// looked up (the collapsed gmail row, and a row Python stored dotted), so either spelling removes it; a locked row
// among them refuses the whole removal.
func RemoveSuppression(ctx context.Context, tx pgx.Tx, wsID, email string) (Outcome, error) {
	keys := emailnorm.MatchKeys(email)
	if len(keys) == 0 { // Python looks up "" and finds nothing
		return OutcomeNotFound, nil
	}
	q := gen.New(tx)
	var ids []int32
	for _, k := range keys {
		row, err := q.OutreachSuppressionGet(ctx, gen.OutreachSuppressionGetParams{WorkspaceID: wsID, Email: k})
		if err == pgx.ErrNoRows {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("outreachstore: remove suppression: %w", err)
		}
		if row.Locked.Bool {
			return OutcomeLocked, nil
		}
		ids = append(ids, row.ID)
	}
	if len(ids) == 0 {
		return OutcomeNotFound, nil
	}
	for _, id := range ids {
		if _, err := q.OutreachSuppressionDelete(ctx, gen.OutreachSuppressionDeleteParams{WorkspaceID: wsID, ID: id}); err != nil {
			return "", fmt.Errorf("outreachstore: remove suppression: %w", err)
		}
	}
	return OutcomeOK, nil
}

// currentWorkspace reads the tenant GUC the transaction was opened with
// (ListSuppressions takes no workspace id, like list_suppressions).
func currentWorkspace(ctx context.Context, tx pgx.Tx) string {
	var ws string
	_ = tx.QueryRow(ctx, `SELECT COALESCE(current_setting('app.workspace_id', true), '')`).Scan(&ws)
	return ws
}
