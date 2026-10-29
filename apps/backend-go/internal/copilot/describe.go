package copilot

import (
	"strings"
	"unicode/utf8"
)

// pyLen is len(v) for a JSON value; ok=false where Python raises TypeError.
func pyLen(v any) (int, bool) {
	switch x := v.(type) {
	case []any:
		return len(x), true
	case map[string]any:
		return len(x), true
	case string:
		return utf8.RuneCountInString(x), true
	}
	return 0, false
}

// argOr is f"{args.get(key, dflt)}": the default only when the key is absent.
func argOr(args map[string]any, key, dflt string) string {
	if v, ok := args[key]; ok {
		return pyStr(v)
	}
	return dflt
}

// DescribeAction ports services/copilot/tools.py describe_action: the confirmation-card text. ok=false means
// Go cannot answer identically (execute_plan renders the server-stored autopilot plan, owned by
// WP-COPILOT-TOOLS; a value Python would raise on) and the caller falls back.
func DescribeAction(name string, args map[string]any) (string, bool) {
	if name == "execute_plan" {
		return "", false
	}
	if args == nil {
		args = map[string]any{}
	}
	label, reason := name, "This action modifies data."
	if m, ok := loadCatalog().meta[name]; ok {
		label, reason = m.Label, m.Reason
	}
	var d strings.Builder
	switch name {
	case "start_collection":
		d.WriteString(`Query: "` + argOr(args, "query", "?") + `"`)
	case "update_lead_status":
		d.WriteString("Lead #" + argOr(args, "lead_id", "?") + " → " + argOr(args, "status", "?"))
	case "enrich_lead":
		d.WriteString("Lead #" + argOr(args, "lead_id", "?"))
	case "import_ambitionbox_to_workbook":
		d.WriteString(argOr(args, "limit", "10") + " companies")
		if pyTruthy(args["industry"]) {
			d.WriteString(" · Industry: " + pyStr(args["industry"]))
		}
		if pyTruthy(args["name"]) {
			d.WriteString(" · Workbook: " + pyStr(args["name"]))
		}
	case "create_people_workbook", "enrich_people_contacts":
		d.WriteString("Conversation: " + argOr(args, "conversation_id", "?"))
		if pyTruthy(args["person_ids"]) {
			n, ok := pyLen(args["person_ids"])
			if !ok {
				return "", false
			}
			d.WriteString(" · People: " + pyStr(n))
		}
		if name == "create_people_workbook" && pyTruthy(args["name"]) {
			d.WriteString(" · Workbook: " + pyStr(args["name"]))
		}
	case "create_source_workbook":
		d.WriteString("Target rows: " + argOr(args, "target_rows", "?") + " · Query: " + argOr(args, "icp_description", "?"))
	case "track_account_signals":
		accounts := args["account_ids"]
		if !pyTruthy(accounts) {
			accounts = []any{}
		}
		n, ok := pyLen(accounts)
		if !ok {
			return "", false
		}
		var sigs []string
		if st := args["signal_types"]; pyTruthy(st) {
			list, isList := st.([]any)
			if s, isStr := st.(string); isStr { // ', '.join of a str joins its characters
				for _, r := range s {
					list = append(list, string(r))
				}
				isList = true
			}
			if !isList {
				return "", false
			}
			for _, s := range list {
				str, isStr := s.(string)
				if !isStr {
					return "", false
				}
				sigs = append(sigs, str)
			}
		}
		d.WriteString("Workbook: " + argOr(args, "workbook_id", "?") + " · Accounts: " + pyStr(n) +
			" · Cadence: " + argOr(args, "cadence", "?") + " · Signals: " + strings.Join(sigs, ", "))
	case "draft_grounded_outreach":
		d.WriteString("Conversation: " + argOr(args, "conversation_id", "?") + " · Person: " + argOr(args, "person_id", "?") +
			" · Risky address approved: " + pyStr(pyTruthy(args["allow_risky"])) + " · Send: disabled")
	}
	return label + "\n" + reason + "\n" + d.String(), true
}
