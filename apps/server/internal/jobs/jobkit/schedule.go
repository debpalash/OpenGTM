package jobkit

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Stamp is the SQL for binding an aware instant (parameter n) into a
// "timestamp without time zone" column the way psycopg does for the aware
// UTC datetimes Python writes: through timestamptz, so PostgreSQL applies the
// session time zone. Every domain timestamp the Python handlers write goes
// through it, which keeps the stored value identical whatever the zone.
func Stamp(n int) string { return fmt.Sprintf(StampFormat, n) }

// StampFormat is the Sprintf template behind Stamp, for SQL assembled with
// several placeholders.
const StampFormat = "$%d::timestamptz::timestamp"

// IsoFormat is datetime.isoformat() for an aware UTC datetime: the fractional
// part is omitted when the microsecond is zero and the offset is +00:00.
// Python fire keys embed it, so Go occurrences must spell it identically to
// stay single-flight with Python-created ones.
func IsoFormat(t time.Time) string {
	t = t.UTC()
	s := fmt.Sprintf("%04d-%02d-%02dT%02d:%02d:%02d", t.Year(), int(t.Month()), t.Day(), t.Hour(), t.Minute(), t.Second())
	if us := t.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	return s + "+00:00"
}

// Micros truncates to microseconds, the resolution of Python datetimes and of
// PostgreSQL timestamps.
func Micros(t time.Time) time.Time { return t.Truncate(time.Microsecond) }

// ClampMinutes is `max(lo, min(int(v or def), hi))`, the interval clamp the
// schedulers apply to a stored minute count (zero or NULL means def).
func ClampMinutes(v *int64, def, lo, hi int64) int64 {
	n := def
	if v != nil && *v != 0 {
		n = *v
	}
	return max(lo, min(n, hi))
}

// CancelPending cancels pending (never processing) jobs of a type whose
// fire_key matches a SQL LIKE pattern. Like the Python schedulers it sets only
// the status, and the pattern is deliberately NOT escaped: an '_' or '%' in an
// identifier behaves as a wildcard here exactly as it does in Python.
func CancelPending(ctx context.Context, tx pgx.Tx, jobType, likePattern string) (int64, error) {
	tag, err := tx.Exec(ctx, `UPDATE jobs SET status = 'cancelled'
		WHERE type = $1 AND status = 'pending' AND fire_key LIKE $2`, jobType, likePattern)
	if err != nil {
		return 0, fmt.Errorf("cancel pending %s jobs: %w", jobType, err)
	}
	return tag.RowsAffected(), nil
}

// Mirror describes one of the non-RLS "schedule mirror" tables
// (audience_schedules, playbook_schedules, retention_schedules): identifiers
// and timing only, read by the scheduler to rebuild occurrences after a
// restart. WorkspaceCol is empty when the key is itself the workspace id.
type Mirror struct {
	Table        string
	KeyCol       string
	WorkspaceCol string
	EnabledCol   string
	NextCol      string
}

// Upsert records the mirror row. The row is written only when something
// changed, like the ORM's dirty tracking, so updated_at (onupdate=now()) is
// not bumped by a no-op reschedule.
func (m Mirror) Upsert(ctx context.Context, tx pgx.Tx, key, workspaceID string, enabled bool, next *time.Time) error {
	cols, vals, sets, diffs := m.KeyCol, "$1", "", ""
	args := []any{key}
	add := func(col, valExpr string, arg any) {
		args = append(args, arg)
		cols += ", " + col
		vals += ", " + fmt.Sprintf(valExpr, len(args))
		if sets != "" {
			sets += ", "
			diffs += " OR "
		}
		sets += fmt.Sprintf("%s = EXCLUDED.%s", col, col)
		diffs += fmt.Sprintf("%s.%s IS DISTINCT FROM EXCLUDED.%s", m.Table, col, col)
	}
	if m.WorkspaceCol != "" {
		add(m.WorkspaceCol, "$%d", workspaceID)
	}
	add(m.EnabledCol, "$%d", enabled)
	add(m.NextCol, "$%d::timestamptz::timestamp", next)
	sql := fmt.Sprintf(`INSERT INTO %[1]s (%[2]s) VALUES (%[3]s)
ON CONFLICT (%[4]s) DO UPDATE SET %[5]s, updated_at = now() WHERE %[6]s`,
		m.Table, cols, vals, m.KeyCol, sets, diffs)
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		return fmt.Errorf("update %s: %w", m.Table, err)
	}
	return nil
}

// Delete removes the mirror row for key.
func (m Mirror) Delete(ctx context.Context, tx pgx.Tx, key string) error {
	if _, err := tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE %s = $1`, m.Table, m.KeyCol), key); err != nil {
		return fmt.Errorf("delete from %s: %w", m.Table, err)
	}
	return nil
}
