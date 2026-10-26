package signals

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// Signal mirrors SignalStore._signal_to_dict; Read is 1/0 like Python.
// NULL text columns read as "" (the Python scanner always writes them).
type Signal struct {
	ID, WorkspaceID                                            string
	LeadID                                                     *int64
	Company, SignalType, Title, Description, Source, SourceURL string
	Weight                                                     *int64
	CreatedAt                                                  float64
	Read                                                       int
}

// MarshalJSON emits the Python key order and float repr for created_at.
func (s Signal) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	put := func(k string, v any) {
		if b.Len() > 1 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		b.Write(kb)
		b.WriteByte(':')
		var vb bytes.Buffer
		enc := json.NewEncoder(&vb)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(v)
		b.Write(bytes.TrimSuffix(vb.Bytes(), []byte("\n")))
	}
	put("id", s.ID)
	put("workspace_id", s.WorkspaceID)
	put("lead_id", s.LeadID)
	put("company", s.Company)
	put("signal_type", s.SignalType)
	put("title", s.Title)
	put("description", s.Description)
	put("source", s.Source)
	put("source_url", s.SourceURL)
	put("weight", s.Weight)
	b.WriteString(`,"created_at":` + pyFloatJSON(s.CreatedAt))
	put("read", s.Read)
	b.WriteByte('}')
	return b.Bytes(), nil
}

// pyFloatJSON renders like Python's json.dumps on a float (allow_nan).
func pyFloatJSON(f float64) string {
	if s, err := canonjson.FloatRepr(f); err == nil {
		return s
	}
	switch {
	case f != f:
		return "NaN"
	case f > 0:
		return "Infinity"
	}
	return "-Infinity"
}

// PageResult is one keyset page. Python key order for the HTTP body depends on the
// route (cursor present or not); that is the route's concern.
type PageResult struct {
	Signals    []Signal
	NextCursor *string
	HasMore    bool
}

func i64p(v pgtype.Int4) *int64 {
	if !v.Valid {
		return nil
	}
	n := int64(v.Int32)
	return &n
}

func lead4(id *int64) pgtype.Int4 {
	if id == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(*id), Valid: true}
}

func clamp32(n int) int32 {
	switch {
	case n < 0:
		return 0
	case n > 1<<30:
		return 1 << 30
	}
	return int32(n)
}

func readInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// List ports SignalStore.get_signals: newest first, id descending on ties.
// signalType "" is unset. Run inside tenancy.InWorkspace.
func List(ctx context.Context, tx pgx.Tx, signalType string, leadID *int64, limit, offset int) ([]Signal, error) {
	st := pgtype.Text{}
	if signalType != "" {
		st = pgtype.Text{String: signalType, Valid: true}
	}
	rows, err := gen.New(tx).ListSignals(ctx, gen.ListSignalsParams{
		SignalType: st, LeadID: lead4(leadID), PageOffset: clamp32(offset), PageLimit: clamp32(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]Signal, 0, len(rows))
	for _, r := range rows {
		out = append(out, Signal{ID: r.ID, WorkspaceID: r.WorkspaceID, LeadID: i64p(r.LeadID), Company: r.Company,
			SignalType: r.SignalType, Title: r.Title, Description: r.Description, Source: r.Source, SourceURL: r.SourceUrl,
			Weight: i64p(r.Weight), CreatedAt: r.CreatedAt, Read: readInt(r.Read)})
	}
	return out, nil
}

// Page ports SignalStore.get_signals_page (limit+1 probe). cursor "" is unset;
// an undecodable cursor is ErrBadCursor. An empty signalTypes slice is unset.
// limit must be >= 1.
func Page(ctx context.Context, tx pgx.Tx, signalTypes []string, leadID *int64, limit int, cursor string) (PageResult, error) {
	p := gen.PageSignalsParams{LeadID: lead4(leadID), PageLimit: clamp32(limit) + 1}
	if len(signalTypes) > 0 {
		p.SignalTypes = signalTypes
	}
	if cursor != "" {
		ts, id, err := DecodeCursor(cursor)
		if err != nil {
			return PageResult{}, err
		}
		p.CursorTs = pgtype.Float8{Float64: ts, Valid: true}
		p.CursorID = pgtype.Text{String: id, Valid: true}
	}
	rows, err := gen.New(tx).PageSignals(ctx, p)
	if err != nil {
		return PageResult{}, err
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	out := make([]Signal, 0, len(rows))
	for _, r := range rows {
		out = append(out, Signal{ID: r.ID, WorkspaceID: r.WorkspaceID, LeadID: i64p(r.LeadID), Company: r.Company,
			SignalType: r.SignalType, Title: r.Title, Description: r.Description, Source: r.Source, SourceURL: r.SourceUrl,
			Weight: i64p(r.Weight), CreatedAt: r.CreatedAt, Read: readInt(r.Read)})
	}
	pg := PageResult{Signals: out, HasMore: hasMore}
	if hasMore {
		last := out[len(out)-1]
		c := EncodeCursor(last.CreatedAt, last.ID)
		pg.NextCursor = &c
	}
	return pg, nil
}

// TypeCount is one signal_type bucket.
type TypeCount struct {
	Type string
	N    int64
}

// CountsResult mirrors get_signal_counts: per-type counts, then total and unread.
// Decision D6: types are sorted by name (Python's order was GROUP BY arbitrary).
type CountsResult struct {
	Types         []TypeCount
	Total, Unread int64
}

func (c CountsResult) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for _, t := range c.Types {
		if t.Type == "total" || t.Type == "unread" {
			continue // overwritten by the real totals in Python's dict
		}
		k, _ := json.Marshal(t.Type)
		b.Write(k)
		b.WriteString(":" + strconv.FormatInt(t.N, 10) + ",")
	}
	b.WriteString(`"total":` + strconv.FormatInt(c.Total, 10) + `,"unread":` + strconv.FormatInt(c.Unread, 10) + "}")
	return b.Bytes(), nil
}

// Counts ports get_signal_counts. A NULL signal_type is keyed "null" (Python's
// json.dumps of a None key).
func Counts(ctx context.Context, tx pgx.Tx) (CountsResult, error) {
	q := gen.New(tx)
	rows, err := q.SignalTypeCounts(ctx)
	if err != nil {
		return CountsResult{}, err
	}
	var c CountsResult
	for _, r := range rows {
		name := "null"
		if r.SignalType.Valid {
			name = r.SignalType.String
		}
		c.Types = append(c.Types, TypeCount{Type: name, N: r.N})
	}
	sort.SliceStable(c.Types, func(i, j int) bool { return c.Types[i].Type < c.Types[j].Type })
	tot, err := q.SignalTotals(ctx)
	if err != nil {
		return CountsResult{}, err
	}
	c.Total, c.Unread = tot.Total, tot.Unread
	return c, nil
}

// MarkRead ports mark_signals_read: only this workspace's rows change; an empty
// list is a no-op.
func MarkRead(ctx context.Context, tx pgx.Tx, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	return gen.New(tx).MarkSignalsRead(ctx, ids)
}
