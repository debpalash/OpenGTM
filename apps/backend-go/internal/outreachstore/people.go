package outreachstore

import (
	"context"
	"errors"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/emailnorm"
	"github.com/jackc/pgx/v5"
	"time"
)

// EnrollPerson retains the enrollment ID, recipient snapshot, consent and send
// idempotency contract used by existing campaigns. It also finds historical
// enrollments made through any legacy lead alias of the same person.
func EnrollPerson(ctx context.Context, tx pgx.Tx, ws, seq, pid, source string, consent time.Time) (int, bool, error) {
	if _, e := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, ws+"|enroll|"+seq+"|"+pid); e != nil {
		return 0, false, e
	}
	var email string
	var cid *string
	e := tx.QueryRow(ctx, `SELECT coalesce(accepted_fields->>'email',''),company_entity_id FROM person_entities WHERE workspace_id=$1 AND id=$2`, ws, pid).Scan(&email, &cid)
	if e != nil {
		return 0, false, e
	}
	var id int
	e = tx.QueryRow(ctx, `SELECT id FROM outreach_enrollments WHERE workspace_id=$1 AND sequence_id=$2 AND person_entity_id=$3 ORDER BY id LIMIT 1`, ws, seq, pid).Scan(&id)
	if e == nil {
		return id, false, nil
	}
	if !errors.Is(e, pgx.ErrNoRows) {
		return 0, false, e
	}
	email = emailnorm.Normalize(email)
	if email == "" {
		return 0, false, ErrNoEmail
	}
	suppressed, e := IsSuppressed(ctx, tx, ws, email)
	if e != nil {
		return 0, false, e
	}
	if suppressed {
		return 0, false, ErrPersonSuppressed
	}
	e = tx.QueryRow(ctx, `INSERT INTO outreach_enrollments(workspace_id,sequence_id,person_entity_id,company_entity_id,to_email_snapshot,consent_source,consent_at,current_step,status,next_send_at,sent_count,soft_bounce_count) VALUES($1,$2,$3,$4,$5,$6,$7,0,'pending',now(),0,0) RETURNING id`, ws, seq, pid, cid, email, source, consent).Scan(&id)
	return id, e == nil, e
}

var ErrPersonSuppressed = errors.New("suppressed")
