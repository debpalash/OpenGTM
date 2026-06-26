"""MCP tool catalog + execution, RLS-scoped.

Every tool runs against the workspace-scoped, RLS-protected store
(``get_lead_store(workspace_id, slug)``) inside ``workspace_scope`` — never a
bare ``LeadDB()`` on the shared global file. This is the fix for the current
cross-tenant hole (a tool call could read every tenant's leads). The dispatch
here is the trusted server-side half; ``apps/mcp/server.py`` is now a thin
transport that authenticates, resolves an :class:`MCPCtx`, filters the catalog
to the token's capabilities, and delegates here.

Phase 2 adds the first WRITE tools — ``create_lead``, ``update_lead``,
``enroll_leads`` and a real ``create_workbook`` — each guarded by SIX
independent gates before it touches data:

  1. the per-token capability grant (``leads:write`` / ``sequences:enroll`` /
     ``workbooks:write``), filtered out of ``tools/list`` when absent;
  2. the global ``MCP_WRITE_ENABLED`` flag (default OFF) — two off-switches;
  3. a LIVE ``require_role`` re-check against the user's CURRENT workspace role
     (a downgrade to viewer denies the write even with a valid token);
  4. a per-workspace daily write cap + idempotency reservation (``mcp.caps``,
     reusing the automations ledger) so retries don't double-apply and writes
     can't exceed the tenant budget;
  5. the SAME RLS-scoped stores as the REST API inside ``workspace_scope`` so a
     token bound to workspace A can never write to B (args naming B are ignored —
     the workspace comes from the authenticated token, never the call);
  6. exactly one ``mcp_audit_log`` row per write attempt INCLUDING denials
     (capability/role/cap/error), the forensic trail for the confused-deputy case.

Deferred to v2 (NOT exposed here): ``create_automation``, sends, bulk ops,
``delete_lead``, sequence start/pause — too much blast radius for the first write
release.
"""

from __future__ import annotations

import json
from typing import Dict, List

from apps.api.core.config import settings
from apps.api.core.tenancy import workspace_scope
from apps.api.services.leadgen.store import get_lead_store
from apps.api.services.mcp import auth, audit
from apps.api.services.mcp import caps as mcp_caps
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
    # ── Write tools (Phase 2; hidden unless MCP_WRITE_ENABLED + the cap) ──────
    {
        "name": "create_lead",
        "capability": auth.CAP_LEADS_WRITE,
        "description": "Create a new lead in the caller's workspace. Persists through the same RLS-scoped store as the REST API.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "company": {"type": "string", "description": "Company name (required)"},
                "email": {"type": "string"},
                "phone": {"type": "string"},
                "website": {"type": "string"},
                "city": {"type": "string"},
                "state": {"type": "string"},
                "contact_person": {"type": "string"},
                "contact_title": {"type": "string"},
                "specialization": {"type": "string"},
                "company_size": {"type": "string"},
                "description": {"type": "string"},
                "linkedin_url": {"type": "string"},
                "notes": {"type": "string"},
                "source": {"type": "string"},
                "idempotency_key": {"type": "string", "description": "Optional; a retry with the same key is a no-op (no double-create)."},
            },
            "required": ["company"],
        },
    },
    {
        "name": "update_lead",
        "capability": auth.CAP_LEADS_WRITE,
        "description": "Update fields on an existing lead by ID (workspace-scoped — a lead in another workspace is not found). Only an allowlisted set of business fields can be written.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "lead_id": {"type": "integer", "description": "The lead ID to update"},
                "email": {"type": "string"},
                "phone": {"type": "string"},
                "website": {"type": "string"},
                "city": {"type": "string"},
                "state": {"type": "string"},
                "contact_person": {"type": "string"},
                "contact_title": {"type": "string"},
                "specialization": {"type": "string"},
                "company_size": {"type": "string"},
                "description": {"type": "string"},
                "linkedin_url": {"type": "string"},
                "notes": {"type": "string"},
                "status": {"type": "string", "description": "Lead status: new, contacted, qualified, converted, dead"},
                "idempotency_key": {"type": "string", "description": "Optional; a retry with the same key is a no-op."},
            },
            "required": ["lead_id"],
        },
    },
    {
        "name": "enroll_leads",
        "capability": auth.CAP_SEQUENCES_ENROLL,
        "description": "Enroll leads into an outreach sequence. Skips leads with no email or on the suppression list and records consent (CAN-SPAM). Sequence + leads must be in the caller's workspace.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "sequence_id": {"type": "string", "description": "The sequence ID to enroll into"},
                "lead_ids": {"type": "array", "items": {"type": "integer"}, "description": "Lead IDs to enroll"},
                "idempotency_key": {"type": "string", "description": "Optional; a retry with the same key is a no-op."},
            },
            "required": ["sequence_id", "lead_ids"],
        },
    },
    {
        "name": "create_workbook",
        "capability": auth.CAP_WORKBOOKS_WRITE,  # write-gated; Phase 2 persists a real RLS-scoped workbook
        "description": "Create a new workbook from a description. Auto-generates columns. Example: 'SaaS CTOs in SF with email and LinkedIn'",
        "inputSchema": {
            "type": "object",
            "properties": {
                "description": {"type": "string", "description": "Natural language description of the workbook"},
                "name": {"type": "string", "description": "Optional name for the workbook"},
                "idempotency_key": {"type": "string", "description": "Optional; a retry with the same key is a no-op."},
            },
            "required": ["description"],
        },
    },
]

# Write tools route through the cap/role/idempotency/audit wrapper. The set is
# derived from the catalog so it stays in sync (any *:write tool is a write).
WRITE_TOOLS = frozenset(
    t["name"] for t in TOOL_SPECS if t["capability"] in auth.WRITE_CAPABILITIES
)

# Allowlisted lead fields an MCP write may set — NO arbitrary column writes
# (id/workspace_id/created_at/score internals/provenance are never accepted).
_LEAD_WRITE_FIELDS = frozenset({
    "company", "website", "email", "phone", "contact_person", "contact_title",
    "city", "state", "address", "specialization", "company_size", "description",
    "industry_tags", "linkedin_url", "twitter_url", "facebook_url", "notes",
    "status", "source",
})

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

    arguments = arguments or {}
    is_write = name in WRITE_TOOLS

    # Capability gate (defense in depth — list_tools already hides these). A
    # write tool reached without its grant is an anomaly → audit the denial.
    if cap not in ctx.capabilities:
        if is_write:
            audit.record(ctx, name, arguments, result_status="denied",
                         error="capability not granted")
        return json.dumps({"error": "capability not granted", "required": cap})

    # Write tools are inert unless the flag is on (and even then a token still
    # needs the *:write grant). Two independent off-switches — audit the denial.
    if is_write and not settings.MCP_WRITE_ENABLED:
        audit.record(ctx, name, arguments, result_status="denied",
                     error="mcp writes disabled")
        return json.dumps({"error": "mcp writes disabled"})

    # Writes carry the full guard rail (role re-check + caps + idempotency +
    # audit). Reads stay on the lean path (not audited — owner decision #3).
    if is_write:
        return await _execute_write(ctx, name, arguments)

    try:
        with workspace_scope(ctx.workspace_id):
            return await _dispatch(ctx, name, arguments)
    except Exception as e:  # keep the JSON-RPC envelope intact
        return json.dumps({"error": str(e)})


# ── Write path: role re-check → caps/idempotency → store → audit ─────────────

async def _execute_write(ctx: MCPCtx, name: str, arguments: dict) -> str:
    """Run one write tool behind the live role check, the daily cap, idempotency,
    and a mandatory audit row (one per attempt, incl. every denial).
    """
    idem = arguments.get("idempotency_key") or None

    # (3) LIVE role re-check — a capability grant alone is never sufficient. A
    # user downgraded after the token was minted is denied here.
    try:
        auth.require_role(ctx, *auth.WRITE_ROLES)
    except auth.MCPAuthError as e:
        audit.record(ctx, name, arguments, result_status="denied", error=str(e))
        return json.dumps({"error": str(e)})

    # (4) Reserve against the per-workspace daily write cap (idempotent on idem).
    reservation = mcp_caps.reserve_write(ctx.workspace_id, idem)
    if reservation.replay:
        # Already applied for this key — do NOT re-run the mutation.
        audit.record(ctx, name, arguments, result_status="ok",
                     error="idempotent_replay")
        return json.dumps({
            "status": "duplicate",
            "idempotency_key": reservation.idempotency_key,
            "message": "already applied (idempotent replay)",
        })
    if not reservation.ok:
        audit.record(ctx, name, arguments, result_status="capped",
                     error="daily MCP write cap exceeded")
        return json.dumps({"error": "mcp write cap exceeded"})

    # (5) Perform the write through the SAME RLS-scoped stores as REST.
    try:
        with workspace_scope(ctx.workspace_id):
            result_text = await _dispatch_write(ctx, name, arguments)
    except Exception as e:
        mcp_caps.release_write(ctx.workspace_id, reservation)
        audit.record(ctx, name, arguments, result_status="error", error=str(e))
        return json.dumps({"error": str(e)})

    mcp_caps.settle_write(ctx.workspace_id, reservation)
    # (6) One audit row per applied write, keyed by the idempotency key.
    audit.record(ctx, name, arguments, result_status="ok",
                 idempotency_key=reservation.idempotency_key)
    return result_text


def _lead_write_fields(arguments: dict) -> Dict[str, object]:
    """Allowlist + drop empty values from a lead create/update payload."""
    return {
        k: v for k, v in arguments.items()
        if k in _LEAD_WRITE_FIELDS and v is not None
    }


async def _dispatch_write(ctx: MCPCtx, name: str, arguments: dict) -> str:
    store = get_lead_store(ctx.workspace_id, ctx.slug)

    if name == "create_lead":
        from apps.api.services.leadgen.models import Lead

        fields = _lead_write_fields(arguments)
        if not (fields.get("company") or "").strip():
            return json.dumps({"error": "company is required"})
        # workspace_id is force-stamped by the store from ctx — never from args.
        lead = Lead(**fields)
        lead_id = store.upsert_lead(lead)
        return json.dumps({"status": "created", "lead_id": lead_id,
                           "company": fields["company"]}, default=str)

    if name == "update_lead":
        lead_id = arguments.get("lead_id")
        if lead_id is None:
            return json.dumps({"error": "lead_id is required"})
        # Resolve through the scoped store first: a cross-tenant id is a miss
        # under RLS + the workspace filter, so a token bound to A can never
        # update B's lead even if the id is guessed.
        if store.get_lead(lead_id) is None:
            return json.dumps({"error": "Lead not found"})
        fields = _lead_write_fields(arguments)
        status = fields.pop("status", None)
        if fields:
            store.update_lead_fields(lead_id, fields)
        if status is not None:
            store.update_status(lead_id, status)
        updated = sorted([*fields.keys(), *(["status"] if status is not None else [])])
        return json.dumps({"status": "updated", "lead_id": lead_id, "fields": updated})

    if name == "enroll_leads":
        from datetime import datetime, timezone
        from apps.api.services.outreach.normalize import normalize_email
        from apps.api.services.outreach.store import get_outreach_store

        seq_id = arguments.get("sequence_id")
        lead_ids = arguments.get("lead_ids") or []
        ostore = get_outreach_store(ctx.workspace_id)
        # Anti-orphan / cross-tenant: the sequence must exist IN THIS workspace.
        if not seq_id or not ostore.sequence_exists(seq_id):
            return json.dumps({"error": "Sequence not found"})
        enrolled, skipped = 0, []
        now = datetime.now(timezone.utc)
        for lead_id in lead_ids:
            lead = store.get_lead(lead_id)  # workspace-scoped → B's leads = miss
            email = normalize_email(getattr(lead, "email", "") if lead else "")
            if not email:
                skipped.append({"lead_id": lead_id, "reason": "no_email"})
                continue
            if ostore.is_suppressed(email):
                skipped.append({"lead_id": lead_id, "reason": "suppressed"})
                continue
            eid = ostore.enroll(
                seq_id, lead_id, email,
                consent_source="mcp_enroll", consent_at=now,
            )
            if eid is not None:
                enrolled += 1
            else:
                skipped.append({"lead_id": lead_id, "reason": "already_enrolled"})
        return json.dumps({"enrolled": enrolled, "skipped": skipped,
                           "total_lead_ids": len(lead_ids)})

    if name == "create_workbook":
        from apps.api.database import SessionLocal
        from apps.api.services.workbook.models import Workbook

        description = arguments["description"]
        wb_name = arguments.get("name") or f"MCP — {description[:40]}"
        columns_config = _plan_workbook_columns(description)
        with SessionLocal() as s, s.begin():
            wb = Workbook(
                name=wb_name,
                description=description,
                workspace_id=ctx.workspace_id,  # from ctx; RLS WITH CHECK binds it
                source_type="empty",
                source_config={},
                filter_criteria={},
                columns_config=columns_config,
            )
            s.add(wb)
            s.flush()
            wb_id = wb.id
        return json.dumps({
            "status": "created",
            "workbook_id": wb_id,
            "name": wb_name,
            "columns": [c["name"] for c in columns_config],
        })

    return json.dumps({"error": f"Unknown tool: {name}"})


def _plan_workbook_columns(description: str) -> List[dict]:
    """Auto-generate a workbook column plan (lead_field columns) from a prompt."""
    desc_lower = (description or "").lower()
    cols = [{"id": "company", "name": "Company", "type": "lead_field", "lead_field": "company"}]

    def _add(key: str, label: str):
        if key not in {c["id"] for c in cols}:
            cols.append({"id": key, "name": label, "type": "lead_field", "lead_field": key})

    if any(w in desc_lower for w in ["email", "contact"]):
        _add("email", "Email")
    if any(w in desc_lower for w in ["phone", "call"]):
        _add("phone", "Phone")
    if any(w in desc_lower for w in ["linkedin", "social"]):
        _add("linkedin_url", "LinkedIn")
    if any(w in desc_lower for w in ["title", "cto", "ceo", "vp", "founder"]):
        _add("contact_person", "Contact")
        _add("contact_title", "Title")
    if any(w in desc_lower for w in ["city", "location"]):
        _add("city", "City")
    # Always include email + a contact so the workbook is usable for outreach.
    _add("email", "Email")
    _add("contact_person", "Contact")
    return cols


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
            # Route through the shared cascade so MCP benefits from Reacher (when
            # enabled) + the per-email cache + SMTP fallback. The workspace is
            # resolved from the AUTHENTICATED session (ctx), never tool args, so a
            # caller can't borrow another tenant's verifier config (confused deputy).
            from apps.api.services.leadgen.enrichment import email_verify_cascade as cascade
            vr = await cascade.verify_email(email, workspace_id=ctx.workspace_id)
            return json.dumps({
                "email": email,
                "status": vr.status,                  # valid|invalid|catch_all|unknown
                "valid": vr.status == cascade.VALID,
                "deliverable": vr.deliverable,
                "confidence": vr.confidence,
                "source": vr.source,
                "detail": vr.detail,
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

    return json.dumps({"error": f"Unknown tool: {name}"})
