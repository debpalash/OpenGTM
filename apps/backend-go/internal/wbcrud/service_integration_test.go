//go:build integration

package wbcrud_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

const (
	wsA = "ws-wbcrud-a"
	wsB = "ws-wbcrud-b"
)

func setup(t *testing.T) (*wbcrud.Service, context.Context) {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	ctx := context.Background()
	clean := func() {
		for _, w := range []string{wsA, wsB} {
			_ = tenancy.InWorkspace(ctx, pool, w, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `DELETE FROM workbooks`); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `DELETE FROM leads`)
				return err
			})
		}
	}
	clean()
	t.Cleanup(clean)
	return &wbcrud.Service{Pool: pool, Leads: wbcrud.PGLeads{}}, ctx
}

func insert(t *testing.T, s *wbcrud.Service, ctx context.Context, ws, name, srcType, srcCfg, filter string) string {
	t.Helper()
	id := wbcrud.NewID()
	err := s.WithTx(ctx, ws, func(tx pgx.Tx) error {
		_, err := gen.New(tx).InsertWorkbook(ctx, gen.InsertWorkbookParams{
			ID: id, Name: name, Description: "", WorkspaceID: ws, SourceType: srcType,
			SourceConfig: []byte(srcCfg), FilterCriteria: []byte(filter), ColumnsConfig: []byte(`[]`),
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestOwnedOtherWorkspaceIsNotFound(t *testing.T) {
	s, ctx := setup(t)
	id := insert(t, s, ctx, wsA, "a", "empty", `{}`, `{}`)
	_ = s.WithTx(ctx, wsB, func(tx pgx.Tx) error {
		if _, err := s.Owned(ctx, tx, wsB, id, false); !errors.Is(err, wbcrud.ErrNotFound) {
			t.Errorf("RLS: want ErrNotFound, got %v", err)
		}
		// explicit predicate: asking as ws A inside ws B's tx must not leak either
		if _, err := s.Owned(ctx, tx, wsA, id, false); !errors.Is(err, wbcrud.ErrNotFound) {
			t.Errorf("predicate: want ErrNotFound, got %v", err)
		}
		return nil
	})
	_ = s.WithTx(ctx, wsA, func(tx pgx.Tx) error {
		w, err := s.Owned(ctx, tx, wsA, id, false)
		if err != nil || w.ID != id || w.Status != "draft" {
			t.Errorf("own: %+v %v", w, err)
		}
		if _, err := s.Owned(ctx, tx, wsA, "missing", false); !errors.Is(err, wbcrud.ErrNotFound) {
			t.Errorf("missing: %v", err)
		}
		return nil
	})
}

func TestOwnedForUpdateLocks(t *testing.T) {
	s, ctx := setup(t)
	id := insert(t, s, ctx, wsA, "a", "empty", `{}`, `{}`)
	locked := make(chan struct{})
	release := make(chan struct{})
	done1 := make(chan error, 1)
	go func() {
		done1 <- s.WithTx(ctx, wsA, func(tx pgx.Tx) error {
			if _, err := s.Owned(ctx, tx, wsA, id, true); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	got := make(chan time.Time, 1)
	done2 := make(chan error, 1)
	go func() {
		done2 <- s.WithTx(ctx, wsA, func(tx pgx.Tx) error {
			_, err := s.Owned(ctx, tx, wsA, id, true)
			got <- time.Now()
			return err
		})
	}()
	select {
	case <-got:
		t.Fatal("second FOR UPDATE did not block")
	case <-time.After(400 * time.Millisecond):
	}
	released := time.Now()
	close(release)
	if err := <-done1; err != nil {
		t.Fatal(err)
	}
	if at := <-got; at.Before(released) {
		t.Fatal("second tx acquired before first committed")
	}
	if err := <-done2; err != nil {
		t.Fatal(err)
	}
}

func hydrated(t *testing.T, s *wbcrud.Service, ctx context.Context, ws, id string, pre func(tx pgx.Tx), withLeads ...bool) int {
	wl := len(withLeads) == 0 || withLeads[0]
	t.Helper()
	var n int
	err := s.WithTx(ctx, ws, func(tx pgx.Tx) error {
		w, err := s.Owned(ctx, tx, ws, id, false)
		if err != nil {
			return err
		}
		if pre != nil {
			pre(tx)
		}
		s.Hydrate(ctx, tx, ws, &w, wl)
		n = w.TotalRows
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestTotalRowsRules(t *testing.T) {
	s, ctx := setup(t)
	seedLeads := func() {
		if err := s.WithTx(ctx, wsA, func(tx pgx.Tx) error {
			for _, c := range []string{"c1", "c2", "c3"} {
				if _, err := tx.Exec(ctx, `INSERT INTO leads (workspace_id, company, city) VALUES ($1,$2,'Paris')`, wsA, c); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	seedLeads()
	addRows := func(id string, n int) {
		if err := s.WithTx(ctx, wsA, func(tx pgx.Tx) error {
			for i := 0; i < n; i++ {
				if _, err := tx.Exec(ctx, `INSERT INTO workbook_rows (workbook_id, workspace_id, "position", data) VALUES ($1,$2,$3,'{}'::json)`, id, wsA, i); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	rows := insert(t, s, ctx, wsA, "rows", "leads_filter", `{"row_storage_version":2}`, `{}`)
	addRows(rows, 2)
	if got := hydrated(t, s, ctx, wsA, rows, nil); got != 2 {
		t.Errorf("rows>0: got %d want 2", got)
	}
	legacy := insert(t, s, ctx, wsA, "legacy", "leads_filter", `{}`, `{"city":"Paris"}`)
	if got := hydrated(t, s, ctx, wsA, legacy, nil); got != 3 {
		t.Errorf("legacy filter: got %d want 3", got)
	}
	if got := hydrated(t, s, ctx, wsA, legacy, nil, false); got != 0 {
		t.Errorf("legacy filter without lead_db (get/add/remove column/create): got %d want 0", got)
	}
	v2 := insert(t, s, ctx, wsA, "v2", "leads_filter", `{"row_storage_version":2}`, `{}`)
	if got := hydrated(t, s, ctx, wsA, v2, nil); got != 0 {
		t.Errorf("v2 empty: got %d want 0", got)
	}
	csv := insert(t, s, ctx, wsA, "csv", "csv", `{}`, `{}`)
	if got := hydrated(t, s, ctx, wsA, csv, nil); got != 0 {
		t.Errorf("csv empty: got %d want 0", got)
	}

	// Count error: another session holds ACCESS EXCLUSIVE on workbook_rows, the
	// probe hits lock_timeout, and the stored total_rows is used. The caller's
	// transaction must stay usable afterwards.
	if err := s.WithTx(ctx, wsA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE workbooks SET total_rows = 7 WHERE id = $1`, csv)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ownerURL := os.Getenv("TEST_DATABASE_URL")
	owner, err := pgxpool.New(ctx, ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	otx, err := owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer otx.Rollback(ctx)
	if _, err := otx.Exec(ctx, `LOCK TABLE workbook_rows IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	got := hydrated(t, s, ctx, wsA, csv, func(tx pgx.Tx) {
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '150ms'`); err != nil {
			t.Fatal(err)
		}
	})
	if got != 7 {
		t.Errorf("count error: got %d want stored 7", got)
	}
}

func TestUpdatedAtAdvancesOnUpdate(t *testing.T) {
	s, ctx := setup(t)
	id := insert(t, s, ctx, wsA, "a", "empty", `{}`, `{}`)
	updated := func() time.Time {
		var u time.Time
		_ = s.WithTx(ctx, wsA, func(tx pgx.Tx) error {
			w, err := s.Owned(ctx, tx, wsA, id, false)
			if err != nil {
				t.Fatal(err)
			}
			u = *w.UpdatedAt
			return nil
		})
		return u
	}
	prev := updated()
	steps := map[string]func(q *gen.Queries) error{
		"meta": func(q *gen.Queries) error {
			_, err := q.UpdateWorkbookMeta(ctx, gen.UpdateWorkbookMetaParams{Name: text("b"), WorkbookID: id, WorkspaceID: wsA})
			return err
		},
		"columns": func(q *gen.Queries) error {
			_, err := q.UpdateWorkbookColumns(ctx, gen.UpdateWorkbookColumnsParams{ColumnsConfig: []byte(`[]`), WorkbookID: id, WorkspaceID: wsA})
			return err
		},
		"touch": func(q *gen.Queries) error {
			return q.TouchWorkbook(ctx, gen.TouchWorkbookParams{WorkbookID: id, WorkspaceID: wsA})
		},
	}
	for _, name := range []string{"meta", "columns", "touch"} {
		time.Sleep(20 * time.Millisecond)
		if err := s.WithTx(ctx, wsA, func(tx pgx.Tx) error { return steps[name](gen.New(tx)) }); err != nil {
			t.Fatal(err)
		}
		cur := updated()
		if !cur.After(prev) {
			t.Errorf("%s: updated_at did not advance (%v -> %v)", name, prev, cur)
		}
		prev = cur
	}
	// meta with every field NULL keeps the stored values.
	w := func() wbcrud.Workbook {
		var w wbcrud.Workbook
		_ = s.WithTx(ctx, wsA, func(tx pgx.Tx) (err error) { w, err = s.Owned(ctx, tx, wsA, id, false); return })
		return w
	}()
	if w.Name != "b" || w.Status != "draft" {
		t.Errorf("meta COALESCE changed values: %+v", w)
	}
}

func TestListOrderedByUpdatedAtDesc(t *testing.T) {
	s, ctx := setup(t)
	a := insert(t, s, ctx, wsA, "a", "empty", `{}`, `{}`)
	time.Sleep(20 * time.Millisecond)
	b := insert(t, s, ctx, wsA, "b", "empty", `{}`, `{}`)
	_ = insert(t, s, ctx, wsB, "other", "empty", `{}`, `{}`)
	list := func() []string {
		var ids []string
		if err := s.WithTx(ctx, wsA, func(tx pgx.Tx) error {
			ws, err := s.List(ctx, tx, wsA)
			for _, w := range ws {
				ids = append(ids, w.ID)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return ids
	}
	if got := list(); len(got) != 2 || got[0] != b || got[1] != a {
		t.Fatalf("initial order %v want [%s %s]", got, b, a)
	}
	time.Sleep(20 * time.Millisecond)
	if err := s.WithTx(ctx, wsA, func(tx pgx.Tx) error {
		return gen.New(tx).TouchWorkbook(ctx, gen.TouchWorkbookParams{WorkbookID: a, WorkspaceID: wsA})
	}); err != nil {
		t.Fatal(err)
	}
	if got := list(); got[0] != a || got[1] != b {
		t.Fatalf("after touch %v want [%s %s]", got, a, b)
	}
}

func TestUpdateIfChanged(t *testing.T) {
	s, ctx := setup(t)
	id := insert(t, s, ctx, wsA, "a", "empty", `{}`, `{}`)
	str := func(v string) *string { return &v }
	cols := func(c ...wbcrud.Column) *[]wbcrud.Column { return &c }
	run := func(p wbcrud.UpdatePatch) (wbcrud.Workbook, bool) {
		var out wbcrud.Workbook
		var changed bool
		if err := s.WithTx(ctx, wsA, func(tx pgx.Tx) error {
			w, err := s.Owned(ctx, tx, wsA, id, true)
			if err != nil {
				return err
			}
			out, changed, err = s.UpdateIfChanged(ctx, tx, wsA, w, p)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out, changed
	}
	stamp := func() time.Time {
		var u time.Time
		_ = s.WithTx(ctx, wsA, func(tx pgx.Tx) error {
			w, _ := s.Owned(ctx, tx, wsA, id, false)
			u = *w.UpdatedAt
			return nil
		})
		return u
	}
	c1 := wbcrud.MakeColumn("id", "c1", "name", "X", "type", "lead_field", "width", 200)
	// Seed columns so later equal-value patches are no-ops.
	time.Sleep(15 * time.Millisecond)
	if _, ch := run(wbcrud.UpdatePatch{Columns: cols(c1)}); !ch {
		t.Fatal("columns change not written")
	}
	base := stamp()
	unchanged := map[string]wbcrud.UpdatePatch{
		"empty body":       {},
		"same name":        {Name: str("a")},
		"same status":      {Status: str("draft")},
		"same description": {Description: str("")},
		"same columns":     {Columns: cols(c1.Clone())},
		"same filter {}":   {SetFilterCriteria: true, FilterCriteria: map[string]any{}},
	}
	for name, p := range unchanged {
		time.Sleep(15 * time.Millisecond)
		if _, ch := run(p); ch {
			t.Errorf("%s: reported changed", name)
		}
		if got := stamp(); !got.Equal(base) {
			t.Errorf("%s: updated_at stamped (%v -> %v)", name, base, got)
		}
	}
	changed := map[string]wbcrud.UpdatePatch{
		"name":    {Name: str("b")},
		"status":  {Status: str("paused")},
		"filter":  {SetFilterCriteria: true, FilterCriteria: map[string]any{"city": "Paris"}},
		"columns": {Columns: cols()},
	}
	prev := base
	for _, name := range []string{"name", "status", "filter", "columns"} {
		time.Sleep(15 * time.Millisecond)
		w, ch := run(changed[name])
		if !ch {
			t.Fatalf("%s: not written", name)
		}
		if got := stamp(); !got.After(prev) {
			t.Errorf("%s: updated_at not advanced", name)
		} else {
			prev = got
		}
		_ = w
	}
}
