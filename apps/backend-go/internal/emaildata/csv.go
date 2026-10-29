package emaildata

import (
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// CSVHeader is the first line of every export.
const CSVHeader = "Name,Email,Tags,Notes,Source Task ID,Created At,Used\n"

// ExportFilename is the Content-Disposition filename Python builds from the
// local clock (`export_%Y%m%d_%H%M%S.csv`).
func ExportFilename(now time.Time) string {
	return "export_" + now.Format("20060102_150405") + ".csv"
}

// WriteCSV writes the header followed by rows.
func WriteCSV(w io.Writer, rows []Row) error {
	if _, err := io.WriteString(w, CSVHeader); err != nil {
		return err
	}
	return writeCSVRows(w, rows)
}

var (
	quoteDoubler = strings.NewReplacer(`"`, `""`)
	notesEscaper = strings.NewReplacer(`"`, `""`, "\n", " ")
)

// writeCSVRows reproduces Python's f-string rows: every field quoted, `"`
// doubled (formula prefixes = + - @ TAB CR LF get an apostrophe, the plan 04
// workbook export guard; Python has none, plan 07 Task 14 deviation), newlines of `notes` (only) turned into a space, NULL as empty, a 0 or
// NULL source link as empty, used as Yes/No.
func writeCSVRows(w io.Writer, rows []Row) error {
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(`"`)
		b.WriteString(quoteDoubler.Replace(safe(r.Name)))
		b.WriteString(`","`)
		b.WriteString(quoteDoubler.Replace(safe(r.Email)))
		b.WriteString(`","`)
		b.WriteString(quoteDoubler.Replace(safe(r.Tags)))
		b.WriteString(`","`)
		b.WriteString(notesEscaper.Replace(safe(r.Notes)))
		b.WriteString(`","`)
		if r.SourceLinkID != nil && *r.SourceLinkID != 0 {
			b.WriteString(strconv.FormatInt(*r.SourceLinkID, 10))
		}
		b.WriteString(`","`)
		b.WriteString(deref(r.CreatedAt))
		b.WriteString(`","`)
		if r.IsUsed != nil && *r.IsUsed {
			b.WriteString("Yes")
		} else {
			b.WriteString("No")
		}
		b.WriteString("\"\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// safe is deref plus the spreadsheet formula guard shared with the workbook export.
func safe(s *string) string { return wbcrud.ExportSafeValue(deref(s)) }
