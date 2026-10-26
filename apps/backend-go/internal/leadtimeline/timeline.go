// Package leadtimeline builds the unified, tenant-scoped account activity feed
// (port of lead_timeline in routers/leads.py).
package leadtimeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
)

// Item is one timeline entry. JSON key order is id, kind, title, description,
// status, occurred_at, metadata. Title, Description and Status are pointers so a
// NULL column renders null like Python (signals and outreach rows may carry
// NULLs). Metadata is one of the ordered *Meta structs below.
type Item struct {
	ID          string
	Kind        string
	Title       *string
	Description *string
	Status      *string
	OccurredAt  float64
	Metadata    any
}

type SignalMeta struct {
	Source    *string `json:"source"`
	SourceURL *string `json:"source_url"`
	Weight    *int32  `json:"weight"`
}

type AudienceMeta struct {
	AudienceID   string `json:"audience_id"`
	AudienceName string `json:"audience_name"`
}

type ActivationMeta struct {
	DestinationID   string `json:"destination_id"`
	DestinationType string `json:"destination_type"`
	Operation       string `json:"operation"`
}

type OutreachMeta struct {
	SequenceID *string `json:"sequence_id"`
	StepNumber *int32  `json:"step_number"`
}

type LeadMeta struct{}

// Page is one keyset page; NextBefore/NextBeforeID are set only when more items exist.
type Page struct {
	Items        []Item
	NextBefore   *float64
	NextBeforeID *string
}

func enc(v any) []byte {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	_ = e.Encode(v)
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

// pyFloat renders like Python json.dumps on a float (repr, NaN/Infinity allowed).
func pyFloat(f float64) string {
	if s, err := canonjson.FloatRepr(f); err == nil {
		return s
	}
	switch {
	case math.IsNaN(f):
		return "NaN"
	case f > 0:
		return "Infinity"
	}
	return "-Infinity"
}

func (i Item) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(`{"id":`)
	b.Write(enc(i.ID))
	b.WriteString(`,"kind":`)
	b.Write(enc(i.Kind))
	b.WriteString(`,"title":`)
	b.Write(enc(i.Title))
	b.WriteString(`,"description":`)
	b.Write(enc(i.Description))
	b.WriteString(`,"status":`)
	b.Write(enc(i.Status))
	b.WriteString(`,"occurred_at":` + pyFloat(i.OccurredAt))
	b.WriteString(`,"metadata":`)
	b.Write(enc(i.Metadata))
	b.WriteByte('}')
	return b.Bytes(), nil
}

func (p Page) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(`{"items":[`)
	for n, it := range p.Items {
		if n > 0 {
			b.WriteByte(',')
		}
		ib, _ := it.MarshalJSON()
		b.Write(ib)
	}
	b.WriteString(`],"next_before":`)
	if p.NextBefore == nil {
		b.WriteString("null")
	} else {
		b.WriteString(pyFloat(*p.NextBefore))
	}
	b.WriteString(`,"next_before_id":`)
	b.Write(enc(p.NextBeforeID))
	b.WriteByte('}')
	return b.Bytes(), nil
}

func sp(s string) *string { return &s }

func text(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	return &t.String
}

func nonEmpty(t pgtype.Text) bool { return t.Valid && t.String != "" }

func tsNaive(t pgtype.Timestamp) float64 {
	if !t.Valid || t.InfinityModifier != pgtype.Finite {
		return 0
	}
	return microsToFloat(t.Time.UnixMicro())
}

func tsTZ(t pgtype.Timestamptz) float64 {
	if !t.Valid || t.InfinityModifier != pgtype.Finite {
		return 0
	}
	return microsToFloat(t.Time.UnixMicro())
}

func epoch(f pgtype.Float8) float64 {
	if !f.Valid {
		return 0
	}
	return f.Float64
}

func leadTS(s *string) float64 {
	if s == nil {
		return 0
	}
	return ParseTimestamp(*s)
}

// Build assembles the page for lead. Run it inside tenancy.InWorkspace(workspaceID);
// every source is capped at 200 rows. limit must already be validated (1..200).
// Deviation: an outreach send with NULL step_number answers "Sequence step 1" and
// step_number null (Python raises TypeError and answers 500).
func Build(ctx context.Context, tx pgx.Tx, workspaceID string, lead leadsread.Lead, limit int, before *float64, beforeID string) (Page, error) {
	if limit < 1 {
		return Page{}, errors.New("leadtimeline: limit must be at least 1")
	}
	_ = workspaceID // tenancy comes from the transaction setting (RLS + explicit filters in SQL)
	q := gen.New(tx)
	id := int32(lead.ID)
	items := []Item{}

	sigs, err := q.TimelineSignals(ctx, id)
	if err != nil {
		return Page{}, err
	}
	for _, s := range sigs {
		title := text(s.Title)
		if !nonEmpty(s.Title) {
			title = text(s.SignalType)
		}
		var w *int32
		if s.Weight.Valid {
			w = &s.Weight.Int32
		}
		items = append(items, Item{
			ID: "signal:" + s.ID, Kind: "signal", Title: title, Description: text(s.Description),
			Status: text(s.SignalType), OccurredAt: epoch(s.CreatedAt),
			Metadata: SignalMeta{Source: text(s.Source), SourceURL: text(s.SourceUrl), Weight: w},
		})
	}

	evs, err := q.TimelineAudienceEvents(ctx, id)
	if err != nil {
		return Page{}, err
	}
	for _, e := range evs {
		items = append(items, Item{
			ID: "audience:" + itoa(e.ID), Kind: "audience", Title: sp(pyTitle(e.EventType) + " " + e.AudienceName),
			Description: sp("Audience membership " + e.EventType), Status: sp(e.EventType),
			OccurredAt: tsNaive(e.CreatedAt), Metadata: AudienceMeta{AudienceID: e.AudienceID, AudienceName: e.AudienceName},
		})
	}

	dels, err := q.TimelineDeliveries(ctx, id)
	if err != nil {
		return Page{}, err
	}
	for _, d := range dels {
		desc := "Destination delivery"
		if nonEmpty(d.Summary) {
			desc = d.Summary.String
		} else if nonEmpty(d.Error) {
			desc = d.Error.String
		}
		when := tsNaive(d.DeliveredAt)
		if !d.DeliveredAt.Valid {
			when = tsNaive(d.UpdatedAt)
		}
		items = append(items, Item{
			ID: "activation:" + itoa(d.ID), Kind: "activation", Title: sp("Activated to " + d.DestinationName),
			Description: sp(desc), Status: sp(d.Status), OccurredAt: when,
			Metadata: ActivationMeta{DestinationID: d.DestinationID, DestinationType: d.DestinationType, Operation: d.Operation},
		})
	}

	sends, err := q.TimelineOutreachSends(ctx, id)
	if err != nil {
		return Page{}, err
	}
	for _, s := range sends {
		title := "Outreach email"
		if nonEmpty(s.Subject) {
			title = s.Subject.String
		}
		step := int32(0)
		var stepPtr *int32
		if s.StepNumber.Valid {
			step = s.StepNumber.Int32
			stepPtr = &s.StepNumber.Int32
		}
		var when float64
		for _, c := range []pgtype.Timestamptz{s.RepliedAt, s.OpenedAt, s.BouncedAt, s.SentAt, s.CreatedAt} {
			if c.Valid {
				when = tsTZ(c)
				break
			}
		}
		items = append(items, Item{
			ID: "outreach:" + itoa(s.ID), Kind: "outreach", Title: sp(title),
			Description: sp("Sequence step " + itoa(step+1)), Status: text(s.Status), OccurredAt: when,
			Metadata: OutreachMeta{SequenceID: text(s.SequenceID), StepNumber: stepPtr},
		})
	}

	for _, e := range []struct {
		kind, label string
		v           *string
	}{
		{"created", "Lead created", lead.CreatedAt},
		{"updated", "Lead updated", lead.UpdatedAt},
		{"enriched", "Lead enriched", lead.LastEnrichedAt},
	} {
		if ts := leadTS(e.v); ts != 0 {
			items = append(items, Item{
				ID: "lead:" + e.kind, Kind: "lead", Title: sp(e.label), Description: sp(""),
				Status: sp(e.kind), OccurredAt: ts, Metadata: LeadMeta{},
			})
		}
	}

	if before != nil {
		kept := items[:0]
		for _, it := range items {
			if lessThan(it.OccurredAt, it.ID, *before, beforeID) {
				kept = append(kept, it)
			}
		}
		items = kept
	}
	sort.SliceStable(items, func(a, b int) bool {
		return lessThan(items[b].OccurredAt, items[b].ID, items[a].OccurredAt, items[a].ID)
	})
	page := Page{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		last := page.Items[limit-1]
		page.NextBefore, page.NextBeforeID = &last.OccurredAt, &last.ID
	}
	return page, nil
}

func itoa(n int32) string { return string(enc(int64(n))) }
