package sourcehealth

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// Flags are the two Python master flags the summary echoes (both default off).
type Flags struct{ Enforce, ProbeEnabled bool }

func fromGen(g gen.SourceHealth) *Row {
	r := &Row{
		Name: g.Name, State: g.State,
		ConsecutiveZero: int(g.ConsecutiveZero), ConsecutiveNonzero: int(g.ConsecutiveNonzero),
		Probes: int(g.Probes), ManualOverride: g.ManualOverride,
		LastProbeAt: ts(g.LastProbeAt), LastOkAt: ts(g.LastOkAt), LastOutageAt: ts(g.LastOutageAt),
		DisabledAt: ts(g.DisabledAt), UpdatedAt: ts(g.UpdatedAt),
	}
	if g.LastYield.Valid {
		v := int(g.LastYield.Int32)
		r.LastYield = &v
	}
	if g.EwmaYield.Valid {
		v := g.EwmaYield.Float64
		r.EwmaYield = &v
	}
	if g.DisabledReason.Valid {
		v := g.DisabledReason.String
		r.DisabledReason = &v
	}
	return r
}

// ts reads a naive timestamp as naive UTC (the column has no zone; the Python
// worker writes UTC).
func ts(p pgtype.Timestamp) *time.Time {
	if !p.Valid {
		return nil
	}
	t := time.Date(p.Time.Year(), p.Time.Month(), p.Time.Day(), p.Time.Hour(), p.Time.Minute(), p.Time.Second(), p.Time.Nanosecond(), time.UTC)
	return &t
}

func tsArg(t *time.Time) pgtype.Timestamp {
	if t == nil {
		return pgtype.Timestamp{}
	}
	return pgtype.Timestamp{Time: *t, Valid: true}
}

func upsert(ctx context.Context, q *gen.Queries, r *Row) error {
	p := gen.SHUpsertParams{
		Name: r.Name, State: r.State,
		ConsecutiveZero: int32(r.ConsecutiveZero), ConsecutiveNonzero: int32(r.ConsecutiveNonzero),
		Probes: int32(r.Probes), ManualOverride: r.ManualOverride,
		LastProbeAt: tsArg(r.LastProbeAt), LastOkAt: tsArg(r.LastOkAt), LastOutageAt: tsArg(r.LastOutageAt),
		DisabledAt: tsArg(r.DisabledAt),
	}
	if r.LastYield != nil {
		p.LastYield = pgtype.Int4{Int32: int32(*r.LastYield), Valid: true}
	}
	if r.EwmaYield != nil {
		p.EwmaYield = pgtype.Float8{Float64: *r.EwmaYield, Valid: true}
	}
	if r.DisabledReason != nil {
		p.DisabledReason = pgtype.Text{String: *r.DisabledReason, Valid: true}
	}
	return q.SHUpsert(ctx, p)
}

// Summary is health_summary (GET /sources/health): rows ordered by name.
func Summary(ctx context.Context, tx pgx.Tx, flags Flags) (*Obj, error) {
	rows, err := gen.New(tx).SHAll(ctx)
	if err != nil {
		return nil, err
	}
	byState := wbcrud.NewObj()
	counts := map[string]int{}
	sources := make([]any, 0, len(rows))
	for _, g := range rows {
		if _, ok := counts[g.State]; !ok {
			byState.Set(g.State, 0)
		}
		counts[g.State]++
		byState.Set(g.State, counts[g.State])
		sources = append(sources, fromGen(g).ToAPI())
	}
	o := wbcrud.NewObj()
	o.Set("enforce", flags.Enforce)
	o.Set("probe_enabled", flags.ProbeEnabled)
	o.Set("total", len(rows))
	o.Set("by_state", byState)
	o.Set("sources", sources)
	return o, nil
}

// Map is health_map: name -> to_api.
func Map(ctx context.Context, tx pgx.Tx) (map[string]*Obj, error) {
	rows, err := gen.New(tx).SHAll(ctx)
	if err != nil {
		return nil, err
	}
	m := make(map[string]*Obj, len(rows))
	for _, g := range rows {
		m[g.Name] = fromGen(g).ToAPI()
	}
	return m, nil
}

// ManualEnable is manual_enable: get-or-create, pin manual_override, clear an
// auto_disabled state (and consecutive_zero).
func ManualEnable(ctx context.Context, tx pgx.Tx, name string) (*Obj, error) {
	g, err := gen.New(tx).SHManualEnable(ctx, name)
	if err != nil {
		return nil, err
	}
	return fromGen(gen.SourceHealth(g)).ToAPI(), nil
}

// Reset is reset_health: nil when the source has no row.
func Reset(ctx context.Context, tx pgx.Tx, name string) (*Obj, error) {
	g, err := gen.New(tx).SHReset(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return fromGen(gen.SourceHealth(g)).ToAPI(), nil
}

// DisabledNames is health_disabled_names: empty whenever enforcement is off
// (no query at all), and fail-open (empty) when the query fails.
func DisabledNames(ctx context.Context, tx pgx.Tx, enforce bool) (map[string]bool, error) {
	out := map[string]bool{}
	if !enforce {
		return out, nil
	}
	names, err := gen.New(tx).SHDisabledNames(ctx)
	if err != nil {
		return map[string]bool{}, nil // fail-open: a health bug can't blind the engine
	}
	for _, n := range names {
		out[n] = true
	}
	return out, nil
}

// IsDisabled is is_health_disabled: true only when enforcing and the row is
// auto_disabled; a missing row or a query error is false (fail-open).
func IsDisabled(ctx context.Context, tx pgx.Tx, enforce bool, name string) (bool, error) {
	if !enforce {
		return false, nil
	}
	g, err := gen.New(tx).SHGet(ctx, name)
	if err != nil {
		return false, nil
	}
	return g.State == StateDisabled, nil
}

// ApplyRun persists one probe run inside tx (commit/rollback is the caller's:
// any error means the whole run must roll back). Names are processed in sorted
// order and each row is locked FOR UPDATE after an insert-if-missing, so
// concurrent runs serialize per row without deadlocking or losing updates.
func ApplyRun(ctx context.Context, tx pgx.Tx, yields map[string]int, outage bool, now time.Time) error {
	return ApplyRunWith(ctx, tx, yields, outage, now, DefaultThresholds)
}

// ApplyRunWith is ApplyRun with explicit thresholds.
func ApplyRunWith(ctx context.Context, tx pgx.Tx, yields map[string]int, outage bool, now time.Time, th Thresholds) error {
	names := make([]string, 0, len(yields))
	for n := range yields {
		names = append(names, n)
	}
	sort.Strings(names)
	q := gen.New(tx)
	for _, n := range names {
		if err := q.SHCreateIfMissing(ctx, n); err != nil {
			return err
		}
		g, err := q.SHGetForUpdate(ctx, n)
		if err != nil {
			return err
		}
		r := fromGen(gen.SourceHealth(g))
		ApplyTransition(r, yields[n], outage, now, th)
		if err := upsert(ctx, q, r); err != nil {
			return err
		}
	}
	return nil
}

// ApplyRunPool runs ApplyRunWith in its own transaction on the global (no
// workspace GUC) pool: all rows commit together or none do.
func ApplyRunPool(ctx context.Context, pool *pgxpool.Pool, yields map[string]int, outage bool, now time.Time) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	if err := ApplyRun(ctx, tx, yields, outage, now); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}
