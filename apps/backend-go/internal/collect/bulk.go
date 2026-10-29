package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

type bulkPayload struct {
	JobID       string        `json:"job_id"`
	IDs         []json.Number `json:"ids"`
	Action      string        `json:"action"`
	WorkspaceID string        `json:"workspace_id"`
	Slug        string        `json:"slug"`
}

// bulkFields are the columns scrape_website may write (handle_bulk_enrich).
// email_confidence and email_provider travel with a found email (review 3a L4); the website
// action writes only the contact fields (its social links are not stored: bulk enrich fills
// contact data, the LinkedIn/Twitter URLs come from collection and the waterfall).
var bulkFields = []string{"email", "email_confidence", "email_provider", "phone", "description", "contact_person"}

// HandleBulkEnrich is handle_bulk_enrich: "find_emails" runs enrich_emails over the leads,
// anything else runs the website contact scrape over the leads that have a website; changed
// fields are written with last_enriched_at, with bulk_enrich_started / _progress / _done (or
// _error) events.
//
// Fixes: legacy compared each lead with itself after enriching it in place, so neither action
// ever wrote a field (only the events ran); here the values are compared with a snapshot taken
// before enrichment. legacy also stored the literal text "now" in last_enriched_at; here it is
// the ISO timestamp.
func HandleBulkEnrich(ctx context.Context, d Deps, c *queue.Claim) error {
	var p bulkPayload
	ws, err := claimTenant(c, &p, &p.WorkspaceID, &p.Slug)
	if err != nil {
		return err
	}
	r := d.For(ws)
	em := d.Emit(ws)
	emit := func(typ string, f ...any) {
		if em != nil {
			em.Emit(ctx, typ, obj(f...))
		}
	}
	leads, err := bulkLeads(ctx, d, p)
	if err != nil {
		return err
	}
	emit("bulk_enrich_started", "job_id", p.JobID, "action", p.Action, "total", len(leads))
	done, updated := 0, 0
	write := func(l *Lead, fields map[string]string) error {
		if len(fields) == 0 {
			return nil
		}
		return tenancy.InWorkspace(ctx, d.Pool, ws, func(tx pgx.Tx) error {
			return updateBulkFields(ctx, tx, l.ID, fields)
		})
	}
	fail := func(err error) error {
		emit("bulk_enrich_error", "job_id", p.JobID, "error", truncRunes(err.Error(), 200))
		return err
	}
	if p.Action == "find_emails" {
		for _, l := range leads {
			if err := ctx.Err(); err != nil {
				return fail(err)
			}
			before := l.Email
			needed := !l.HasEmail() && l.Company != ""
			EnrichEmails(ctx, r.Web, r.Enrich, l)
			if l.Email != "" && l.Email != before {
				if err := write(l, map[string]string{"email": l.Email, "email_confidence": l.EmailConfidence,
					"email_provider": "email_finder"}); err != nil {
					return fail(err)
				}
				updated++
			}
			if needed {
				_ = sleepCtx(ctx, r.Sleep, 500*time.Millisecond)
			}
			done++
			emit("bulk_enrich_progress", "job_id", p.JobID, "done", done, "total", len(leads))
		}
	} else {
		var targets []*Lead
		for _, l := range leads {
			if l.Website != "" {
				targets = append(targets, l)
			}
		}
		snap := make([]Lead, len(targets))
		for i, l := range targets {
			snap[i] = *l
		}
		EnrichFromWebsites(ctx, r.Web, targets, 5)
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		for i, l := range targets {
			fields := map[string]string{}
			now, was := map[string]string{"email": l.Email, "phone": l.Phone, "description": l.Description,
				"contact_person": l.ContactPerson}, map[string]string{"email": snap[i].Email, "phone": snap[i].Phone,
				"description": snap[i].Description, "contact_person": snap[i].ContactPerson}
			for _, f := range bulkFields {
				if now[f] != "" && now[f] != was[f] {
					fields[f] = now[f]
				}
			}
			if len(fields) > 0 {
				if err := write(l, fields); err != nil {
					return fail(err)
				}
				updated++
			}
			done++
			emit("bulk_enrich_progress", "job_id", p.JobID, "done", done, "total", len(targets))
		}
	}
	emit("bulk_enrich_done", "job_id", p.JobID, "updated", updated, "total", len(leads))
	return nil
}

func sleepCtx(ctx context.Context, s func(context.Context, time.Duration) error, d time.Duration) error {
	if s != nil {
		return s(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// bulkLeads loads the requested leads of the workspace in request order (missing ids skipped).
func bulkLeads(ctx context.Context, d Deps, p bulkPayload) ([]*Lead, error) {
	// ids are arbitrary-precision integers in the payload (pydantic int); one outside the int4 key
	// range names no lead (legacy get_lead returned None).
	var want []int64
	ids := make([]int32, 0, len(p.IDs))
	for _, n := range p.IDs {
		id, err := n.Int64()
		if err == nil && id > 0 && id <= 1<<31-1 {
			ids = append(ids, int32(id))
			want = append(want, id)
		}
	}
	byID := map[int64]*Lead{}
	err := tenancy.InWorkspace(ctx, d.Pool, p.WorkspaceID, func(tx pgx.Tx) error {
		rows, err := gen.New(tx).CollectLeadsForBulk(ctx, ids)
		for _, r := range rows {
			byID[int64(r.ID)] = &Lead{ID: int64(r.ID), Company: r.Company, Website: r.Website.String,
				Email: r.Email.String, EmailConfidence: r.EmailConfidence.String, Phone: r.Phone.String,
				ContactPerson: r.ContactPerson.String, City: r.City.String, Description: r.Description.String,
				LinkedinURL: r.LinkedinUrl.String, TwitterURL: r.TwitterUrl.String}
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	var out []*Lead
	for _, id := range want {
		if l, ok := byID[id]; ok {
			out = append(out, l)
		}
	}
	return out, nil
}

// updateBulkFields is update_lead_fields(id, {...fields, "last_enriched_at": now}) plus updated_at.
func updateBulkFields(ctx context.Context, tx pgx.Tx, id int64, fields map[string]string) error {
	now := nowISO()
	sets := []string{"updated_at = $2", "last_enriched_at = $2"}
	args := []any{int32(id), now}
	for _, f := range bulkFields { // fixed column names, never caller text
		if v, ok := fields[f]; ok {
			args = append(args, v)
			sets = append(sets, fmt.Sprintf("%s = $%d", f, len(args)))
		}
	}
	_, err := tx.Exec(ctx, "UPDATE leads SET "+strings.Join(sets, ", ")+
		" WHERE workspace_id = current_setting('app.workspace_id', true) AND id = $1", args...)
	return err
}
