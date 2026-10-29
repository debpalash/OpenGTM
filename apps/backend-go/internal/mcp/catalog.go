// Code generated from legacy/python apps/api/services/mcp/tools.py TOOL_SPECS; DO NOT EDIT.
// Verified byte for byte against testdata/mcp tools/list fixtures.

package mcp

import "github.com/debpalash/OpenGTM/apps/backend-go/internal/mcp/pyjson"

// ToolSpec is one public MCP tool entry; InputSchema key order is wire parity.
type ToolSpec struct {
	Name, Capability, Description string
	InputSchema                   *pyjson.Obj
}

// Specs is TOOL_SPECS in Python order.
var Specs = []ToolSpec{
	{
		Name:        "find_leads",
		Capability:  CapLeadsRead,
		Description: "Search for leads in the OpenGTM database by company, city, industry, tier, or keyword. Returns matching leads with contact info and scores.",
		InputSchema: pyjson.NewObj(
			"type", "object",
			"properties", pyjson.NewObj(
				"query", pyjson.NewObj(
					"type", "string",
					"description", "Search query (company name, keyword, industry)",
				),
				"city", pyjson.NewObj(
					"type", "string",
					"description", "Filter by city",
				),
				"tier", pyjson.NewObj(
					"type", "string",
					"description", "Filter by score tier: hot, warm, cold",
				),
				"limit", pyjson.NewObj(
					"type", "integer",
					"description", "Max results (default: 10)",
					"default", int64(10),
				),
			),
			"required", []any{"query"},
		),
	},
	{
		Name:        "get_lead_detail",
		Capability:  CapLeadsRead,
		Description: "Get complete profile of a specific lead by ID — company info, contacts, enrichment data, scores, decision makers.",
		InputSchema: pyjson.NewObj(
			"type", "object",
			"properties", pyjson.NewObj(
				"lead_id", pyjson.NewObj(
					"type", "integer",
					"description", "The lead ID",
				),
			),
			"required", []any{"lead_id"},
		),
	},
	{
		Name:        "get_pipeline_stats",
		Capability:  CapLeadsRead,
		Description: "Get GTM pipeline statistics: total leads, breakdown by tier/status/city, enrichment coverage rates.",
		InputSchema: pyjson.NewObj(
			"type", "object",
			"properties", pyjson.NewObj(),
		),
	},
	{
		Name:        "enrich_company",
		Capability:  CapLeadsRead,
		Description: "Trigger on-demand enrichment for a lead — scrape website, find emails, social profiles, decision makers.",
		InputSchema: pyjson.NewObj(
			"type", "object",
			"properties", pyjson.NewObj(
				"lead_id", pyjson.NewObj(
					"type", "integer",
					"description", "The lead ID to enrich",
				),
			),
			"required", []any{"lead_id"},
		),
	},
	{
		Name:        "verify_email",
		Capability:  CapLeadsRead,
		Description: "Verify an email address using SMTP RCPT TO validation. Returns deliverable/undeliverable status.",
		InputSchema: pyjson.NewObj(
			"type", "object",
			"properties", pyjson.NewObj(
				"email", pyjson.NewObj(
					"type", "string",
					"description", "Email address to verify",
				),
			),
			"required", []any{"email"},
		),
	},
	{
		Name:        "get_hiring_signals",
		Capability:  CapLeadsRead,
		Description: "Check for active job postings at a company. Returns job titles, counts, and hiring velocity as buying signals.",
		InputSchema: pyjson.NewObj(
			"type", "object",
			"properties", pyjson.NewObj(
				"company", pyjson.NewObj(
					"type", "string",
					"description", "Company name to check",
				),
				"lead_id", pyjson.NewObj(
					"type", "integer",
					"description", "Optional lead ID for enriched context",
				),
			),
			"required", []any{"company"},
		),
	},
	{
		Name:        "score_lead",
		Capability:  CapLeadsRead,
		Description: "Calculate a lead quality score (0-100) based on data completeness, company signals, and contact depth.",
		InputSchema: pyjson.NewObj(
			"type", "object",
			"properties", pyjson.NewObj(
				"lead_id", pyjson.NewObj(
					"type", "integer",
					"description", "The lead ID to score",
				),
			),
			"required", []any{"lead_id"},
		),
	},
	{
		Name:        "create_lead",
		Capability:  CapLeadsWrite,
		Description: "Create a new lead in the caller's workspace. Persists through the same RLS-scoped store as the REST API.",
		InputSchema: pyjson.NewObj(
			"type", "object",
			"properties", pyjson.NewObj(
				"company", pyjson.NewObj(
					"type", "string",
					"description", "Company name (required)",
				),
				"email", pyjson.NewObj(
					"type", "string",
				),
				"phone", pyjson.NewObj(
					"type", "string",
				),
				"website", pyjson.NewObj(
					"type", "string",
				),
				"city", pyjson.NewObj(
					"type", "string",
				),
				"state", pyjson.NewObj(
					"type", "string",
				),
				"contact_person", pyjson.NewObj(
					"type", "string",
				),
				"contact_title", pyjson.NewObj(
					"type", "string",
				),
				"specialization", pyjson.NewObj(
					"type", "string",
				),
				"company_size", pyjson.NewObj(
					"type", "string",
				),
				"description", pyjson.NewObj(
					"type", "string",
				),
				"linkedin_url", pyjson.NewObj(
					"type", "string",
				),
				"notes", pyjson.NewObj(
					"type", "string",
				),
				"source", pyjson.NewObj(
					"type", "string",
				),
				"idempotency_key", pyjson.NewObj(
					"type", "string",
					"description", "Optional; a retry with the same key is a no-op (no double-create).",
				),
			),
			"required", []any{"company"},
		),
	},
	{
		Name:        "update_lead",
		Capability:  CapLeadsWrite,
		Description: "Update fields on an existing lead by ID (workspace-scoped — a lead in another workspace is not found). Only an allowlisted set of business fields can be written.",
		InputSchema: pyjson.NewObj(
			"type", "object",
			"properties", pyjson.NewObj(
				"lead_id", pyjson.NewObj(
					"type", "integer",
					"description", "The lead ID to update",
				),
				"email", pyjson.NewObj(
					"type", "string",
				),
				"phone", pyjson.NewObj(
					"type", "string",
				),
				"website", pyjson.NewObj(
					"type", "string",
				),
				"city", pyjson.NewObj(
					"type", "string",
				),
				"state", pyjson.NewObj(
					"type", "string",
				),
				"contact_person", pyjson.NewObj(
					"type", "string",
				),
				"contact_title", pyjson.NewObj(
					"type", "string",
				),
				"specialization", pyjson.NewObj(
					"type", "string",
				),
				"company_size", pyjson.NewObj(
					"type", "string",
				),
				"description", pyjson.NewObj(
					"type", "string",
				),
				"linkedin_url", pyjson.NewObj(
					"type", "string",
				),
				"notes", pyjson.NewObj(
					"type", "string",
				),
				"status", pyjson.NewObj(
					"type", "string",
					"description", "Lead status: new, contacted, qualified, converted, dead",
				),
				"idempotency_key", pyjson.NewObj(
					"type", "string",
					"description", "Optional; a retry with the same key is a no-op.",
				),
			),
			"required", []any{"lead_id"},
		),
	},
	{
		Name:        "enroll_leads",
		Capability:  CapSequencesEnroll,
		Description: "Enroll leads into an outreach sequence. Skips leads with no email or on the suppression list and records consent (CAN-SPAM). Sequence + leads must be in the caller's workspace.",
		InputSchema: pyjson.NewObj(
			"type", "object",
			"properties", pyjson.NewObj(
				"sequence_id", pyjson.NewObj(
					"type", "string",
					"description", "The sequence ID to enroll into",
				),
				"lead_ids", pyjson.NewObj(
					"type", "array",
					"items", pyjson.NewObj(
						"type", "integer",
					),
					"description", "Lead IDs to enroll",
				),
				"idempotency_key", pyjson.NewObj(
					"type", "string",
					"description", "Optional; a retry with the same key is a no-op.",
				),
			),
			"required", []any{"sequence_id", "lead_ids"},
		),
	},
	{
		Name:        "create_workbook",
		Capability:  CapWorkbooksWrite,
		Description: "Create a new workbook from a description. Auto-generates columns. Example: 'SaaS CTOs in SF with email and LinkedIn'",
		InputSchema: pyjson.NewObj(
			"type", "object",
			"properties", pyjson.NewObj(
				"description", pyjson.NewObj(
					"type", "string",
					"description", "Natural language description of the workbook",
				),
				"name", pyjson.NewObj(
					"type", "string",
					"description", "Optional name for the workbook",
				),
				"idempotency_key", pyjson.NewObj(
					"type", "string",
					"description", "Optional; a retry with the same key is a no-op.",
				),
			),
			"required", []any{"description"},
		),
	},
	{
		Name:        "create_automation",
		Capability:  CapAutomationsWrite,
		Description: "Create an automation rule (trigger + conditions + actions) in the caller's workspace. Routes through the same validation as the REST API; outreach actions require legacy outreach to be enabled. Admin only.",
		InputSchema: pyjson.NewObj(
			"type", "object",
			"properties", pyjson.NewObj(
				"name", pyjson.NewObj(
					"type", "string",
					"description", "Automation name (required)",
				),
				"trigger_type", pyjson.NewObj(
					"type", "string",
					"description", "on_signal | on_row_changed | on_row_added | on_schedule",
				),
				"trigger_config", pyjson.NewObj(
					"type", "object",
					"description", "Trigger-type-specific config (e.g. {interval: daily} for on_schedule)",
				),
				"condition", pyjson.NewObj(
					"type", "string",
					"description", "Optional condition expression",
				),
				"actions", pyjson.NewObj(
					"type", "array",
					"items", pyjson.NewObj(
						"type", "object",
					),
					"description", "List of {type, config} actions",
				),
				"scope_workbook_ids", pyjson.NewObj(
					"type", "array",
					"items", pyjson.NewObj(
						"type", "string",
					),
				),
				"enabled", pyjson.NewObj(
					"type", "boolean",
					"default", true,
				),
				"stop_on_error", pyjson.NewObj(
					"type", "boolean",
					"default", false,
				),
				"max_spend_usd_per_day", pyjson.NewObj(
					"type", "number",
				),
				"max_actions_per_day", pyjson.NewObj(
					"type", "integer",
				),
				"idempotency_key", pyjson.NewObj(
					"type", "string",
					"description", "Optional; a retry with the same key is a no-op.",
				),
			),
			"required", []any{"name", "trigger_type"},
		),
	},
	{
		Name:        "send_email",
		Capability:  CapOutreachSend,
		Description: "Enqueue an outreach email to a lead through the durable send path (inherits suppression, rate limits, debit-on-success, the bounce circuit breaker). Optionally tied to a sequence step. Refused unless legacy outreach is enabled. Admin only — HIGHEST RISK.",
		InputSchema: pyjson.NewObj(
			"type", "object",
			"properties", pyjson.NewObj(
				"lead_id", pyjson.NewObj(
					"type", "integer",
					"description", "Lead to email (must be in the caller's workspace)",
				),
				"sequence_id", pyjson.NewObj(
					"type", "string",
					"description", "Optional sequence id; the send is keyed to its step",
				),
				"step_number", pyjson.NewObj(
					"type", "integer",
					"default", int64(0),
				),
				"subject", pyjson.NewObj(
					"type", "string",
				),
				"body_html", pyjson.NewObj(
					"type", "string",
				),
				"idempotency_key", pyjson.NewObj(
					"type", "string",
					"description", "Optional; a retry with the same key is a no-op (at-most-once send).",
				),
			),
			"required", []any{"lead_id"},
		),
	},
	{
		Name:        "delete_lead",
		Capability:  CapLeadsDelete,
		Description: "Permanently delete a lead by ID in the caller's workspace. A lead in another workspace is a no-op miss. Idempotent. Admin only — DESTRUCTIVE.",
		InputSchema: pyjson.NewObj(
			"type", "object",
			"properties", pyjson.NewObj(
				"lead_id", pyjson.NewObj(
					"type", "integer",
					"description", "The lead ID to delete",
				),
				"idempotency_key", pyjson.NewObj(
					"type", "string",
					"description", "Optional; a retry with the same key is a no-op.",
				),
			),
			"required", []any{"lead_id"},
		),
	},
	{
		Name:        "delete_workbook",
		Capability:  CapWorkbooksDelete,
		Description: "Permanently delete a workbook by ID in the caller's workspace. A workbook in another workspace is a no-op miss. Idempotent. Admin only — DESTRUCTIVE.",
		InputSchema: pyjson.NewObj(
			"type", "object",
			"properties", pyjson.NewObj(
				"workbook_id", pyjson.NewObj(
					"type", "string",
					"description", "The workbook ID to delete",
				),
				"idempotency_key", pyjson.NewObj(
					"type", "string",
					"description", "Optional; a retry with the same key is a no-op.",
				),
			),
			"required", []any{"workbook_id"},
		),
	},
}
