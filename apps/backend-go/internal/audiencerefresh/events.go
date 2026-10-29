package audiencerefresh

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// emitEvents is _emit_refresh_events: one refresh's events in stable keyset pages by
// id, each page emitted in its own workspace transaction after the diff committed.
func emitEvents(ctx context.Context, pool *pgxpool.Pool, ws, audienceID, refreshID string, deps Deps) error {
	if deps.AudienceEvents == nil {
		return nil
	}
	var after int32
	for {
		var evs []MembershipEvent
		err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
			rows, err := gen.New(tx).AudRefreshEventsPage(ctx, gen.AudRefreshEventsPageParams{WorkspaceID: ws, AudienceID: audienceID,
				RefreshID: refreshID, AfterID: after, PageLimit: int32(deps.PageSize)})
			if err != nil {
				return fmt.Errorf("audiencerefresh: read events: %w", err)
			}
			for _, r := range rows {
				evs = append(evs, MembershipEvent{ID: int64(r.ID), Type: r.EventType, AudienceID: audienceID, LeadID: int64(r.LeadID)})
			}
			if len(evs) == 0 {
				return nil
			}
			_, err = deps.AudienceEvents.AudienceEvents(ctx, tx, ws, evs)
			return err
		})
		if err != nil {
			return err
		}
		if len(evs) == 0 {
			return nil
		}
		after = int32(evs[len(evs)-1].ID)
	}
}
