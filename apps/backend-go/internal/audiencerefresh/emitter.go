// Package audiencerefresh holds the audience refresh engine (plan 10). Only the
// membership-event Emitter seam lives here until the engine lands.
package audiencerefresh

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/automationhook"
)

// MembershipEvent is plan 08's automationhook.AudienceEvent (Type is "entered" or "exited").
type MembershipEvent = automationhook.AudienceEvent

// Emitter is the audience slice of plan 08's automationhook.Emitter, so the automations
// emitter is assigned to Deps.AudienceEvents without an adapter. Called inside the
// refresh's workspace transaction.
type Emitter interface {
	AudienceEvents(ctx context.Context, tx pgx.Tx, workspaceID string, evs []MembershipEvent) (enqueued int, err error)
}
