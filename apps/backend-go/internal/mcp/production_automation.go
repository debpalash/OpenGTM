package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autosvc"
)

// maxRuleNameChars is the REST create bound (autosvc): triggers.name is varchar(200).
const maxRuleNameChars = 200

// PGAutomationPort is the production AutomationPort over internal/autosvc, the
// rule service behind /api/automations: the same validation (trigger, schedule
// interval, action types, max actions per rule, webhook URL and domain
// allowlist, the outbound header_secret_ref policy, the legacy outreach policy,
// safe conditions) and the same insert. Unlike autosvc.Service.Create it runs
// inside the write pipeline's transaction, so the rule commits with the cap
// settlement and idempotency key.
type PGAutomationPort struct {
	Svc *autosvc.Service
}

// CountRules takes the per-workspace rule lock first (held to commit, as the
// REST create): two concurrent creates cannot both pass the rule cap.
func (p PGAutomationPort) CountRules(ctx context.Context, tx pgx.Tx, ws string) (int, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('auto_rules:' || $1::text, 0))`, ws); err != nil {
		return 0, err
	}
	return p.Svc.CountRules(ctx, tx, ws)
}

// ValidateRule is _validate_rule through autosvc. A failure that is not a rule
// rejection (a database error) is logged by kind and answered as a masked 500,
// which rolls the write back.
func (p PGAutomationPort) ValidateRule(ctx context.Context, tx pgx.Tx, ws string, r RuleInput) *HTTPError {
	if len([]rune(r.Name)) > maxRuleNameChars {
		return &HTTPError{Status: 422, Detail: fmt.Sprintf("name too long (max %d characters)", maxRuleNameChars)}
	}
	in, he := toAutosvcRule(r)
	if he != nil {
		return he
	}
	err := p.Svc.Validate(ctx, tx, ws, in)
	if err == nil {
		return nil
	}
	var ae *autosvc.HTTPError
	if errors.As(err, &ae) {
		return &HTTPError{Status: ae.Status, Detail: detailText(ae.Detail)}
	}
	slog.Error("mcp create_automation validation failed", "cause", errorKind(err))
	return &HTTPError{Status: 500, Detail: errInternal.Error()}
}

// CreateTrigger inserts the validated rule (schedule_anchor = now for
// on_schedule, created_by = the token's user) and returns Trigger.to_api.
func (p PGAutomationPort) CreateTrigger(ctx context.Context, tx pgx.Tx, ws string, r RuleInput) (json.RawMessage, error) {
	in, he := toAutosvcRule(r)
	if he != nil {
		return nil, he
	}
	t, err := p.Svc.InsertTrigger(ctx, tx, ws, in)
	if err != nil {
		return nil, err
	}
	return t.MarshalJSON()
}

// BootstrapSchedule runs after the commit in its own transaction.
func (p PGAutomationPort) BootstrapSchedule(ctx context.Context, ws, id string) error {
	return p.Svc.BootstrapSchedule(ctx, ws, id)
}

// toAutosvcRule decodes the JSON fields into autosvc's ordered values (objects
// keep their key order: Trigger.to_api and the stored columns follow it).
func toAutosvcRule(r RuleInput) (autosvc.RuleInput, *HTTPError) {
	in := autosvc.RuleInput{
		Name: r.Name, TriggerType: r.TriggerType, Condition: r.Condition, ScopeWorkbookIDs: r.ScopeWorkbookIDs,
		StopOnError: r.StopOnError, MaxSpendUSDPerDay: r.MaxSpendUSDPerDay, Enabled: r.Enabled,
	}
	tc, err := autosvc.ParseJSON(r.TriggerConfig)
	if err != nil {
		return in, &HTTPError{Status: 422, Detail: "trigger_config must be an object"}
	}
	in.TriggerConfig = tc
	acts, err := autosvc.ParseJSON(r.Actions)
	if err != nil {
		return in, &HTTPError{Status: 422, Detail: "actions must be a list"}
	}
	if acts != nil {
		l, ok := acts.([]any)
		if !ok {
			return in, &HTTPError{Status: 422, Detail: "actions must be a list"}
		}
		in.Actions = l
	}
	if r.MaxActionsPerDay != nil {
		v := *r.MaxActionsPerDay
		if v > 1<<31-1 {
			return in, &HTTPError{Status: 422, Detail: "max_actions_per_day must be at most 2147483647"}
		}
		if v < -(1 << 31) {
			v = -1 // negative either way: ValidateRule rejects it
		}
		n := int32(v)
		in.MaxActionsPerDay = &n
	}
	if r.CreatedBy != nil {
		in.CreatedBy = strconv.Itoa(int(*r.CreatedBy))
	}
	return in, nil
}

// detailText renders a REST detail: strings as is, anything else as JSON (a
// Python repr in the original; accepted deviation, contract README MCP section).
func detailText(d any) string {
	if s, ok := d.(string); ok {
		return s
	}
	b, err := json.Marshal(d)
	if err != nil {
		return fmt.Sprint(d)
	}
	return string(b)
}
