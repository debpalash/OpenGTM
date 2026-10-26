// Package emaildata is the /api/data email data service (Python routers/crm.py).
//
// email_data and links are legacy GLOBAL tables (no workspace_id, no RLS), so this
// service never runs inside tenancy.InWorkspace: it is reachable only through the
// router-level global-admin gate (plan 07 D5) and is the only code that touches
// those tables. Every method answers the whole table.
package emaildata

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

var (
	// ErrNotFound: no email_data row has the id (also for ids outside int4).
	ErrNotFound = errors.New("emaildata: record not found")
	// ErrBadPaging: negative limit or offset (Python: 500; Go: 422, plan 07 D4).
	ErrBadPaging = errors.New("emaildata: limit and offset must not be negative")
	// ErrInvalidInput: a string carries a NUL byte, which Postgres cannot store or
	// compare (Python: driver error, 500; Go: 422).
	ErrInvalidInput = errors.New("emaildata: invalid input")
)

// NotFoundDetail is the 404 `detail` of PATCH for a missing id.
func NotFoundDetail(id int64) string { return fmt.Sprintf("Record with id %d not found", id) }

// PagingErrors answers the 422 entries for negative limit/offset in FastAPI's
// shape (nil when both are valid).
func PagingErrors(limit, offset int64) []httpx.FieldError {
	var errs []httpx.FieldError
	for _, p := range []struct {
		name string
		v    int64
	}{{"limit", limit}, {"offset", offset}} {
		if p.v < 0 {
			errs = append(errs, httpx.FieldError{Type: "greater_than_equal", Loc: []any{"query", p.name}, Input: p.v,
				Msg: "Input should be greater than or equal to 0", Ctx: map[string]any{"ge": 0}})
		}
	}
	return errs
}

// Filter is the shared list/export/mark filter. Falsy values are ignored as in
// Python (`if source_link_id:`, `if q:`, ...).
type Filter struct {
	SourceLinkID  *int64
	Q             *string
	StartDate     *string
	EndDate       *string
	IsUsed        *bool
	SortBy, Order string
}

// Row is one email_data row; JSON key order is the ORM column order.
type Row struct {
	ID           int64   `json:"id"`
	Name         *string `json:"name"`
	Email        *string `json:"email"`
	SourceLinkID *int64  `json:"source_link_id"`
	CreatedAt    *string `json:"created_at"`
	IsUsed       *bool   `json:"is_used"`
	Tags         *string `json:"tags"`
	Notes        *string `json:"notes"`
}

// Item is a list row: the columns then source_url (left join of links).
type Item struct {
	Row
	SourceURL *string `json:"source_url"`
}

// Page is the list answer.
type Page struct {
	Total int64  `json:"total"`
	Items []Item `json:"items"`
}

// MarkUsedRequest is the POST /mark-used body (Used defaults to true).
type MarkUsedRequest struct {
	IDs          []int64
	Used         bool
	Q            *string
	StartDate    *string
	EndDate      *string
	SourceLinkID *int64
	IsUsedFilter *bool
}

// OptStr / OptBool carry pydantic exclude_unset semantics: Set records that the
// key was present, Val nil is an explicit null (stored as NULL).
type OptStr struct {
	Set bool
	Val *string
}
type OptBool struct {
	Set bool
	Val *bool
}

// Patch is the PATCH body.
type Patch struct {
	Name, Email, Tags, Notes OptStr
	IsUsed                   OptBool
}

// DayCount and DomainCount are the stats rows.
type DayCount struct {
	Date  string `json:"date"`
	Count int64  `json:"count"`
}
type DomainCount struct {
	Domain string `json:"domain"`
	Count  int64  `json:"count"`
}

// Service reads and writes the global email data tables.
type Service struct{ q *gen.Queries }

// New wraps a pool (or any gen.DBTX).
func New(db gen.DBTX) *Service { return &Service{q: gen.New(db)} }

const exportBatch = 1000

var sortColumns = map[string]bool{"id": true, "name": true, "email": true, "source_link_id": true,
	"created_at": true, "is_used": true, "tags": true, "notes": true}

// norm is a Filter reduced to SQL arguments.
type norm struct {
	link      pgtype.Int4
	q, sd, ed pgtype.Text
	used      pgtype.Bool
	sortBy    string
	desc      bool
	none      bool // a filter that can never match (source link outside int4)
}

func text(s *string) (pgtype.Text, error) {
	if s == nil || *s == "" {
		return pgtype.Text{}, nil
	}
	if strings.IndexByte(*s, 0) >= 0 {
		return pgtype.Text{}, ErrInvalidInput
	}
	return pgtype.Text{String: *s, Valid: true}, nil
}

func int4(id int64) (int32, bool) {
	if id < math.MinInt32 || id > math.MaxInt32 {
		return 0, false
	}
	return int32(id), true
}

func normalize(f Filter) (norm, error) {
	n := norm{sortBy: "id", desc: f.Order != "asc"}
	if sortColumns[f.SortBy] {
		n.sortBy = f.SortBy
	}
	if f.SourceLinkID != nil && *f.SourceLinkID != 0 {
		if v, ok := int4(*f.SourceLinkID); ok {
			n.link = pgtype.Int4{Int32: v, Valid: true}
		} else {
			n.none = true
		}
	}
	var err error
	if n.q, err = text(f.Q); err != nil {
		return n, err
	}
	if n.sd, err = text(f.StartDate); err != nil {
		return n, err
	}
	if n.ed, err = text(f.EndDate); err != nil {
		return n, err
	}
	if f.IsUsed != nil {
		n.used = pgtype.Bool{Bool: *f.IsUsed, Valid: true}
	}
	return n, nil
}

func p32(v pgtype.Int4) *int64 {
	if !v.Valid {
		return nil
	}
	x := int64(v.Int32)
	return &x
}
func ps(v pgtype.Text) *string {
	if !v.Valid {
		return nil
	}
	return &v.String
}
func pb(v pgtype.Bool) *bool {
	if !v.Valid {
		return nil
	}
	return &v.Bool
}

func fromModel(m gen.EmailDatum) Row {
	return Row{ID: int64(m.ID), Name: ps(m.Name), Email: ps(m.Email), SourceLinkID: p32(m.SourceLinkID),
		CreatedAt: ps(m.CreatedAt), IsUsed: pb(m.IsUsed), Tags: ps(m.Tags), Notes: ps(m.Notes)}
}

// List answers one page. limit 0 answers only the total; a negative limit or
// offset is ErrBadPaging.
func (s *Service) List(ctx context.Context, f Filter, limit, offset int) (Page, error) {
	if limit < 0 || offset < 0 {
		return Page{}, ErrBadPaging
	}
	n, err := normalize(f)
	if err != nil {
		return Page{}, err
	}
	page := Page{Items: []Item{}}
	if n.none {
		return page, nil
	}
	if page.Total, err = s.q.CountEmailData(ctx, gen.CountEmailDataParams{SourceLinkID: n.link, Q: n.q,
		StartDate: n.sd, EndDate: n.ed, IsUsed: n.used}); err != nil {
		return Page{}, err
	}
	if limit == 0 {
		return page, nil
	}
	rows, err := s.q.ListEmailData(ctx, gen.ListEmailDataParams{SourceLinkID: n.link, Q: n.q, StartDate: n.sd,
		EndDate: n.ed, IsUsed: n.used, SortBy: n.sortBy, SortDesc: n.desc, RowOffset: int64(offset), RowLimit: int64(limit)})
	if err != nil {
		return Page{}, err
	}
	for _, r := range rows {
		page.Items = append(page.Items, Item{
			Row: fromModel(gen.EmailDatum{ID: r.ID, Name: r.Name, Email: r.Email, SourceLinkID: r.SourceLinkID,
				CreatedAt: r.CreatedAt, IsUsed: r.IsUsed, Tags: r.Tags, Notes: r.Notes}),
			SourceURL: ps(r.SourceUrl)})
	}
	return page, nil
}

// MarkUsed sets is_used on the listed ids; without ids on every row matching the
// bulk filter, and with no filter at all on EVERY row (Python parity, global
// admin only). Ids outside int4 cannot exist and are dropped.
func (s *Service) MarkUsed(ctx context.Context, req MarkUsedRequest) (int64, error) {
	if len(req.IDs) > 0 {
		ids := make([]int32, 0, len(req.IDs))
		for _, id := range req.IDs {
			if v, ok := int4(id); ok {
				ids = append(ids, v)
			}
		}
		if len(ids) == 0 {
			return 0, nil
		}
		return s.q.MarkEmailDataUsedByIDs(ctx, gen.MarkEmailDataUsedByIDsParams{Used: req.Used, Ids: ids})
	}
	n, err := normalize(Filter{SourceLinkID: req.SourceLinkID, Q: req.Q, StartDate: req.StartDate,
		EndDate: req.EndDate, IsUsed: req.IsUsedFilter})
	if err != nil {
		return 0, err
	}
	if n.none {
		return 0, nil
	}
	return s.q.MarkEmailDataUsedByFilter(ctx, gen.MarkEmailDataUsedByFilterParams{Used: req.Used,
		SourceLinkID: n.link, Q: n.q, StartDate: n.sd, EndDate: n.ed, IsUsed: n.used})
}

func opt(o OptStr) (bool, pgtype.Text, error) {
	if !o.Set {
		return false, pgtype.Text{}, nil
	}
	if o.Val == nil {
		return true, pgtype.Text{}, nil
	}
	if strings.IndexByte(*o.Val, 0) >= 0 {
		return false, pgtype.Text{}, ErrInvalidInput
	}
	return true, pgtype.Text{String: *o.Val, Valid: true}, nil
}

// Update applies the keys present in patch (explicit null sets NULL) and returns
// the full row, or ErrNotFound.
func (s *Service) Update(ctx context.Context, id int64, patch Patch) (Row, error) {
	v, ok := int4(id)
	if !ok {
		return Row{}, ErrNotFound
	}
	a := gen.UpdateEmailDataParams{ID: v}
	var err error
	if a.SetName, a.Name, err = opt(patch.Name); err != nil {
		return Row{}, err
	}
	if a.SetEmail, a.Email, err = opt(patch.Email); err != nil {
		return Row{}, err
	}
	if a.SetTags, a.Tags, err = opt(patch.Tags); err != nil {
		return Row{}, err
	}
	if a.SetNotes, a.Notes, err = opt(patch.Notes); err != nil {
		return Row{}, err
	}
	if patch.IsUsed.Set {
		a.SetIsUsed = true
		if patch.IsUsed.Val != nil {
			a.IsUsed = pgtype.Bool{Bool: *patch.IsUsed.Val, Valid: true}
		}
	}
	m, err := s.q.UpdateEmailData(ctx, a)
	if errors.Is(err, pgx.ErrNoRows) {
		return Row{}, ErrNotFound
	}
	if err != nil {
		return Row{}, err
	}
	return fromModel(m), nil
}

// Delete removes one row; found is false for a missing id (the route answers
// 200 {"status":"not found"} as Python does).
func (s *Service) Delete(ctx context.Context, id int64) (bool, error) {
	v, ok := int4(id)
	if !ok {
		return false, nil
	}
	n, err := s.q.DeleteEmailData(ctx, v)
	return n > 0, err
}
