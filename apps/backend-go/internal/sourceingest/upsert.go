package sourceingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/automationhook"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

const (
	// MaxBatchRecords bounds one Upsert call (plan 04's workbook row ceiling,
	// wbcrud MaxRows 50000); connector pages are far smaller.
	MaxBatchRecords = 50000
	// MaxBatchBytes bounds the encoded size of one Upsert call (plan 04's
	// import body cap, 64 MiB), so a page cannot exceed what one import may hold.
	MaxBatchBytes = 64 << 20
)

var (
	ErrWorkbookNotFound = errors.New("sourceingest: workbook not found")
	ErrBatchTooLarge    = errors.New("sourceingest: batch exceeds the import row or byte cap")
)

// Upsert is upsert_source_records. It runs in the caller's transaction (which
// must be workspace-scoped) and never commits, so the row changes and the
// caller's cursor update land atomically.
func Upsert(ctx context.Context, tx pgx.Tx, workbookID, workspaceID, provider string, recs []Record, resetRows bool) (Summary, error) {
	var sum Summary
	if len(recs) > MaxBatchRecords {
		return sum, ErrBatchTooLarge
	}
	q := gen.New(tx)
	ok, err := q.SourceIngestWorkbookExists(ctx, gen.SourceIngestWorkbookExistsParams{WorkbookID: workbookID, WorkspaceID: workspaceID})
	if err != nil {
		return sum, err
	}
	if !ok {
		return sum, ErrWorkbookNotFound
	}
	if err := q.SourceIngestLock(ctx, "source_ingest:"+workbookID+":"+provider); err != nil {
		return sum, err
	}
	if resetRows {
		if err := q.SourceIngestDeleteRows(ctx, gen.SourceIngestDeleteRowsParams{WorkbookID: workbookID, WorkspaceID: workspaceID}); err != nil {
			return sum, err
		}
	}
	existingRows, err := q.SourceIngestExistingRows(ctx, gen.SourceIngestExistingRowsParams{WorkbookID: workbookID, WorkspaceID: workspaceID, Provider: provider})
	if err != nil {
		return sum, err
	}
	existing := make(map[string]gen.SourceIngestExistingRowsRow, len(existingRows))
	for _, r := range existingRows {
		existing[r.SourceRecordID] = r
	}
	maxPos, err := q.SourceIngestMaxPosition(ctx, gen.SourceIngestMaxPositionParams{WorkbookID: workbookID, WorkspaceID: workspaceID})
	if err != nil {
		return sum, err
	}
	nextPosition := int(maxPos) + 1

	sum.AddedRowIDs = []int64{}
	seen := make(map[string]struct{}, len(recs))
	total := 0
	for _, rec := range recs {
		recordID := strings.TrimSpace(rec.RecordID)
		if recordID == "" || rec.Provider != provider {
			sum.Skipped++
			continue
		}
		if _, dup := seen[recordID]; dup {
			sum.Skipped++
			continue
		}
		seen[recordID] = struct{}{}

		newPairs, err := newData(rec, provider, recordID)
		if err != nil {
			return sum, err
		}
		fetched := pgtype.Timestamptz{Time: rec.FetchedAt, Valid: true}
		row, found := existing[recordID]
		if !found {
			data := encodePairs(newPairs)
			if total += len(data); total > MaxBatchBytes {
				return sum, ErrBatchTooLarge
			}
			position := nextPosition
			if resetRows {
				position = rec.Rank
			}
			nextPosition++
			id, err := q.SourceIngestInsertRow(ctx, gen.SourceIngestInsertRowParams{
				WorkspaceID: workspaceID, WorkbookID: workbookID, Position: int32(position), Data: data,
				Provider: provider, RecordID: recordID, Rank: int32(rec.Rank), FetchedAt: fetched,
			})
			if err != nil {
				return sum, err
			}
			existing[recordID] = gen.SourceIngestExistingRowsRow{ID: id, SourceRecordID: recordID, Data: data,
				SourceRank: pgtype.Int4{Int32: int32(rec.Rank), Valid: true}, SourceFetchedAt: fetched}
			sum.Added++
			sum.AddedRowIDs = append(sum.AddedRowIDs, int64(id))
			continue
		}

		merged := mergePairs(parseObject(row.Data), newPairs)
		mergedText := encodePairs(merged)
		if total += len(mergedText); total > MaxBatchBytes {
			return sum, ErrBatchTooLarge
		}
		changed := !jsonEqual(row.Data, mergedText) ||
			!row.SourceRank.Valid || int(row.SourceRank.Int32) != rec.Rank ||
			!row.SourceFetchedAt.Valid || !row.SourceFetchedAt.Time.Equal(rec.FetchedAt)
		if !changed {
			sum.Skipped++
			continue
		}
		pos := pgtype.Int4{}
		if resetRows {
			pos = pgtype.Int4{Int32: int32(rec.Rank), Valid: true}
		}
		if err := q.SourceIngestUpdateRow(ctx, gen.SourceIngestUpdateRowParams{
			Data: mergedText, Rank: int32(rec.Rank), FetchedAt: fetched, Position: pos,
			ID: row.ID, WorkbookID: workbookID, WorkspaceID: workspaceID,
		}); err != nil {
			return sum, err
		}
		row.Data = mergedText
		row.SourceRank = pgtype.Int4{Int32: int32(rec.Rank), Valid: true}
		row.SourceFetchedAt = fetched
		existing[recordID] = row
		sum.Updated++
	}
	return sum, nil
}

// UpsertNotify is Upsert followed by the on_row_added automation event for the
// rows it added (Python emits it from the caller after the page commit; Go runs
// it inside the caller's transaction, in a savepoint, so a hook failure never
// breaks the page). A nil hook skips the event.
func UpsertNotify(ctx context.Context, tx pgx.Tx, hook automationhook.Hook, workbookID, workspaceID, provider string, recs []Record, resetRows bool) (Summary, error) {
	sum, err := Upsert(ctx, tx, workbookID, workspaceID, provider, recs, resetRows)
	if err != nil {
		return sum, err
	}
	if hook != nil && len(sum.AddedRowIDs) > 0 {
		if _, err := hook.RowsAdded(ctx, tx, workspaceID, workbookID, sum.AddedRowIDs); err != nil {
			return sum, fmt.Errorf("sourceingest: row-added hook: %w", err)
		}
	}
	return sum, nil
}

type pair struct {
	key string
	val json.RawMessage
}

// newData is dict(record.data) plus the `_source` key (provider, record_id,
// rank, url, fetched_at in that order). Go maps are unordered, so data keys are
// sorted; `_source` always comes last.
func newData(rec Record, provider, recordID string) ([]pair, error) {
	keys := make([]string, 0, len(rec.Data))
	for k := range rec.Data {
		if k != "_source" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	pairs := make([]pair, 0, len(keys)+1)
	for _, k := range keys {
		b, err := marshal(rec.Data[k])
		if err != nil {
			return nil, fmt.Errorf("sourceingest: record %q field %q: %w", recordID, k, err)
		}
		pairs = append(pairs, pair{k, b})
	}
	var url any
	if rec.SourceURL != nil {
		url = *rec.SourceURL
	}
	var sb bytes.Buffer
	sb.WriteByte('{')
	for i, f := range []struct {
		k string
		v any
	}{{"provider", provider}, {"record_id", recordID}, {"rank", rec.Rank}, {"url", url}, {"fetched_at", isoformat(rec.FetchedAt)}} {
		if i > 0 {
			sb.WriteByte(',')
		}
		kb, _ := marshal(f.k)
		vb, err := marshal(f.v)
		if err != nil {
			return nil, err
		}
		sb.Write(kb)
		sb.WriteByte(':')
		sb.Write(vb)
	}
	sb.WriteByte('}')
	return append(pairs, pair{"_source", sb.Bytes()}), nil
}

func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(buf.Bytes()), nil
}

// parseObject reads a JSON object keeping key order; anything else is empty
// (Python `row.data or {}`).
func parseObject(text string) []pair {
	dec := json.NewDecoder(strings.NewReader(text))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil
	}
	var out []pair
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return out
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return out
		}
		out = append(out, pair{kt.(string), raw})
	}
	return out
}

// mergePairs is {**old, **new}: old key order, replaced values in place, new
// keys appended.
func mergePairs(old, add []pair) []pair {
	out := append([]pair(nil), old...)
	idx := make(map[string]int, len(out))
	for i, p := range out {
		idx[p.key] = i
	}
	for _, p := range add {
		if i, ok := idx[p.key]; ok {
			out[i].val = p.val
			continue
		}
		idx[p.key] = len(out)
		out = append(out, p)
	}
	return out
}

func encodePairs(ps []pair) string {
	var sb strings.Builder
	sb.WriteByte('{')
	for i, p := range ps {
		if i > 0 {
			sb.WriteByte(',')
		}
		kb, _ := marshal(p.key)
		sb.Write(kb)
		sb.WriteByte(':')
		sb.Write(p.val)
	}
	sb.WriteByte('}')
	return sb.String()
}

// jsonEqual is Python dict equality on two JSON texts (key order ignored,
// 1 == 1.0).
func jsonEqual(a, b string) bool {
	av, aok := decodeNum(a)
	bv, bok := decodeNum(b)
	if !aok || !bok {
		return a == b
	}
	return valEqual(av, bv)
}

func decodeNum(s string) (any, bool) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil && err != io.EOF {
		return nil, false
	}
	return v, true
}

func valEqual(a, b any) bool {
	switch x := a.(type) {
	case json.Number:
		y, ok := b.(json.Number)
		return ok && numEqual(x, y)
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, xv := range x {
			yv, ok := y[k]
			if !ok || !valEqual(xv, yv) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !valEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(a, b)
}

func numEqual(a, b json.Number) bool {
	if a == b {
		return true
	}
	ai, aerr := strconv.ParseInt(string(a), 10, 64)
	bi, berr := strconv.ParseInt(string(b), 10, 64)
	if aerr == nil && berr == nil {
		return ai == bi
	}
	af, e1 := strconv.ParseFloat(string(a), 64)
	bf, e2 := strconv.ParseFloat(string(b), 64)
	return e1 == nil && e2 == nil && af == bf
}

// isoformat is Python datetime.isoformat() for an aware datetime: a six-digit
// fraction only when the microseconds are non-zero, "+00:00" for UTC.
func isoformat(t time.Time) string {
	s := t.Format("2006-01-02T15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	return s + t.Format("-07:00")
}
