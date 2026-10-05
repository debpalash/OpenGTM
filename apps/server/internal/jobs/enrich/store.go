package enrich

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/pycompat"
)

// optString is a nullable text parameter.
func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// metadataJSON is the cell_metadata column. SQLAlchemy stores Python None in a
// JSON column as the JSON literal null (not SQL NULL), which readers of the
// column can observe, so the same is written here.
func metadataJSON(w cellWrite, withEvidence bool) (string, error) {
	if len(w.skipped) == 0 && (!withEvidence || w.evidence == nil) {
		return "null", nil
	}
	m := pycompat.NewMap()
	if len(w.skipped) > 0 {
		list := make([]any, len(w.skipped))
		for i, s := range w.skipped {
			list[i] = obj("provider", s.provider, "reason", s.reason)
		}
		_ = m.Set("skipped_providers", list)
	}
	if withEvidence && w.evidence != nil {
		_ = m.Set("evidence", w.evidence)
	}
	b, err := pycompat.Dumps(m, pycompat.DumpOptions{EnsureASCII: true})
	return string(b), err
}

// writeCell is _set_enrichment: upsert the workbook_enrichments overlay row and
// mirror the cell into workbook_rows.enrichments so it survives a reload. The
// caller commits, under the job lease.
func writeCell(ctx context.Context, tx pgx.Tx, ws, wb string, rowID, leadID int64, colID string, w cellWrite) error {
	meta, err := metadataJSON(w, true)
	if err != nil {
		return err
	}
	var value *string
	if w.status == "complete" {
		value = w.value
	}
	provider := optString(w.provider)
	if w.status != "complete" {
		provider = nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO workbook_enrichments
			(workbook_id, workspace_id, lead_id, column_id, value, status, provider, error, cell_metadata)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::json)
		ON CONFLICT (workbook_id, lead_id, column_id) DO UPDATE SET
			value = EXCLUDED.value, status = EXCLUDED.status, provider = EXCLUDED.provider,
			error = EXCLUDED.error, cell_metadata = EXCLUDED.cell_metadata, updated_at = now()`,
		wb, ws, leadID, colID, value, w.status, provider, optString(w.err), meta); err != nil {
		return err
	}

	// The row mirror carries only the keys the rows endpoint reads.
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT enrichments::text FROM workbook_rows
		WHERE workbook_id = $1 AND id = $2 FOR UPDATE`, wb, rowID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	overlay, err := decodeObject(raw)
	if err != nil {
		return err
	}
	overlay = overlay.Copy()
	var mv, mp, me any
	if value != nil {
		mv = *value
	}
	if provider != nil {
		mp = *provider
	}
	if w.err != "" {
		me = w.err
	}
	cell := obj("value", mv, "status", w.status, "provider", mp, "error", me)
	if len(w.skipped) > 0 {
		list := make([]any, len(w.skipped))
		for i, s := range w.skipped {
			list[i] = obj("provider", s.provider, "reason", s.reason)
		}
		_ = cell.Set("skipped_providers", list)
	}
	_ = overlay.Set(colID, cell)
	enc, err := pycompat.Dumps(overlay, pycompat.DumpOptions{EnsureASCII: true})
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE workbook_rows SET enrichments = $3::json, updated_at = now()
		WHERE workbook_id = $1 AND id = $2`, wb, rowID, string(enc))
	return err
}

// recordAttempt is planner.record_attempt: one atomic provider_stats upsert.
// Cooldown timestamps are written the way Python's aware-UTC datetimes land
// in a naive column (converted through the session time zone) and read back
// as UTC in cooldowns().
func (w *Worker) recordAttempt(ctx context.Context, a attemptRecord) error {
	hits, conf, cost := 0, 0.0, 0.0
	if a.success {
		hits, conf = 1, a.confidence
		cost = baseCost(a.provider, w.plugin(a.provider))
	}
	cooldown := 0.0
	switch {
	case a.rateLimited:
		cooldown = 300
	case a.timedOut:
		cooldown = 180
	}
	_, err := w.pool.Exec(ctx, `INSERT INTO provider_stats AS s
			(provider, field, attempts, hits, total_confidence, total_latency_ms, total_cost_usd,
			 cooldown_until, updated_at, accuracy_samples)
		VALUES ($1, $2, 1, $3, $4, $5, $6,
			CASE WHEN $7::float8 > 0 THEN (now() + make_interval(secs => $7::float8))::timestamp END,
			now()::timestamp, 0)
		ON CONFLICT (provider, field) DO UPDATE SET
			attempts = coalesce(s.attempts, 0) + 1,
			hits = coalesce(s.hits, 0) + $3,
			total_confidence = coalesce(s.total_confidence, 0) + $4,
			total_latency_ms = coalesce(s.total_latency_ms, 0) + $5,
			total_cost_usd = coalesce(s.total_cost_usd, 0) + $6,
			updated_at = now()::timestamp,
			cooldown_until = coalesce(EXCLUDED.cooldown_until, s.cooldown_until)`,
		a.provider, a.field, hits, conf, a.latencyMS, cost, cooldown)
	return err
}
