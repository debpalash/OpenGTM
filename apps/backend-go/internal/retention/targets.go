package retention

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// CutoffKind is how a target stores its age column.
type CutoffKind int

const (
	// KindTimestamp: created_at timestamp/timestamptz; the cutoff is an instant.
	KindTimestamp CutoffKind = iota
	// KindDate: a 'YYYY-MM-DD' string column.
	KindDate
	// KindEpoch: a float seconds-since-epoch column.
	KindEpoch
)

// Target is one table the policy governs; several tables may share a Category.
type Target struct {
	Category string
	Table    string
	Kind     CutoffKind
}

var targets = []Target{
	{"audit", "governance_audit_events", KindTimestamp},
	{"llm_usage", "llm_usage_daily", KindDate},
	{"signals", "signals", KindEpoch},
	{"activation", "destination_deliveries", KindTimestamp},
	{"activation", "destination_inbound_receipts", KindTimestamp},
	{"audience_history", "audience_membership_events", KindTimestamp},
	{"agent_results", "playbook_results", KindTimestamp},
	{"outreach_history", "outreach_sends", KindTimestamp},
}

// Targets returns the eight targets in Python's _targets order.
func Targets() []Target { return append([]Target(nil), targets...) }

// Cutoff is the value a row's age column must be strictly below to be expired:
// a time.Time instant (KindTimestamp), a 'YYYY-MM-DD' string (KindDate, the date
// in now's own zone like Python's cutoff.date()), or a float64 epoch (KindEpoch).
// now is truncated to microseconds, Python's datetime resolution. days must have
// been through NormalizeDays: Cutoff does not validate.
func Cutoff(t Target, days int, now time.Time) any {
	c := now.Truncate(time.Microsecond).Add(-time.Duration(days) * 24 * time.Hour)
	switch t.Kind {
	case KindEpoch:
		return float64(c.UnixMicro()) / 1e6
	case KindDate:
		return c.Format("2006-01-02")
	}
	return c
}

// Preview counts, per category, the rows older than the policy cutoff in ws
// (categories with two tables are summed; keys in Categories order). It only
// reads. days is normalized (defaults filled, range checked) exactly as the
// enforcement job does. tx must be inside tenancy.InWorkspace; every count also
// filters workspace_id. Naive timestamp columns compare as timestamptz in the
// session TimeZone, like Python's aware-datetime bind.
func Preview(ctx context.Context, tx pgx.Tx, ws string, days map[string]int, now time.Time) (Counts, error) {
	norm, err := normalizeInts(days)
	if err != nil {
		return nil, err
	}
	if ws == "" {
		return nil, fmt.Errorf("retention: preview needs a workspace")
	}
	q := gen.New(tx)
	counts := Counts{}
	for _, c := range Categories {
		counts[c] = 0
	}
	for _, t := range targets {
		cut := Cutoff(t, norm[t.Category], now)
		ts := func() pgtype.Timestamptz { return pgtype.Timestamptz{Time: cut.(time.Time).UTC(), Valid: true} }
		var n int64
		var err error
		switch t.Table {
		case "governance_audit_events":
			n, err = q.RetCountAudit(ctx, gen.RetCountAuditParams{WorkspaceID: ws, Cutoff: ts()})
		case "llm_usage_daily":
			n, err = q.RetCountLLMUsage(ctx, gen.RetCountLLMUsageParams{WorkspaceID: ws, Cutoff: cut.(string)})
		case "signals":
			n, err = q.RetCountSignals(ctx, gen.RetCountSignalsParams{WorkspaceID: ws, Cutoff: cut.(float64)})
		case "destination_deliveries":
			n, err = q.RetCountDeliveries(ctx, gen.RetCountDeliveriesParams{WorkspaceID: ws, Cutoff: ts()})
		case "destination_inbound_receipts":
			n, err = q.RetCountInboundReceipts(ctx, gen.RetCountInboundReceiptsParams{WorkspaceID: ws, Cutoff: ts()})
		case "audience_membership_events":
			n, err = q.RetCountMembershipEvents(ctx, gen.RetCountMembershipEventsParams{WorkspaceID: ws, Cutoff: ts()})
		case "playbook_results":
			n, err = q.RetCountPlaybookResults(ctx, gen.RetCountPlaybookResultsParams{WorkspaceID: ws, Cutoff: ts()})
		case "outreach_sends":
			n, err = q.RetCountOutreachSends(ctx, gen.RetCountOutreachSendsParams{WorkspaceID: ws, Cutoff: ts()})
		default:
			err = fmt.Errorf("retention: unknown target %s", t.Table)
		}
		if err != nil {
			return nil, fmt.Errorf("retention: count %s: %w", t.Table, err)
		}
		counts[t.Category] += n
	}
	return counts, nil
}
