package deststore

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

const (
	healthRunWindow      = 20
	healthRecentFailures = 10
)

// HealthReport is destination_health: bounded operational state plus recent
// failure evidence.
type HealthReport struct {
	DestinationID, HealthStatus string
	LastError                   *string
	LastSuccessAt               *time.Time
	RunCounts                   map[string]int
	Deliveries                  struct{ Total, Succeeded, Failed int }
	SuccessRate                 *float64
	ConsecutiveUnhealthyRuns    int
	RecentFailures              []Delivery
	LatestRun                   *Run
}

// SuccessRate is round(succeeded / total, 4) as Python computes it (correctly
// rounded decimal, ties to even on the exact binary value); nil when total is 0.
func SuccessRate(succeeded, total int) *float64 {
	if total == 0 {
		return nil
	}
	v, _ := strconv.ParseFloat(strconv.FormatFloat(float64(succeeded)/float64(total), 'f', 4, 64), 64)
	return &v
}

// ConsecutiveUnhealthy counts the newest-first run statuses that are failed or
// completed_with_errors, stopping at the first other one, over at most the newest 20.
func ConsecutiveUnhealthy(latestFirst []string) int {
	n := 0
	for i, st := range latestFirst {
		if i >= healthRunWindow || (st != "failed" && st != "completed_with_errors") {
			break
		}
		n++
	}
	return n
}

// Health assembles the health report of one destination (ErrNotFound when it is
// not in the workspace).
func Health(ctx context.Context, tx pgx.Tx, destID string) (HealthReport, error) {
	ws, err := tenant(ctx, tx)
	if err != nil {
		return HealthReport{}, err
	}
	q := gen.New(tx)
	d, err := getDestination(ctx, q, ws, destID)
	if err != nil {
		return HealthReport{}, err
	}
	h := HealthReport{DestinationID: d.ID, HealthStatus: d.HealthStatus, LastError: d.LastError, LastSuccessAt: d.LastSuccessAt,
		RunCounts: map[string]int{}, RecentFailures: []Delivery{}}
	counts, err := q.DestRunCountsByStatus(ctx, gen.DestRunCountsByStatusParams{WorkspaceID: ws, DestinationID: destID})
	if err != nil {
		return HealthReport{}, fmt.Errorf("deststore: run counts: %w", err)
	}
	for _, c := range counts {
		h.RunCounts[c.Status] = int(c.N)
	}
	tot, err := q.DestDeliveryTotals(ctx, gen.DestDeliveryTotalsParams{WorkspaceID: ws, DestinationID: destID})
	if err != nil {
		return HealthReport{}, fmt.Errorf("deststore: delivery totals: %w", err)
	}
	h.Deliveries.Total, h.Deliveries.Succeeded, h.Deliveries.Failed = int(tot.Total), int(tot.Succeeded), int(tot.Failed)
	h.SuccessRate = SuccessRate(h.Deliveries.Succeeded, h.Deliveries.Total)
	fails, err := q.DestRecentFailures(ctx, gen.DestRecentFailuresParams{WorkspaceID: ws, DestinationID: destID, RowLimit: healthRecentFailures})
	if err != nil {
		return HealthReport{}, fmt.Errorf("deststore: recent failures: %w", err)
	}
	for _, f := range fails {
		h.RecentFailures = append(h.RecentFailures, deliveryFrom(f))
	}
	latest, err := q.DestRunsLatest(ctx, gen.DestRunsLatestParams{WorkspaceID: ws, DestinationID: destID, RowLimit: healthRunWindow})
	if err != nil {
		return HealthReport{}, fmt.Errorf("deststore: latest runs: %w", err)
	}
	statuses := make([]string, 0, len(latest))
	for _, r := range latest {
		statuses = append(statuses, r.Status)
	}
	h.ConsecutiveUnhealthyRuns = ConsecutiveUnhealthy(statuses)
	if len(latest) > 0 {
		r := runFrom(latest[0])
		h.LatestRun = &r
	}
	return h, nil
}
