package automationhook

import (
	"context"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// AudienceEvent is one AudienceMembershipEvent row: Type is "entered" or "exited".
type AudienceEvent struct {
	ID                  int64
	Type                string
	AudienceID          string
	LeadID              int64
	CompanyID, PersonID string
}

// AudienceEvents is events.emit_audience_membership.
func (h *hook) AudienceEvents(ctx context.Context, tx pgx.Tx, workspaceID string, evs []AudienceEvent) (int, error) {
	if !h.enabled || len(evs) == 0 {
		return 0, nil
	}
	return h.guarded(ctx, tx, "audience", workspaceID, func(sp pgx.Tx) (int, error) {
		q := gen.New(sp)
		byType := map[string][]gen.ListEnabledTriggersWithConfigRow{}
		for typ, trig := range map[string]string{"entered": "on_audience_enter", "exited": "on_audience_exit"} {
			rules, err := q.ListEnabledTriggersWithConfig(ctx, gen.ListEnabledTriggersWithConfigParams{
				WorkspaceID: workspaceID, TriggerType: trig})
			if err != nil {
				return 0, err
			}
			byType[typ] = rules
		}
		n := 0
		for _, ev := range evs {
			for _, rule := range byType[ev.Type] {
				ids, ok := configSet(rule.TriggerConfig, "audience_ids")
				if !ok {
					h.log.Warn("automation rule skipped: malformed audience_ids", "workspace_id", workspaceID, "trigger_id", rule.ID)
					continue
				}
				if len(ids) > 0 && !ids[ev.AudienceID] {
					continue
				}
				targets, err := h.subjectTargets(ctx, sp, workspaceID, ev.LeadID, ev.CompanyID, ev.PersonID, scopeList(rule.ScopeWorkbookIds))
				if err != nil {
					return 0, err
				}
				if len(targets) == 0 {
					continue
				}
				key := "audience:" + strconv.FormatInt(ev.ID, 10) + ":" + ev.Type
				if err := h.enqueue(ctx, q, workspaceID, rule.ID, targets, "audience_"+ev.Type, key); err != nil {
					return 0, err
				}
				n++
			}
		}
		return n, nil
	}), nil
}
