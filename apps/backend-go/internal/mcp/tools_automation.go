package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/mcp/pyjson"
)

const (
	maxAutomationActions = 50
	msgAutomationsOff    = "automations disabled"
)

// automationWriteTools is create_automation (automations:write, admin only). It
// runs through AutomationPort (plan 08, D9); with a nil port every call answers
// "not available in this build" (the port accessor's ErrUnavailable).
//
// The result carries automation.trigger_type, automation.enabled and
// automation_id: the write pipeline's post-commit hook reads exactly those to
// run BootstrapSchedule for an enabled on_schedule rule (Task 10). The adapter's
// CreateTrigger is responsible for schedule_anchor = now on an on_schedule rule
// and for created_by = RuleInput.CreatedBy (the token's user id).
func automationWriteTools(d Deps) []WriteTool {
	return []WriteTool{{
		Name:       "create_automation",
		Capability: CapAutomationsWrite,
		Roles:      AdminRoles,
		Gate: func(cfg Config) string {
			if !cfg.AutomationsEnabled {
				return msgAutomationsOff
			}
			return ""
		},
		Apply: func(ctx context.Context, tx pgx.Tx, c Ctx, args Args) (string, error) {
			return createAutomation(ctx, tx, d, c, args)
		},
	}}
}

func createAutomation(ctx context.Context, tx pgx.Tx, d Deps, c Ctx, args Args) (string, error) {
	in, msg, err := parseRuleInput(args)
	if err != nil {
		return errorText(err.Error()), nil
	}
	if msg != "" {
		return errorText(msg), nil
	}
	in.CreatedBy = c.UserID
	// The type checks above ran on the decoded map; store the client's bytes so
	// object keys keep their order (non-empty values only: {} and [] are defaults).
	if raw := orderedArg(ctx, "trigger_config"); raw != nil && len(in.TriggerConfig) > 2 {
		in.TriggerConfig = raw
	}
	if raw := orderedArg(ctx, "actions"); raw != nil && len(in.Actions) > 2 {
		in.Actions = raw
	}

	port := d.AutomationPort()
	count, err := port.CountRules(ctx, tx, c.WorkspaceID)
	if err != nil {
		return "", err
	}
	if max := d.Cfg.AutomationsMaxRulesPerWS; count >= max {
		return errorText(fmt.Sprintf("max rules per workspace reached (%d)", max)), nil
	}
	if he := port.ValidateRule(ctx, tx, c.WorkspaceID, in); he != nil {
		b, derr := pyjson.Dumps(pyjson.NewObj("error", he.Detail, "status_code", int64(he.Status)), 0)
		if derr != nil {
			return "", derr
		}
		return string(b), nil
	}
	stored, err := port.CreateTrigger(ctx, tx, c.WorkspaceID, in)
	if err != nil {
		return "", err
	}
	var row struct {
		ID any `json:"id"`
	}
	if json.Unmarshal(stored, &row) != nil {
		return "", errInternal
	}
	b, err := pyjson.Dumps(pyjson.NewObj("status", "created", "automation_id", row.ID, "automation", stored), 0)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// parseRuleInput applies the Python defaults and the D3 type checks. It returns
// either an ArgError (wrong type, "<field> must be <type>") or a plain message
// ("name is required"), never both.
func parseRuleInput(args Args) (RuleInput, string, error) {
	in := RuleInput{
		TriggerConfig: json.RawMessage(`{}`),
		Actions:       json.RawMessage(`[]`),
		Enabled:       true,
	}
	name, _, err := args.String("name")
	if err != nil {
		return in, "", err
	}
	if in.Name = strings.TrimSpace(name); in.Name == "" {
		return in, "name is required", nil
	}
	if tt, ok, err := args.String("trigger_type"); err != nil {
		return in, "", err
	} else if ok {
		in.TriggerType = &tt
	}
	if v, ok := args.get("trigger_config"); ok {
		m, isObj := v.(map[string]any)
		if !isObj {
			return in, "", &ArgError{Field: "trigger_config", Want: "an object"}
		}
		if len(m) > 0 {
			in.TriggerConfig, _ = json.Marshal(m)
		}
	}
	if v, ok := args.get("actions"); ok {
		arr, isArr := v.([]any)
		if !isArr || len(arr) > maxAutomationActions {
			return in, "", &ArgError{Field: "actions", Want: fmt.Sprintf("a list of at most %d objects", maxAutomationActions)}
		}
		for _, e := range arr {
			if _, isObj := e.(map[string]any); !isObj {
				return in, "", &ArgError{Field: "actions", Want: fmt.Sprintf("a list of at most %d objects", maxAutomationActions)}
			}
		}
		if len(arr) > 0 {
			in.Actions, _ = json.Marshal(arr)
		}
	}
	if in.Condition, _, err = args.String("condition"); err != nil {
		return in, "", err
	}
	ids, _, err := args.StringList("scope_workbook_ids")
	if err != nil {
		return in, "", err
	}
	in.ScopeWorkbookIDs = ids
	if in.Enabled, err = boolArg(args, "enabled", true); err != nil {
		return in, "", err
	}
	if in.StopOnError, err = boolArg(args, "stop_on_error", false); err != nil {
		return in, "", err
	}
	if v, ok := args.get("max_spend_usd_per_day"); ok {
		f, isNum := asFloat(v)
		if !isNum {
			return in, "", &ArgError{Field: "max_spend_usd_per_day", Want: "a number"}
		}
		in.MaxSpendUSDPerDay = &f
	}
	if i, ok, err := args.Int("max_actions_per_day"); err != nil {
		return in, "", err
	} else if ok {
		in.MaxActionsPerDay = &i
	}
	return in, "", nil
}

func boolArg(a Args, key string, def bool) (bool, error) {
	v, ok := a.get(key)
	if !ok {
		return def, nil
	}
	b, isBool := v.(bool)
	if !isBool {
		return false, &ArgError{Field: key, Want: "a boolean"}
	}
	return b, nil
}

// asFloat accepts JSON numbers only (never booleans or strings).
func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}
