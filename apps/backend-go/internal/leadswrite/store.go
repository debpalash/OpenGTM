// Package leadswrite is the write half of PgLeadStore (store.py): create-only
// insert, status update, allow-listed field update and delete. `leads` is
// FORCE RLS, so every call takes the transaction of tenancy.InWorkspace.
package leadswrite

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// NewLead is the AddLeadRequest field set; all other columns take the Python
// Lead dataclass defaults.
type NewLead struct {
	Company, City, Website, Email, Phone, Specialization, ContactPerson, LinkedinURL, Notes, Source string
}

// ExistsError is LeadAlreadyExistsError: the (workspace, company, city) key is
// taken by LeadID and nothing was modified.
type ExistsError struct{ LeadID int64 }

func (e *ExistsError) Error() string {
	return "lead already exists: " + strconv.FormatInt(e.LeadID, 10)
}

// isoNow renders datetime.now(timezone.utc).isoformat(): microseconds only
// when non-zero, "+00:00" offset.
func isoNow() string {
	t := time.Now().UTC()
	if t.Nanosecond()/1000 == 0 {
		return t.Format("2006-01-02T15:04:05") + "+00:00"
	}
	return t.Format("2006-01-02T15:04:05.000000") + "+00:00"
}

// idOK reports whether id fits the int4 primary key; others cannot exist.
func idOK(id int64) bool { return id >= 1 && id <= math.MaxInt32 }

// Create inserts a lead forced into workspaceID. A collision on
// (workspace_id, company, city) returns *ExistsError carrying the winner's id,
// including when concurrent creators race (ON CONFLICT DO NOTHING waits for the
// winner's commit, then the winner is selected).
func Create(ctx context.Context, tx pgx.Tx, workspaceID string, n NewLead) (int64, error) {
	q := gen.New(tx)
	now := isoNow()
	id, err := q.InsertLeadCreateOnly(ctx, gen.InsertLeadCreateOnlyParams{
		WorkspaceID: workspaceID, Company: n.Company, City: n.City, Website: n.Website, Email: n.Email,
		Phone: n.Phone, Specialization: n.Specialization, ContactPerson: n.ContactPerson,
		LinkedinUrl: n.LinkedinURL, Notes: n.Notes, Source: n.Source, CreatedAt: now, UpdatedAt: now,
	})
	if err == nil {
		return int64(id), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("leadswrite: insert: %w", err)
	}
	winner, err := q.LeadIDByKey(ctx, gen.LeadIDByKeyParams{WorkspaceID: workspaceID, Company: n.Company, City: n.City})
	if err != nil {
		return 0, fmt.Errorf("leadswrite: select winner: %w", err)
	}
	return 0, &ExistsError{LeadID: int64(winner)}
}

// UpdateStatus sets status and advances updated_at. found is false for an
// unknown or foreign id. Python's note argument is ignored.
func UpdateStatus(ctx context.Context, tx pgx.Tx, id int64, status string) (bool, error) {
	if !idOK(id) {
		return false, nil
	}
	n, err := gen.New(tx).UpdateLeadStatus(ctx, gen.UpdateLeadStatusParams{Status: status, UpdatedAt: isoNow(), ID: int32(id)})
	if err != nil {
		return false, fmt.Errorf("leadswrite: status: %w", err)
	}
	return n > 0, nil
}

// UpdateFields applies the allow-listed subset of fields. Unknown and immutable
// keys are dropped; if nothing remains nothing is written (no updated_at bump)
// and found reports existence. A value Postgres would reject returns
// *FieldError before any write.
func UpdateFields(ctx context.Context, tx pgx.Tx, id int64, fields map[string]any) (bool, error) {
	if !idOK(id) {
		return false, nil
	}
	set, args, err := buildUpdate(fields)
	if err != nil {
		return false, err
	}
	if set == "" {
		ok, err := gen.New(tx).LeadExists(ctx, int32(id))
		if err != nil {
			return false, fmt.Errorf("leadswrite: exists: %w", err)
		}
		return ok, nil
	}
	// set only names allow-listed columns (see buildUpdate), never caller text.
	sql := "UPDATE leads SET " + set +
		" WHERE workspace_id = current_setting('app.workspace_id', true) AND id = $1"
	tag, err := tx.Exec(ctx, sql, append([]any{int32(id), isoNow()}, args...)...)
	if err != nil {
		return false, fmt.Errorf("leadswrite: update: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// Delete removes the lead row only (no FK references leads).
func Delete(ctx context.Context, tx pgx.Tx, id int64) (bool, error) {
	if !idOK(id) {
		return false, nil
	}
	n, err := gen.New(tx).DeleteLeadByID(ctx, int32(id))
	if err != nil {
		return false, fmt.Errorf("leadswrite: delete: %w", err)
	}
	return n > 0, nil
}
