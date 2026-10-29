package wbcrud

import "io"

// WriteCSVRow writes one row exactly like Python's csv.writer (QUOTE_MINIMAL,
// "\r\n"); unlike encoding/csv it does not quote a field with a leading space.
func WriteCSVRow(w io.Writer, cells []string) error { return writeCSVRow(w, cells) }
