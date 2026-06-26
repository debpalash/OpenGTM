"""MCP tool catalog + execution, RLS-scoped.

Every tool runs against the workspace-scoped, RLS-protected store
(``get_lead_store(workspace_id, slug)``) inside ``workspace_scope`` — never a
bare ``LeadDB()`` on the shared global file. This is the fix for the current
cross-tenant hole (a tool call could read every tenant's leads). The dispatch
here is the trusted server-side half; ``apps/mcp/server.py`` is now a thin
transport that authenticates, resolves an :class:`MCPCtx`, filters the catalog
to the token's capabilities, and delegates here.

Phase 1 = READ tools only. ``create_workbook`` is the lone write-shaped tool and
is gated behind ``MCP_WRITE_ENABLED`` (default OFF) + the ``workbooks:write``
capability, so it stays hidden and inert by default — Phase 2 replaces its stub
with a real RLS-scoped workbook create.
"""

from __future__ import annotations

import json
from typing import Dict, List

from apps.api.core.config import settings
from apps.api.core.tenancy import workspace_scope
from apps.api.services.leadgen.store import get_lead_store
from apps.api.services.mcp import auth
from apps.api.services.mcp.auth import MCPCtx


# ── Tool catalog ──────────────────────────────────────────────────────────────
# Each entry: the public MCP tool spec + the capability it requires. Tools whose
# capability is a *:write capability are additionally gated by MCP_WRITE_ENABLED.

TOOL_SPECS = [
    {
        "name": "find_leads",
        "capability": auth.CAP_LEADS_READ,
        "description": "Search for leads in the Yupcha database by company, city, industry, tier, or keyword. Returns matching leads with contact info and scores.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "query": {"type": "string", "description": "Search query (company name, keyword, industry)"},
                "city": {"type": "string", "description": "Filter by city"},
                "tier": {"type": "string", "description": "Filter by score tier: hot, warm, cold"},
                "limit": {"type": "integer", "description": "Max results (default: 10)", "default": 10},
            },
            "required": ["query"],
        },
    },
    {
        "name": "get_lead_detail",
        "capability": auth.CAP_LEADS_READ,
        "description": "Get complete profile of a specific lead by ID — company info, contacts, enrichment data, scores, decision makers.",
        "inputSchema": {
            "type": "object",
            "properties": {"lead_id": {"type": "integer", "description": "The lead ID"}},
            "required": ["lead_id"],
        },
    },
    {
        "name": "get_pipeline_stats",
        "capability": auth.CAP_LEADS_READ,
        "description": "Get GTM pipeline statistics: total leads, breakdown by tier/status/city, enrichment coverage rates.",
        "inputSchema": {"type": "object", "properties": {}},
    },
    {
        "name": "enrich_company",
        "capability": auth.CAP_LEADS_READ,
        "description": "Trigger on-demand enrichment for a lead — scrape website, find emails, social profiles, decision makers.",
        "inputSchema": {
            "type": "object",
            "properties": {"lead_id": {"type": "integer", "description": "The lead ID to enrich"}},
            "required": ["lead_id"],
        },
    },
    {
        "name": "verify_email",
        "capability": auth.CAP_LEADS_READ,
        "description": "Verify an email address using SMTP RCPT TO validation. Returns deliverable/undeliverable status.",
        "inputSchema": {
            "type": "object",
            "properties": {"email": {"type": "string", "description": "Email address to verify"}},
            "required": ["email"],
        },
    },
    {
        "name": "get_hiring_signals",
        "capability": auth.CAP_LEADS_READ,
        "description": "Check for active job postings at a company. Returns job titles, counts, and hiring velocity as buying signals.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "company": {"type": "string", "description": "Company name to check"},
                "lead_id": {"type": "integer", "description": "Optional lead ID for enriched context"},
            },
            "required": ["company"],
        },
    },
    {
        "name": "score_lead",
        "capability": auth.CAP_LEADS_READ,
        "description": "Calculate a lead quality score (0-100) based on data completeness, company signals, and contact depth.",
        "inputSchema": {
            "type": "object",
            "properties": {"lead_id": {"type": "integer", "description": "The lead ID to score"}},
            "required": ["lead_id"],
        },
    },
    {
        "name": "create_workbook",
        "capability": auth.CAP_WORKBOOKS_WRITE,  # write-gated (Phase 2 wires the real store call)
        "description": "Create a new workbook from a description. Auto-generates columns. Example: 'SaaS CTOs in SF with email and LinkedIn'",
        "inputSchema": {
            "type": "object",
            "properties": {
                "description": {"type": "string", "description": "Natural language description of the workbook"},
                "name": {"type": "string", "description": "Optional name for the workbook"},
            },
            "required": ["description"],
        },
    },
]

# tool name -> required capability
_TOOL_CAP: Dict[str, str] = {t["name"]: t["capability"] for t in TOOL_SPECS}


def _is_write_tool(name: str) -> bool:
    return _TOOL_CAP.get(name) in auth.WRITE_CAPABILITIES


def list_tools(ctx: MCPCtx) -> List[dict]:
    """Public tool specs visible to ``ctx`` — filtered to its capabilities.

    A tool is shown only when the token holds its capability AND, for write
    tools, ``MCP_WRITE_ENABLED`` is on. An agent never even sees a tool it
    cannot call.
    """
    out = []
    for t in TOOL_SPECS:
        cap = t["capability"]
        if cap not in ctx.capabilities:
            continue
        if cap in auth.WRITE_CAPABILITIES and not settings.MCP_WRITE_ENABLED:
            continue
        out.append({k: v for k, v in t.items() if k != "capability"})
    return out


async def execute_tool(ctx: MCPCtx, name: str, arguments: dict) -> str:
    """Execute one MCP tool scoped to ``ctx``. Returns a JSON text payload.

    Enforces capability + (for write tools) the global write flag before any
    work, then runs every store access inside ``workspace_scope(ctx.workspace_id)``
    so Postgres RLS scopes the transaction to this tenant.
    """
    cap = _TOOL_CAP.get(name)
    if cap is None:
        return json.dumps({"error": f"Unknown tool: {name}"})

    # Capability gate (defense in depth — list_tools already hides these).
    if cap not in ctx.capabilities:
        return json.dumps({"error": "capability not granted", "required": cap})

    # Write tools are inert in Phase 1 unless the flag is on (and even then a
    # token still needs the *:write grant). Two independent off-switches.
    if cap in auth.WRITE_CAPABILITIES and not settings.MCP_WRITE_ENABLED:
        return json.dumps({"error": "mcp writes disabled"})

    arguments = arguments or {}
    try:
        with workspace_scope(ctx.workspace_id):
            return await _dispatch(ctx, name, arguments)
    except Exception as e:  # keep the JSON-RPC envelope intact
        return json.dumps({"error": str(e)})


async def _dispatch(ctx: MCPCtx, name: str, arguments: dict) -> str:
    store = get_lead_store(ctx.workspace_id, ctx.slug)

    if name == "find_leads":
        leads = store.get_leads(
            search=arguments.get("query"),
            city=arguments.get("city"),
            score_tier=arguments.get("tier"),
            limit=arguments.get("limit", 10),
        )
        result = [
            {
                "id": l.id, "company": l.company, "city": l.city,
                "email": l.email, "phone": l.phone, "score": l.score,
                "tier": l.score_tier, "status": l.status,
                "website": l.website, "specialization": l.specialization,
                "company_size": l.company_size, "contact_person": l.contact_person,
            }
            for l in leads
        ]
        return json.dumps({"leads": result, "count": len(result)}, indent=2)

    if name == "get_lead_detail":
        lead = store.get_lead(arguments["lead_id"])
        if not lead:
            return json.dumps({"error": "Lead not found"})
        d = lead.to_dict()
        if d.get("decision_makers"):
            try:
                d["decision_makers"] = json.loads(d["decision_makers"])
            except Exception:
                pass
        return json.dumps(d, indent=2, default=str)

    if name == "get_pipeline_stats":
        return json.dumps(store.get_stats(), indent=2)

    if name == "enrich_company":
        # Resolve the lead through the scoped store first: a cross-tenant lead_id
        # is a miss (RLS + workspace filter), so enrichment spend can only ever
        # be triggered for the caller's own leads.
        lead = store.get_lead(arguments["lead_id"])
        if not lead:
            return json.dumps({"error": "Lead not found"})
        from apps.api.services.leadgen.enrichment.provider import WaterfallEnricher
        enricher = WaterfallEnricher()
        result = await enricher.enrich(lead)
        return json.dumps(
            {"enriched_fields": result.fields, "provider": result.provider}, indent=2
        )

    if name == "verify_email":
        email = arguments["email"]
        try:
            from apps.api.services.leadgen.enrichment.providers.mailscout_verify import (
                MailScoutVerifyProvider,
            )
            from apps.api.services.leadgen.models import Lead
            provider = MailScoutVerifyProvider()
            mock_lead = Lead(email=email, company="")
            result = await provider.enrich(mock_lead)
            return json.dumps({
                "email": email,
                "valid": result.success,
                "confidence": result.confidence,
                "fields": result.fields,
            }, indent=2)
        except Exception as e:
            return json.dumps({"email": email, "error": str(e)})

    if name == "get_hiring_signals":
        company = arguments["company"]
        try:
            from apps.api.services.leadgen.enrichment.providers.jobspy_signals import (
                JobSpySignalProvider,
            )
            from apps.api.services.leadgen.models import Lead
            provider = JobSpySignalProvider()
            mock_lead = Lead(company=company, id=arguments.get("lead_id", 0))
            result = await provider.enrich(mock_lead)
            return json.dumps({
                "company": company,
                "hiring": result.success,
                "data": result.fields,
            }, indent=2, default=str)
        except Exception as e:
            return json.dumps({"company": company, "error": str(e)})

    if name == "score_lead":
        from apps.api.services.leadgen.scoring import score_lead as _score
        lead = store.get_lead(arguments["lead_id"])
        if not lead:
            return json.dumps({"error": "Lead not found"})
        return json.dumps(_score(lead), indent=2, default=str)

    if name == "create_workbook":
        # Phase-1 placeholder (write-gated above). Persists NOTHING — Phase 2
        # replaces this with the real RLS-scoped workbook create. Returns the
        # auto-generated column plan only.
        description = arguments["description"]
        wb_name = arguments.get("name", f"MCP — {description[:40]}")
        desc_lower = description.lower()
        column_defs = [{"key": "company", "name": "Company", "type": "text"}]
        if any(w in desc_lower for w in ["email", "contact"]):
            column_defs.append({"key": "email", "name": "Email", "type": "email"})
        if any(w in desc_lower for w in ["phone", "call"]):
            column_defs.append({"key": "phone", "name": "Phone", "type": "phone"})
        if any(w in desc_lower for w in ["linkedin", "social"]):
            column_defs.append({"key": "linkedin_url", "name": "LinkedIn", "type": "url"})
        if any(w in desc_lower for w in ["title", "cto", "ceo", "vp", "founder"]):
            column_defs.append({"key": "contact_person", "name": "Contact", "type": "text"})
            column_defs.append({"key": "contact_title", "name": "Title", "type": "text"})
        if any(w in desc_lower for w in ["city", "location"]):
            column_defs.append({"key": "city", "name": "City", "type": "text"})
        keys = [c["key"] for c in column_defs]
        if "email" not in keys:
            column_defs.append({"key": "email", "name": "Email", "type": "email"})
        if "contact_person" not in keys:
            column_defs.append({"key": "contact_person", "name": "Contact", "type": "text"})
        return json.dumps({
            "name": wb_name,
            "columns": [c["name"] for c in column_defs],
            "message": f"Workbook '{wb_name}' plan ready with {len(column_defs)} columns (not persisted).",
        }, indent=2)

    return json.dumps({"error": f"Unknown tool: {name}"})
