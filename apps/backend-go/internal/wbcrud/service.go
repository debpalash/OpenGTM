package wbcrud

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/automationhook"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbregistry"
)

// ErrNotFound means the workbook does not exist in the caller's workspace
// (other workspaces look identical). Routes map it to 404 "Workbook not found".
var ErrNotFound = errors.New("workbook not found")

// NotFoundDetail is the FastAPI detail string for ErrNotFound.
const NotFoundDetail = "Workbook not found"

// LeadReader is the lead store the workbook service reads through (Task 4,
// leadsread). Methods run inside a workspace transaction.
type LeadReader interface {
	QueryLeads(ctx context.Context, tx pgx.Tx, f leadsread.Filter, page, pageSize int) ([]leadsread.Lead, int, error)
	LeadsByIDs(ctx context.Context, tx pgx.Tx, ids []int64) ([]leadsread.Lead, error)
}

// PGLeads adapts the leadsread package functions to LeadReader.
type PGLeads struct{}

func (PGLeads) QueryLeads(ctx context.Context, tx pgx.Tx, f leadsread.Filter, page, pageSize int) ([]leadsread.Lead, int, error) {
	return leadsread.QueryLeads(ctx, tx, f, page, pageSize)
}

func (PGLeads) LeadsByIDs(ctx context.Context, tx pgx.Tx, ids []int64) ([]leadsread.Lead, error) {
	return leadsread.LeadsByIDs(ctx, tx, ids)
}

// Service is the shared core of the workbook CRUD routes.
type Service struct {
	Pool  *pgxpool.Pool
	Leads LeadReader
	Hook  automationhook.Hook
	Reg   *wbregistry.Registry
	Now   func() time.Time
}

// Workbook is a stored workbook (WorkbookResponse shape when marshalled).
// TotalRows holds the stored value until Hydrate computes the live one.
type Workbook struct {
	ID             string
	Name           string
	Description    string
	Status         string
	SourceType     string
	SourceConfig   any // ordered JSON value; {} when null
	FilterCriteria any // ordered JSON value; nil marshals null
	Columns        []Column
	TotalRows      int
	CompletedRows  int
	SyncToLeads    bool
	CreatedAt      *time.Time
	UpdatedAt      *time.Time
	LastRunAt      *time.Time
}

// naiveTime formats like pydantic for naive datetimes: no zone, microsecond
// fraction only when non-zero.
func naiveTime(t *time.Time) *string {
	if t == nil {
		return nil
	}
	u := t.UTC()
	s := u.Format("2006-01-02T15:04:05")
	if us := u.Nanosecond() / 1000; us != 0 {
		s += u.Format(".000000")
	}
	return &s
}

// MarshalJSON produces WorkbookResponse with FastAPI key order.
func (w Workbook) MarshalJSON() ([]byte, error) {
	cols := w.Columns
	if cols == nil {
		cols = []Column{}
	}
	sc := w.SourceConfig
	if sc == nil {
		sc = NewObj()
	}
	return json.Marshal(struct {
		ID             string   `json:"id"`
		Name           string   `json:"name"`
		Description    string   `json:"description"`
		Status         string   `json:"status"`
		SourceType     string   `json:"source_type"`
		SourceConfig   any      `json:"source_config"`
		FilterCriteria any      `json:"filter_criteria"`
		ColumnsConfig  []Column `json:"columns_config"`
		TotalRows      int      `json:"total_rows"`
		CompletedRows  int      `json:"completed_rows"`
		SyncToLeads    bool     `json:"sync_to_leads"`
		CreatedAt      *string  `json:"created_at"`
		UpdatedAt      *string  `json:"updated_at"`
		LastRunAt      *string  `json:"last_run_at"`
	}{w.ID, w.Name, w.Description, w.Status, w.SourceType, sc, w.FilterCriteria, cols,
		w.TotalRows, w.CompletedRows, w.SyncToLeads, naiveTime(w.CreatedAt), naiveTime(w.UpdatedAt), naiveTime(w.LastRunAt)})
}

func ts(t pgtype.Timestamp) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

// jsonValue decodes a stored json column preserving key order; nil for SQL
// NULL, JSON null or undecodable text.
func jsonValue(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	v, err := decodeDocument(raw, false)
	if err != nil {
		return nil
	}
	return v
}

// FromRow converts a stored row. Python's `or` defaults apply: description
// "", status "draft", source_type "leads_filter", sync_to_leads true.
func FromRow(r gen.Workbook) (Workbook, error) {
	cols, err := ParseColumns(r.ColumnsConfig)
	if err != nil {
		return Workbook{}, err
	}
	w := Workbook{
		ID: r.ID, Name: r.Name,
		Description: r.Description.String, Status: r.Status.String, SourceType: r.SourceType.String,
		SourceConfig: jsonValue(r.SourceConfig), FilterCriteria: jsonValue(r.FilterCriteria),
		Columns: cols, TotalRows: int(r.TotalRows.Int32), CompletedRows: int(r.CompletedRows.Int32),
		SyncToLeads: !r.SyncToLeads.Valid || r.SyncToLeads.Bool,
		CreatedAt:   ts(r.CreatedAt), UpdatedAt: ts(r.UpdatedAt), LastRunAt: ts(r.LastRunAt),
	}
	if w.Status == "" {
		w.Status = "draft"
	}
	if w.SourceType == "" {
		w.SourceType = "leads_filter"
	}
	if o, ok := w.SourceConfig.(*Obj); !ok || o == nil {
		w.SourceConfig = NewObj()
	}
	return w, nil
}

// UsesLegacyLeads ports cell_scope.uses_legacy_leads.
func (w Workbook) UsesLegacyLeads() bool {
	if w.SourceType != "leads_filter" {
		return false
	}
	if o, ok := w.SourceConfig.(*Obj); ok {
		if v, ok := o.Get("row_storage_version"); ok {
			if n, ok := v.(json.Number); ok {
				if f, err := n.Float64(); err == nil && f == 2 {
					return false
				}
			}
		}
	}
	return true
}

// NewID returns a random UUID v4 string like Python's generate_uuid.
func NewID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// WithTx runs fn in a workspace-scoped transaction (tenancy.InWorkspace).
func (s *Service) WithTx(ctx context.Context, ws string, fn func(pgx.Tx) error) error {
	return tenancy.InWorkspace(ctx, s.Pool, ws, fn)
}

// Owned fetches a workbook of workspace ws (optionally FOR UPDATE). Missing or
// foreign ids return ErrNotFound.
func (s *Service) Owned(ctx context.Context, tx pgx.Tx, ws, id string, forUpdate bool) (Workbook, error) {
	q := gen.New(tx)
	var (
		row gen.Workbook
		err error
	)
	if forUpdate {
		row, err = q.GetWorkbookOwnedForUpdate(ctx, gen.GetWorkbookOwnedForUpdateParams{WorkbookID: id, WorkspaceID: ws})
	} else {
		row, err = q.GetWorkbookOwned(ctx, gen.GetWorkbookOwnedParams{WorkbookID: id, WorkspaceID: ws})
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return Workbook{}, ErrNotFound
	}
	if err != nil {
		return Workbook{}, err
	}
	return FromRow(row)
}

// List returns the workspace's workbooks ordered by updated_at desc, with live
// total_rows (Hydrate).
func (s *Service) List(ctx context.Context, tx pgx.Tx, ws string) ([]Workbook, error) {
	rows, err := gen.New(tx).ListWorkbooks(ctx, ws)
	if err != nil {
		return nil, err
	}
	out := make([]Workbook, 0, len(rows))
	for _, r := range rows {
		w, err := FromRow(r)
		if err != nil {
			return nil, err
		}
		s.Hydrate(ctx, tx, ws, &w, true)
		out = append(out, w)
	}
	return out, nil
}

// Hydrate sets w.TotalRows by the _workbook_response rule: live row count when
// > 0, else for legacy leads_filter workbooks the lead query total, else 0; a
// row-count error falls back to the stored total. Each probe runs in a
// savepoint so a failing statement cannot poison the caller's transaction.
//
// withLeads mirrors whether Python's _workbook_response received a lead_db:
// true for list_workbooks, from-jobs merge and update_workbook (workbooks.py
// lines 338, 568, 998); false for create_workbook, get_workbook, add_column and
// remove_column (450, 672, 720, 1225, 1304), where a legacy leads_filter
// workbook with no rows reports 0.
func (s *Service) Hydrate(ctx context.Context, tx pgx.Tx, ws string, w *Workbook, withLeads bool) {
	stored := w.TotalRows
	w.TotalRows = 0

	var count int64
	if err := savepoint(ctx, tx, func(tx pgx.Tx) (err error) {
		count, err = gen.New(tx).CountWorkbookRows(ctx, gen.CountWorkbookRowsParams{WorkbookID: w.ID, WorkspaceID: ws})
		return err
	}); err != nil {
		w.TotalRows = stored
		return
	}
	if count > 0 {
		w.TotalRows = int(count)
		return
	}
	if !withLeads || s.Leads == nil || !w.UsesLegacyLeads() {
		return
	}
	var f leadsread.Filter
	if raw, err := json.Marshal(w.FilterCriteria); err == nil && w.FilterCriteria != nil {
		if json.Unmarshal(raw, &f) != nil {
			return // Python raises inside its try: total stays 0
		}
	}
	var total int
	if err := savepoint(ctx, tx, func(tx pgx.Tx) (err error) {
		_, total, err = s.Leads.QueryLeads(ctx, tx, f, 1, 1)
		return err
	}); err == nil {
		w.TotalRows = total
	}
}

// savepoint runs fn in a nested transaction, rolling back on error.
func savepoint(ctx context.Context, tx pgx.Tx, fn func(pgx.Tx) error) error {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	if err := fn(sp); err != nil {
		_ = sp.Rollback(context.WithoutCancel(ctx))
		return err
	}
	return sp.Commit(ctx)
}
