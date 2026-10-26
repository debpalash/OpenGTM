package wbcrud

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// CSV import of routers/workbooks.py (preview_csv_import / import_csv_leads).
// Import runs entirely on the caller's workspace transaction: columns, rows,
// source_config and the automation hook commit together or not at all.

const (
	// MaxImportColumns is the "CSV has more than 500 columns" bound.
	MaxImportColumns = 500

	noImportableRows = "CSV has no importable rows"
	tooManyColumns   = "CSV has more than 500 columns"
)

// Importer runs imports. BatchHook (tests only) is called after each inserted
// batch; an error aborts the import.
type Importer struct {
	*Service
	BatchHook func(batch int) error
}

func (s *Service) csvPlanner() *CSVPlanner { return NewCSVPlanner(s.leadFieldList()) }

func fileNameOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// PreviewImport is analyze_csv_import over the stored columns; it writes nothing.
func (s *Service) PreviewImport(w Workbook, req *ImportRequest) *Obj {
	p := s.csvPlanner()
	prep := p.Prepare(req.Rows, w.Columns, req.Mapping, req.CreateColumns)
	return p.Analyze(req.Rows, prep, fileNameOrEmpty(req.FileName), req.SourceSystem)
}

// pyIsoUTC is datetime.now(timezone.utc).isoformat().
func pyIsoUTC(t time.Time) string {
	u := t.UTC()
	s := u.Format("2006-01-02T15:04:05")
	if us := u.Nanosecond() / 1000; us != 0 {
		s += u.Format(".000000")
	}
	return s + "+00:00"
}

// Import appends the CSV rows to w (loaded FOR UPDATE by the caller). req.Rows
// is released as soon as the rows are normalised, and every normalised row is
// dropped once its batch is written, so memory stays proportional to one batch
// beyond the decoded upload itself. A *GuardError is a 400 the route answers
// (the caller rolls the transaction back); any other error is a 500.
func (im Importer) Import(ctx context.Context, tx pgx.Tx, ws string, w Workbook, req *ImportRequest) (*Obj, *GuardError, error) {
	p := im.csvPlanner()
	prep := p.Prepare(req.Rows, w.Columns, req.Mapping, req.CreateColumns)
	analysis := p.Analyze(req.Rows, prep, fileNameOrEmpty(req.FileName), req.SourceSystem)
	req.Rows = nil
	if len(prep.Rows) == 0 {
		return nil, &GuardError{Status: 400, Detail: noImportableRows}, nil
	}
	if len(prep.Resolved) > MaxImportColumns {
		return nil, &GuardError{Status: 400, Detail: tooManyColumns}, nil
	}

	q := gen.New(tx)
	var seen map[string]struct{}
	if req.Dedupe {
		var err error
		if seen, err = existingIdentities(ctx, tx, w.ID); err != nil {
			return nil, nil, err
		}
	}
	// New rows go at max(position)+1 (0 for an empty workbook): the query
	// answers max or -1 and positions are start + 1...
	maxPos, err := q.ImportMaxRowPosition(ctx, w.ID)
	if err != nil {
		return nil, nil, err
	}

	var (
		newIDs  []int64
		skipped int
		batches int
		batch   = make([]string, 0, insertChunk)
	)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		ids, err := q.ImportAppendRows(ctx, gen.ImportAppendRowsParams{
			WorkspaceID: ws, WorkbookID: w.ID, StartPosition: maxPos + int32(len(newIDs)), Datas: batch,
		})
		if err != nil {
			return err
		}
		if len(ids) != len(batch) { // the workbook is not visible in this workspace
			return ErrNotFound
		}
		for _, id := range ids {
			newIDs = append(newIDs, int64(id))
		}
		batch = batch[:0]
		n := batches
		batches++
		if im.BatchHook != nil {
			return im.BatchHook(n)
		}
		return nil
	}
	for i, row := range prep.Rows {
		prep.Rows[i] = nil
		if req.Dedupe {
			if id := RowIdentity(row); id != "" {
				if _, dup := seen[id]; dup {
					skipped++
					continue
				}
				seen[id] = struct{}{}
			}
		}
		batch = append(batch, PyDumps(row, false))
		if len(batch) == insertChunk {
			if err := flush(); err != nil {
				return nil, nil, err
			}
		}
	}
	if err := flush(); err != nil {
		return nil, nil, err
	}
	prep.Rows = nil

	sc := NewObj()
	if old, ok := w.SourceConfig.(*Obj); ok && old != nil {
		for _, k := range old.Keys() {
			v, _ := old.Get(k)
			sc.Set(k, v)
		}
	}
	last := NewObj()
	if req.FileName != nil {
		last.Set("file_name", *req.FileName)
	} else {
		last.Set("file_name", nil)
	}
	src, _ := analysis.Get("source_system")
	coll, _ := analysis.Get("collisions")
	last.Set("source_system", src)
	last.Set("mapping", prep.ResolvedObj())
	last.Set("collisions", coll)
	last.Set("rows", len(newIDs))
	last.Set("columns_added", len(prep.Added))
	now := time.Now
	if im.Now != nil {
		now = im.Now
	}
	last.Set("imported_at", pyIsoUTC(now()))
	sc.Set("last_csv_import", last)

	colsJSON, err := marshalNoEscape(prep.Columns)
	if err != nil {
		return nil, nil, err
	}
	scJSON, err := marshalNoEscape(sc)
	if err != nil {
		return nil, nil, err
	}
	srcType := w.SourceType
	if srcType == "empty" {
		srcType = "csv"
	}
	if err := q.ImportUpdateWorkbook(ctx, gen.ImportUpdateWorkbookParams{
		ColumnsConfig: json.RawMessage(colsJSON), SourceType: srcType, SourceConfig: json.RawMessage(scJSON),
		WorkbookID: w.ID, WorkspaceID: ws,
	}); err != nil {
		return nil, nil, err
	}

	if len(newIDs) > 0 && im.Hook != nil {
		_, _ = im.Hook.RowsAdded(ctx, tx, ws, w.ID, newIDs) // errors are logged and swallowed by the hook
	}
	total, err := q.CountWorkbookRows(ctx, gen.CountWorkbookRowsParams{WorkbookID: w.ID, WorkspaceID: ws})
	if err != nil {
		return nil, nil, err
	}

	created := make([]any, 0, len(prep.Added))
	for _, c := range prep.Added {
		created = append(created, pyStr(c.Get("name")))
	}
	out := NewObj()
	out.Set("created", len(newIDs))
	out.Set("added", len(newIDs))
	out.Set("skipped_duplicates", skipped)
	out.Set("total_rows", int(total))
	out.Set("columns_added", created)
	out.Set("mapping", prep.ResolvedObj())
	out.Set("analysis", analysis)
	return out, nil, nil
}
