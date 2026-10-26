package autoactions

import (
	"context"
	"fmt"
	"strconv"
	"time"
	"unicode"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

// reEnrichProviderTimeout is the bounded provider timeout of the nested run
// (§4 nested-timeout safety).
const reEnrichProviderTimeout = 10 * time.Second

// RowRunner runs the given columns for the given rows of one workbook
// (run_workbook_enrichment). A row id is an int64 or, when the id is not a
// digit string, the raw string.
type RowRunner func(ctx context.Context, ws, workbookID string, columnIDs []string, rowIDs []any, providerTimeout time.Duration) (workbook.RunSummary, error)

// WorkbookRowRunner binds RowRunner to the Go run loop (workbook.RunRows).
// A row id that is not an integer matches no row: it is a failed action, not a
// crash.
func WorkbookRowRunner(wd workbook.Deps) RowRunner {
	return func(ctx context.Context, ws, wb string, cols []string, rows []any, to time.Duration) (workbook.RunSummary, error) {
		ids := make([]int32, 0, len(rows))
		for _, r := range rows {
			n, ok := r.(int64)
			if !ok || n < 1 || n > 1<<31-1 {
				return workbook.RunSummary{}, fmt.Errorf("invalid row id %v", r)
			}
			ids = append(ids, int32(n))
		}
		return workbook.RunRows(ctx, wd, ws, wb, cols, ids, to)
	}
}

// pyRowID is `int(row_id) if str(row_id).isdigit() else row_id`: Unicode
// decimal digits count (int("٣") is 3); anything else stays a string.
func pyRowID(s string) any {
	if s == "" {
		return s
	}
	var n int64
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return s
		}
		// Decimal digits come in aligned runs of ten starting at zero.
		z := r
		for unicode.IsDigit(z - 1) {
			z--
		}
		d := int64(r-z) % 10
		if n > (1<<62)/10 {
			return s
		}
		n = n*10 + d
	}
	return n
}

// ReEnrich is _act_re_enrich: re-run the configured columns for one row with a
// 10 s provider timeout. Charging is the engine's (projected cost, reserved
// before the action). The run's own failure is a failed result, never a panic
// and never a retry.
func ReEnrich(ctx context.Context, d Deps, ws, workbookID, rowID string, cfg map[string]any, cols []Column) (res Result) {
	ids, _ := cfg["column_ids"].([]any)
	if len(ids) == 0 {
		return Result{Status: "skipped", SkipReason: "no_target", Summary: "no column_ids"}
	}
	have := map[string]bool{}
	for _, c := range cols {
		if id, ok := colID(c); ok {
			have[id] = true
		}
	}
	var present []string
	for _, v := range ids {
		if s, ok := v.(string); ok && have[s] {
			present = append(present, s)
		}
	}
	if len(present) == 0 {
		return Result{Status: "skipped", SkipReason: "no_target", Summary: "none of the configured columns exist in this workbook"}
	}
	if d.RunRows == nil {
		return failed("re_enrich is not available: no workbook runner configured")
	}
	defer func() {
		if r := recover(); r != nil {
			res = failed(fmt.Sprint(r))
		}
	}()
	sum, err := d.RunRows(ctx, ws, workbookID, present, []any{pyRowID(rowID)}, reEnrichProviderTimeout)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return failed(cerr.Error())
		}
		return failed(err.Error())
	}
	if sum.Error != "" {
		return failed(sum.Error)
	}
	summary := "re_enrich: " + strconv.Itoa(sum.Completed) + " cells completed, " + strconv.Itoa(sum.Errors) + " errors"
	if sum.BudgetExceeded {
		summary += " [workbook budget cap hit]"
	}
	if sum.Errors == 0 {
		return Result{Status: "success", Summary: summary}
	}
	if cerr := ctx.Err(); cerr != nil {
		return failed(cerr.Error())
	}
	return Result{Status: "failed", Summary: summary, Error: strconv.Itoa(sum.Errors) + " cell errors"}
}
