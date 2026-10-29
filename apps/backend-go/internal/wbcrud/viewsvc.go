package wbcrud

import (
	"bytes"
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// ViewNotFoundDetail is _owned_view's 404.
const ViewNotFoundDetail = "View not found"

// View is WorkbookViewResponse (key order id, workbook_id, name, config,
// created_at, updated_at).
type View struct {
	ID         string     `json:"id"`
	WorkbookID string     `json:"workbook_id"`
	Name       string     `json:"name"`
	Config     ViewConfig `json:"config"`
	CreatedAt  *string    `json:"created_at"`
	UpdatedAt  *string    `json:"updated_at"`
}

func emptyViewConfig() ViewConfig {
	return ViewConfig{Filters: []ViewFilter{}, Sort: []ViewSort{}, HiddenColumns: []string{}}
}

// viewFromRow expands the stored config with defaults. A NULL or malformed
// stored config (FastAPI answers 500 on the response model) is shown as the
// empty default config: documented hardening.
func viewFromRow(r gen.WorkbookView) View {
	cfg := emptyViewConfig()
	if len(r.Config) > 0 && string(bytes.TrimSpace(r.Config)) != "null" {
		if c, errs := ParseStoredViewConfig(r.Config); errs == nil {
			cfg = c
		}
	}
	return View{ID: r.ID, WorkbookID: r.WorkbookID, Name: r.Name, Config: cfg,
		CreatedAt: naiveTime(ts(r.CreatedAt)), UpdatedAt: naiveTime(ts(r.UpdatedAt))}
}

func (v View) MarshalJSON() ([]byte, error) {
	type plain View
	return marshalNoEscape(plain(v))
}

func marshalConfig(c ViewConfig) ([]byte, error) { return marshalNoEscape(c) }

func (s *Service) viewOwned(ctx context.Context, tx pgx.Tx, ws, wbID, viewID string) (gen.WorkbookView, error) {
	r, err := gen.New(tx).GetWorkbookViewOwned(ctx, gen.GetWorkbookViewOwnedParams{ViewID: viewID, WorkbookID: wbID, WorkspaceID: ws})
	if errors.Is(err, pgx.ErrNoRows) {
		return r, guard(404, ViewNotFoundDetail)
	}
	return r, err
}

// ListViews is list_views (oldest first, ties by id).
func (s *Service) ListViews(ctx context.Context, ws, wbID string) ([]View, error) {
	out := []View{}
	err := s.WithTx(ctx, ws, func(tx pgx.Tx) error {
		if _, err := s.Owned(ctx, tx, ws, wbID, false); err != nil {
			if errors.Is(err, ErrNotFound) {
				return guard(404, NotFoundDetail)
			}
			return err
		}
		rows, err := gen.New(tx).ListWorkbookViews(ctx, gen.ListWorkbookViewsParams{WorkbookID: wbID, WorkspaceID: ws})
		if err != nil {
			return err
		}
		for _, r := range rows {
			out = append(out, viewFromRow(r))
		}
		return nil
	})
	return out, err
}

// CreateView is create_view: locks the workbook, validates the referenced columns.
func (s *Service) CreateView(ctx context.Context, ws, wbID string, b ViewCreateBody) (View, error) {
	var out View
	err := s.WithTx(ctx, ws, func(tx pgx.Tx) error {
		w, err := s.lock(ctx, tx, ws, wbID)
		if err != nil {
			return err
		}
		if g := ValidateViewColumns(w.Columns, b.Config); g != nil {
			return g
		}
		cfg, err := marshalConfig(b.Config)
		if err != nil {
			return err
		}
		r, err := gen.New(tx).InsertWorkbookView(ctx, gen.InsertWorkbookViewParams{ID: NewID(), WorkspaceID: ws, WorkbookID: wbID, Name: b.Name, Config: cfg})
		if err != nil {
			return err
		}
		out = viewFromRow(r)
		return nil
	})
	return out, err
}

// UpdateView is update_view: workbook lock, view lookup (404), column
// validation (422) in Python's order; updated_at is always stamped.
func (s *Service) UpdateView(ctx context.Context, ws, wbID, viewID string, b ViewUpdateBody) (View, error) {
	var out View
	err := s.WithTx(ctx, ws, func(tx pgx.Tx) error {
		w, err := s.lock(ctx, tx, ws, wbID)
		if err != nil {
			return err
		}
		if _, err := s.viewOwned(ctx, tx, ws, wbID, viewID); err != nil {
			return err
		}
		p := gen.UpdateWorkbookViewParams{ViewID: viewID, WorkbookID: wbID, WorkspaceID: ws}
		if b.Config != nil {
			if g := ValidateViewColumns(w.Columns, *b.Config); g != nil {
				return g
			}
			if p.Config, err = marshalConfig(*b.Config); err != nil {
				return err
			}
			p.SetConfig = true
		}
		if b.Name != nil {
			p.Name = pgtype.Text{String: *b.Name, Valid: true}
		}
		r, err := gen.New(tx).UpdateWorkbookView(ctx, p)
		if err != nil {
			return err
		}
		out = viewFromRow(r)
		return nil
	})
	return out, err
}

// DeleteView is delete_view (never touches rows).
func (s *Service) DeleteView(ctx context.Context, ws, wbID, viewID string) error {
	return s.WithTx(ctx, ws, func(tx pgx.Tx) error {
		if _, err := s.lock(ctx, tx, ws, wbID); err != nil {
			return err
		}
		if _, err := s.viewOwned(ctx, tx, ws, wbID, viewID); err != nil {
			return err
		}
		_, err := gen.New(tx).DeleteWorkbookView(ctx, gen.DeleteWorkbookViewParams{ViewID: viewID, WorkbookID: wbID, WorkspaceID: ws})
		return err
	})
}
