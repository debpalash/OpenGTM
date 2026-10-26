package wbcrud

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// CSV export of workbooks.py export_workbook_csv: a UTF-8 BOM, a header row of
// column names and one row per matching workbook row, written with Python's
// csv.writer (QUOTE_MINIMAL, "\r\n"), with formula-injection guarding. Rows are
// streamed from the database to the writer in bounded chunks.

const (
	// maxExportSelected is the largest row_ids selection ("Selected export is limited to 1,000 rows").
	maxExportSelected = 1000
	// exportChunk is the size of the buffer between the CSV encoder and the response.
	exportChunk = 32 << 10
)

// ExportParams are the query parameters of GET /{id}/export.csv.
type ExportParams struct {
	ViewID    string
	Search    string
	ColumnIDs []string // nil or empty: every column
	// RowIDs restricts the export to these row ids (an empty list exports everything). Ids that do not fit
	// an int64 are passed as 0, which matches no row.
	RowIDs []int64
}

// pyDumps is json.dumps(v, ensure_ascii=False) with default separators, or
// with separators=(",", ":") when compact. v is a decodeDocument tree.
func pyDumps(v any, compact bool) string {
	var b bytes.Buffer
	pyDumpValue(&b, v, compact)
	return b.String()
}

func pyDumpValue(b *bytes.Buffer, v any, compact bool) {
	itemSep, keySep := ", ", ": "
	if compact {
		itemSep, keySep = ",", ":"
	}
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(t))
	case string:
		pyJSONString(b, t)
	case json.Number:
		b.WriteString(string(t))
	case float64:
		b.WriteString(pyFloat(t))
	case int:
		b.WriteString(strconv.Itoa(t))
	case int64:
		b.WriteString(strconv.FormatInt(t, 10))
	case []any:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteString(itemSep)
			}
			pyDumpValue(b, e, compact)
		}
		b.WriteByte(']')
	case *Obj:
		b.WriteByte('{')
		if t != nil {
			for i, k := range t.Keys() {
				if i > 0 {
					b.WriteString(itemSep)
				}
				pyJSONString(b, k)
				b.WriteString(keySep)
				e, _ := t.Get(k)
				pyDumpValue(b, e, compact)
			}
		}
		b.WriteByte('}')
	default:
		raw, err := json.Marshal(t)
		if err != nil {
			b.WriteString("null")
			return
		}
		b.Write(raw)
	}
}

// pyText is Python's str() of a JSON-decoded scalar.
func pyText(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	case json.Number:
		s := string(t)
		if !strings.ContainsAny(s, ".eE") {
			if n, ok := new(big.Int).SetString(s, 10); ok {
				return n.String() // "-0" is the int 0
			}
			return s
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil && !math.IsInf(f, 0) {
			return s
		}
		switch {
		case math.IsInf(f, 1):
			return "inf"
		case math.IsInf(f, -1):
			return "-inf"
		}
		return pyFloat(f)
	case float64:
		return pyFloat(t)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	}
	return pyDumps(v, true)
}

// ExportSafeValue is safe_value of export_workbook_csv: None is empty, dicts
// and lists are compact JSON, everything else str(value), and a text starting
// with = + - @ TAB or CR gets a leading apostrophe against formula injection.
func ExportSafeValue(v any) string {
	switch v.(type) {
	case nil:
		return ""
	case *Obj, []any:
		v = pyDumps(v, true)
	}
	s := pyText(v)
	if s != "" {
		switch s[0] {
		case '=', '+', '-', '@', '\t', '\r':
			return "'" + s
		}
	}
	return s
}

// exportHeaderCell is a header cell: the column name (or id when unnamed). Stub.
func exportHeaderCell(c Column) string {
	if n := c.Get("name"); truthy(n) {
		return pyText(n)
	}
	return pyText(c.Get("id"))
}

// writeCSVRow writes one csv.writer(lineterminator="\r\n") row: QUOTE_MINIMAL,
// quotes doubled; a row of one empty field is written as "" so it is not a
// blank line.
func writeCSVRow(w io.Writer, cells []string) error {
	var b bytes.Buffer
	if len(cells) == 1 && cells[0] == "" {
		b.WriteString(`""`)
	}
	for i, c := range cells {
		if i > 0 {
			b.WriteByte(',')
		}
		if strings.ContainsAny(c, ",\"\r\n") {
			b.WriteByte('"')
			b.WriteString(strings.ReplaceAll(c, `"`, `""`))
			b.WriteByte('"')
		} else {
			b.WriteString(c)
		}
	}
	b.WriteString("\r\n")
	_, err := w.Write(b.Bytes())
	return err
}

var notFilenameChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// ExportFilename is the Content-Disposition file name stem: runs of other
// characters become "_", leading and trailing "." and "_" are stripped, and an
// empty result is "workbook".
func ExportFilename(name string) string {
	if name == "" {
		name = "workbook"
	}
	s := strings.Trim(notFilenameChars.ReplaceAllString(name, "_"), "._")
	if s == "" {
		return "workbook"
	}
	return s
}

var laxInt = regexp.MustCompile(`^([+-]?)([0-9]+(?:_[0-9]+)*)(?:\.0+)?$`)

// ParseIntBig parses a query string like pydantic's lax int: surrounding
// whitespace, a sign, single underscores between digits and a ".0" fraction are
// accepted; the value is unbounded.
func ParseIntBig(s string) (*big.Int, bool) {
	m := laxInt.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return nil, false
	}
	return new(big.Int).SetString(m[1]+strings.ReplaceAll(m[2], "_", ""), 10)
}

// ParseIntLax is ParseIntBig for ids: a value that does not fit an int64
// parses as 0 (such an id matches no row but still counts toward the
// selection limit).
func ParseIntLax(s string) (int64, bool) {
	n, ok := ParseIntBig(s)
	if !ok {
		return 0, false
	}
	if !n.IsInt64() {
		return 0, true
	}
	return n.Int64(), true
}

// selectSQL is the query's FROM/WHERE/ORDER BY (no cursor, no limit) with a
// custom select list and optionally an id restriction.
func (q RowQuery) selectSQL(cols string, ids []int64) (string, []any) {
	where := append([]string{}, q.where...)
	args := append([]any{}, q.Args...)
	if len(ids) > 0 {
		args = append(args, ids)
		where = append(where, "id = ANY($"+strconv.Itoa(len(args))+"::bigint[])")
	}
	return "SELECT " + cols + " FROM workbook_rows WHERE " + strings.Join(where, " AND ") + " ORDER BY " + q.orderBy(), args
}

// exportCell is the value of one column in one row: lead_field and input
// columns read the row data, others the enrichment overlay's value.
func exportCell(c Column, id string, data *Obj, enrichments *Obj) any {
	if t, _ := c.Get("type").(string); t == "lead_field" || t == "input" {
		key := id
		if lf, ok := c.Get("lead_field").(string); ok && lf != "" {
			key = lf
		}
		v, _ := data.Get(key)
		return v
	}
	overlay, _ := enrichments.Get(id)
	if o, ok := overlay.(*Obj); ok {
		v, _ := o.Get("value")
		return v
	}
	return overlay
}

// objOrEmpty is `value or {}` for a decoded JSON column.
func objOrEmpty(raw []byte) *Obj {
	if len(raw) == 0 {
		return NewObj()
	}
	if v, err := decodeDocument(raw, true); err == nil {
		if o, ok := v.(*Obj); ok {
			return o
		}
	}
	return NewObj()
}

// ExportCSV streams the workbook's matching rows as CSV. Guards (404 unknown
// workbook or view, 400 unknown columns or oversized selection) are returned
// before open is called; open receives the file name stem and returns the
// writer for the body, after which the BOM, header and rows are written in
// chunks of exportChunk bytes. An error after open is a write or database
// error: the response is already underway.
func (s *Service) ExportCSV(ctx context.Context, ws, workbookID string, p ExportParams, open func(filename string) io.Writer) error {
	return s.WithTx(ctx, ws, func(tx pgx.Tx) error {
		wb, err := s.Owned(ctx, tx, ws, workbookID, false)
		if err != nil {
			return err
		}
		available := map[string]Column{}
		var order []string
		for _, c := range wb.Columns {
			v, ok := c.Lookup("id")
			if !ok || !truthy(v) {
				continue
			}
			id := pyText(v)
			if _, seen := available[id]; !seen {
				order = append(order, id)
			}
			available[id] = c
		}
		requested := p.ColumnIDs
		if len(requested) == 0 {
			requested = order
		}
		var unknown []string
		for _, id := range requested {
			if _, ok := available[id]; !ok {
				unknown = append(unknown, id)
			}
		}
		if len(unknown) > 0 {
			if len(unknown) > 10 {
				unknown = unknown[:10]
			}
			return bad400("Unknown columns: " + strings.Join(unknown, ", "))
		}
		cols := make([]Column, len(requested))
		for i, id := range requested {
			cols[i] = available[id]
		}

		q, err := s.detailQuery(ctx, tx, wb, p.ViewID, p.Search)
		if err != nil {
			return err
		}
		if len(p.RowIDs) > maxExportSelected {
			return bad400("Selected export is limited to 1,000 rows")
		}
		sql, args := q.selectSQL("data, enrichments", p.RowIDs)

		out := bufio.NewWriterSize(open(ExportFilename(wb.Name)), exportChunk)
		if _, err := out.WriteString("\ufeff"); err != nil {
			return err
		}
		cells := make([]string, len(cols))
		for i, c := range cols {
			if n := c.Get("name"); truthy(n) {
				cells[i] = pyText(n)
			} else {
				cells[i] = pyText(c.Get("id"))
			}
		}
		if len(cells) == 0 {
			cells = nil
		}
		if err := writeCSVRow(out, cells); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var data, enr []byte
			if err := rows.Scan(&data, &enr); err != nil {
				return err
			}
			d, e := objOrEmpty(data), objOrEmpty(enr)
			for i, c := range cols {
				cells[i] = ExportSafeValue(exportCell(c, pyText(c.Get("id")), d, e))
			}
			if err := writeCSVRow(out, cells); err != nil {
				return err
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		return out.Flush()
	})
}
