package govaudit

import (
	"context"
	"io"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// BatchSize is the keyset batch of the export; memory is bounded by it.
const BatchSize = 500

// batchHook observes each non-empty batch size (tests only).
var batchHook func(n int)

var csvHeader = []string{"id", "created_at", "actor_user_id", "actor_role", "method", "route", "resource_path", "response_status", "outcome", "request_id", "metadata_json"}

// safe neutralises spreadsheet formulas: text starting with = + - @ TAB or CR
// gets a leading apostrophe (Python safe()).
func safe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// csvField quotes like Python csv QUOTE_MINIMAL: only when the field holds a
// comma, double quote, CR or LF.
func csvField(s string) string {
	if strings.ContainsAny(s, ",\"\r\n") {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}

func csvLine(fields []string) string {
	out := make([]string, len(fields))
	for i, f := range fields {
		out[i] = csvField(f)
	}
	return strings.Join(out, ",") + "\r\n"
}

// ExportCSV streams ws's audit events (newest first) to w: UTF-8 BOM, header,
// CRLF rows, formula-safe cells. Rows are read in BatchSize keyset batches,
// each in its own short workspace transaction, and written after the
// transaction closes, so a slow client holds neither a snapshot nor more than
// one batch in memory. Stops at the first write error or ctx cancellation.
func ExportCSV(ctx context.Context, pool *pgxpool.Pool, ws string, f Filter, w io.Writer) error {
	if _, err := io.WriteString(w, "\xef\xbb\xbf"+csvLine(csvHeader)); err != nil {
		return err
	}
	p := params(ws, f, BatchSize)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var rows []gen.ListGovernanceAuditEventsRow
		err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) (err error) {
			rows, err = gen.New(tx).ListGovernanceAuditEvents(ctx, p)
			return err
		})
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		if batchHook != nil {
			batchHook(len(rows))
		}
		for _, r := range rows {
			e, err := toEvent(r)
			if err != nil {
				return err
			}
			actor := ""
			if e.ActorUserID != nil {
				actor = strconv.Itoa(int(*e.ActorUserID))
			}
			line := csvLine([]string{
				safe(e.ID), safe(isoformat(e.CreatedAt)), safe(actor), safe(e.ActorRole), safe(e.Method),
				safe(e.Route), safe(e.ResourcePath), safe(strconv.Itoa(int(e.ResponseStatus))),
				safe(e.Outcome), safe(e.RequestID), safe(string(e.Metadata)),
			})
			if _, err := io.WriteString(w, line); err != nil {
				return err
			}
		}
		if len(rows) < BatchSize {
			return nil
		}
		last := rows[len(rows)-1]
		p.CursorCreatedAt = last.CreatedAt
		p.CursorID = pgtype.Text{String: last.ID, Valid: true}
	}
}
