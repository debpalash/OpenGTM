package wbcrud

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// CountRows counts the workbook's v2 rows that the saved view (404 "Workbook
// view not found" when missing) and the search term select, like
// _workbook_rows_query followed by count(WorkbookRow.id). Legacy lead-store
// workbooks without rows count 0, exactly as the estimate route does.
func (s *Service) CountRows(ctx context.Context, tx pgx.Tx, wb Workbook, viewID, search string) (int, error) {
	q, err := s.detailQuery(ctx, tx, wb, viewID, search)
	if err != nil {
		return 0, err
	}
	sql, args := q.CountSQL()
	var n int64
	if err := tx.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		return 0, err
	}
	return int(n), nil
}
