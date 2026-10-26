// Package audiencerefresh holds the audience refresh engine (plan 10). Only the
// membership-event Emitter seam lives here until the engine lands.
package audiencerefresh

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// MembershipEvent is one audience_membership_events row (plan 08 shape).
type MembershipEvent struct {
	ID         int
	AudienceID string
	LeadID     int
	EventType  string
	Snapshot   map[string]any
	RefreshID  string
	CreatedAt  time.Time
}

// Emitter publishes membership events inside the caller's workspace transaction
// (plan 08 automations event bus implements it).
type Emitter interface {
	Emit(ctx context.Context, tx pgx.Tx, workspaceID string, events []MembershipEvent) error
}
