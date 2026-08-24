"""
CopilotKit Runtime — FastAPI endpoint

Provides the /api/copilotkit endpoint that CopilotKit's frontend connects to.
Uses the configured AI provider from the settings database.
Supports the CopilotKit protocol: /info (GET+POST) and chat (POST).
Includes: conversation history, OpenMemory integration, tool execution.
"""

import json
import os
import functools
import httpx
import uuid
import asyncio
from datetime import datetime, timezone
from typing import AsyncGenerator, Optional, Tuple
from fastapi import APIRouter, Request, HTTPException
from fastapi.responses import StreamingResponse, JSONResponse

from apps.api.routers.settings import _db_get, PROVIDERS
from apps.api.core.config import settings
from apps.api.core.tenancy import workspace_scope
from apps.api.services.leadgen.store import get_lead_store
from apps.api.services.workspace import manager as ws_manager
from apps.api.services import chat_history, memory

router = APIRouter(prefix="/api/copilotkit", tags=["CopilotKit"])


def _max_tool_rounds() -> int:
    """Max ReAct tool-use rounds per turn before the model is forced to answer.

    DB-tunable (settings key CHAT_MAX_TOOL_ROUNDS) so it can change without a
    redeploy, matching the provider-config pattern.
    """
    try:
        return max(1, int(_db_get("CHAT_MAX_TOOL_ROUNDS", "8")))
    except (ValueError, TypeError):
        return 8


def _resolve_provider(provider_id: str) -> dict:
    """Resolve a provider ID into a full config dict with credentials."""
    prov = PROVIDERS.get(provider_id)
    if not prov:
        return None

    api_key = _db_get(prov["env_key"], "")
    base_url = _db_get(prov.get("env_url", ""), "") or prov.get("default_url", "")
    model = _db_get(prov.get("env_model", ""), "") or prov.get("default_model", "")

    if not api_key or not base_url:
        return None

    return {
        "id": provider_id,
        "name": prov.get("name", provider_id),
        "api_key": api_key,
        "base_url": base_url,
        "model": model,
        "openai_compatible": prov.get("openai_compatible", True),
    }


def _get_provider_chain() -> list:
    """Get all configured providers in priority order (active first, then fallbacks).

    Returns a list of provider dicts, each with API keys and URLs resolved.
    Only includes providers that have an API key configured.
    """
    default_id = _db_get("LLM_DEFAULT_PROVIDER", "openrouter")
    chain = []
    seen = set()

    # Active provider first
    active = _resolve_provider(default_id)
    if active:
        chain.append(active)
        seen.add(default_id)

    # Then all other configured providers as fallbacks
    for pid in PROVIDERS:
        if pid in seen:
            continue
        prov = PROVIDERS[pid]
        if not prov.get("openai_compatible", True):
            continue  # Skip non-OpenAI-compatible providers for failover
        resolved = _resolve_provider(pid)
        if resolved:
            chain.append(resolved)
            seen.add(pid)

    return chain


def _get_active_provider() -> dict:
    """Get the primary configured AI provider (backward compat)."""
    chain = _get_provider_chain()
    if chain:
        return chain[0]
    # Absolute fallback — return openrouter even without a key
    default_id = _db_get("LLM_DEFAULT_PROVIDER", "openrouter")
    prov = PROVIDERS.get(default_id, PROVIDERS["openrouter"])
    return {
        "id": default_id,
        "name": prov.get("name", default_id),
        "api_key": "",
        "base_url": prov.get("default_url", ""),
        "model": prov.get("default_model", ""),
        "openai_compatible": prov.get("openai_compatible", True),
    }


# ── Runtime info — handles CopilotKit SDK handshake ──────────────

_INFO_RESPONSE = {
    "actions": [
        {"name": "search_leads", "description": "Search leads in the database"},
        {"name": "get_lead_stats", "description": "Get pipeline statistics"},
        {"name": "update_lead_status", "description": "Update a lead's status"},
        {"name": "start_collection", "description": "Start collecting new leads"},
    ],
}


@router.get("/info")
def copilot_info_get():
    """Runtime info — GET handler."""
    return JSONResponse(content=_INFO_RESPONSE)


@router.post("/info")
async def copilot_info_post():
    """Runtime info — POST handler (CopilotKit SDK sends POST)."""
    return JSONResponse(content=_INFO_RESPONSE)


# ── Conversation endpoints ───────────────────────────────────────


@router.get("/conversations")
def list_conversations(request: Request):
    """List the caller's chat conversations (scoped to workspace + user).

    Authorizes via the same fail-closed path as the chat endpoint: cloud
    requires a valid token + member workspace (401/403 otherwise); self-host
    binds to the keyless `main` workspace.
    """
    workspace_id, user_id, _slug = _resolve_chat_workspace(request)
    convs = chat_history.list_conversations(workspace_id, user_id)
    return JSONResponse(content={"conversations": convs})


@router.get("/conversations/{conv_id}")
def get_conversation(conv_id: str, request: Request):
    """Get messages for a conversation the caller owns."""
    workspace_id, user_id, _slug = _resolve_chat_workspace(request)
    conv = chat_history.get_conversation(conv_id, workspace_id, user_id)
    if not conv:
        return JSONResponse(content={"error": "Not found"}, status_code=404)
    messages = chat_history.get_messages(conv_id, workspace_id, user_id)
    return JSONResponse(content={"conversation": conv, "messages": messages})


@router.delete("/conversations/{conv_id}")
def delete_conversation(conv_id: str, request: Request):
    """Delete a conversation the caller owns (no-op cross-tenant)."""
    workspace_id, user_id, _slug = _resolve_chat_workspace(request)
    chat_history.delete_conversation(conv_id, workspace_id, user_id)
    return JSONResponse(content={"ok": True})


@router.get("/memories")
def list_memories(request: Request):
    """List the caller's stored memories (debug/transparency), scoped to tenant."""
    workspace_id, user_id, _slug = _resolve_chat_workspace(request)
    memories = memory.get_all_memories(workspace_id, user_id)
    return JSONResponse(content={
        "memories": memories,
        "available": memory.is_available(),
    })


def _build_system_prompt(store=None) -> str:
    """Build a dynamic system prompt with ICP and live pipeline stats.

    ``store`` is the request's tenant-scoped lead store; the pipeline stats are
    read from it so the prompt reflects THIS workspace's data, not a global
    cross-tenant view.
    """
    
    # Load OpenUI Lang spec
    openui_spec = ""
    try:
        with open("data/openui_system_prompt.txt", "r") as f:
            openui_spec = f.read()
    except Exception:
        pass

    try:
        from apps.api.services.leadgen.config import ICP
        icp_text = (
            f"**Value Proposition:** {ICP.get('value_proposition', 'B2B SaaS CRM')}\n"
            f"**Target Industries:** {', '.join(ICP.get('target_industries', [])[:8])}\n"
            f"**Target Cities:** {', '.join(ICP.get('target_cities', [])[:8])}\n"
            f"**Preferred Size:** {ICP.get('min_company_size', 10)}+ employees\n"
            f"**Preferred Specializations:** {', '.join(ICP.get('preferred_specializations', [])[:5])}"
        )
    except Exception:
        icp_text = "B2B SaaS targeting HR/staffing companies in India"

    try:
        stats = store.get_stats() if store is not None else {}
        total = stats.get("total", 0)
        by_tier = stats.get("by_tier", {})
        stats_text = (
            f"**Total Leads:** {total}\n"
            f"**By Tier:** Hot: {by_tier.get('hot', 0)}, Warm: {by_tier.get('warm', 0)}, "
            f"Cold: {by_tier.get('cold', 0)}, Unqualified: {by_tier.get('unqualified', 0)}"
        )
    except Exception:
        stats_text = "Pipeline stats unavailable"

    return f"""You are an OpenGTM Agent, an expert B2B go-to-market intelligence assistant for OpenGTM.

## Your Ideal Customer Profile (ICP)
{icp_text}

## Current Pipeline Stats
{stats_text}

## Your Capabilities
You have powerful tools to interact with the lead database. Use them proactively:
- **search_leads** — Find leads by name, city, status, industry
- **get_lead_detail** — Get full profile with decision makers, enrichment data
- **get_lead_stats** — Pipeline overview and metrics
- **update_lead_status** — Move leads through the pipeline
- **start_collection** — Trigger new lead collection from 6 sources
- **enrich_lead** — Trigger on-demand enrichment (website scrape + contact discovery)
- **find_similar_leads** — Find leads similar to a given company
- **get_enrichment_gaps** — Show leads missing email/phone/linkedin
- **suggest_outreach** — Generate personalized outreach messages
- **compare_leads** — Side-by-side comparison of leads
- **ambitionbox_search** — Search up to 100 AmbitionBox companies with ratings, reviews, employee counts, industry data. For "top N" requests, set limit=N. Common aliases such as HR/Human Resources and SaaS are normalized automatically
- **ambitionbox_jobs** — Get current job listings for a company from AmbitionBox (requires company_id from ambitionbox_search)
- **import_ambitionbox_to_workbook** — Snapshot an AmbitionBox search into a new workbook. When the user says "add/save those results to a workbook", use the SAME industry, rating, sort, and limit from their search with this tool. Do not use create_source_workbook for already-found AmbitionBox results
- **draft_plan / execute_plan** — Autopilot for COMPOUND goals (e.g. "build a list of 50 IT staffing firms in Pune and find their founders' emails"): call draft_plan to produce a step-by-step plan, show it to the user, then call execute_plan with that plan (the user approves before anything runs). Use this instead of many manual tool calls for multi-step build-a-list-and-enrich requests.

## Response Guidelines
- For conversational text, explanations, or simple answers, respond in plain markdown.
- Only use OpenUI Lang when displaying structured data (leads, stats, comparisons).
- Be **actionable**: don't just show data, suggest specific next steps
- When asked to find/collect leads, use start_collection tool
- Keep responses concise but data-rich
- Score context: Hot (75-100), Warm (50-74), Cold (25-49), Unqualified (0-24)

## Action Safety (human-in-the-loop)
Read-only tools (search_leads, get_lead_detail, get_lead_stats, find_similar_leads,
get_enrichment_gaps, suggest_outreach, compare_leads, ambitionbox_search,
ambitionbox_jobs) run immediately.
Tools that mutate data or spend resources (update_lead_status, start_collection,
enrich_lead, import_ambitionbox_to_workbook, execute_plan) require explicit user approval:
when you call one, the system pauses and asks the user to confirm before it runs.
So propose the action with a one-line rationale and let the gate handle approval —
do not claim the action is done until you receive its tool result.

## OpenUI UI Generation (STRICT SYNTAX REQUIRED)
You MUST use the custom OpenUI Lang syntax below when generating structured UI. 
**NEVER use XML, HTML, or JSON for the UI.** Do not use `<vertical_sequence>`, `<card>`, or any angle brackets.
You must use exact assignment syntax like `root = Root([chart1, chart2])` and `chart1 = SimplePieChart(...)`.

{openui_spec}
"""


# ── Tool Safety Classification ───────────────────────────────────

# Tools that MUTATE state, cost resources, or delete data.
# These require user confirmation before execution.
DANGEROUS_TOOLS = {
    "start_collection": {
        "level": "high",
        "label": "🚀 Launch Lead Collection",
        "reason": "Runs 7 parallel scraping strategies using DDG, Maps, and external APIs.",
    },
    "update_lead_status": {
        "level": "medium",
        "label": "📝 Update Lead Status",
        "reason": "Changes the pipeline status of a lead.",
    },
    "enrich_lead": {
        "level": "medium",
        "label": "🔍 Enrich Lead",
        "reason": "Triggers external website scraping and contact discovery.",
    },
    "execute_plan": {
        "level": "high",
        "label": "🤖 Run Autopilot Plan",
        "reason": "Builds a workbook and runs sourcing + agent-column enrichment (spends resources).",
    },
    "import_ambitionbox_to_workbook": {
        "level": "medium",
        "label": "📊 Create AmbitionBox Workbook",
        "reason": "Creates a workbook and stores the matching AmbitionBox companies as rows.",
    },
    "create_source_workbook": {
        "level": "high",
        "label": "📚 Create and Source Workbook",
        "reason": "Creates persistent data and launches a background sourcing workflow.",
    },
    "set_workbook_refresh": {
        "level": "medium",
        "label": "⏱️ Change Workbook Refresh",
        "reason": "Changes a recurring schedule that can repeatedly spend resources.",
    },
    "add_agent_column": {
        "level": "medium",
        "label": "🤖 Add Agent Column",
        "reason": "Changes workbook configuration and may run paid AI tools.",
    },
    "add_signal_trigger": {
        "level": "medium",
        "label": "⚡ Add Signal Trigger",
        "reason": "Creates a persistent automation trigger.",
    },
}

# Tools that are safe to execute without confirmation (read-only).
SAFE_TOOLS = {
    "search_leads", "get_lead_detail", "get_lead_stats",
    "find_similar_leads", "get_enrichment_gaps", "suggest_outreach",
    "compare_leads", "ambitionbox_search", "ambitionbox_jobs",
    "draft_plan",
}


def _resolve_chat_workspace(request: Request) -> Tuple[str, Optional[int], str]:
    """Resolve (and authorize) the workspace for a chat request.

    Returns ``(workspace_id, user_id, slug)``. This is the single tenant
    decision point for the whole chat tool path — everything downstream scopes
    to whatever this returns, so it MUST fail closed in cloud.

    * Cloud / multi-tenant (``settings.CHAT_REQUIRE_AUTH`` true): authenticate
      exactly like the ``current_workspace`` dependency — read the
      ``Authorization: Bearer`` token + ``X-Workspace-Id`` header, resolve the
      user, and enforce ``ws_manager.is_member``. Missing/invalid auth → 401;
      non-member (or no resolvable) workspace → 403. NEVER falls through to a
      default workspace.
    * Self-host (SQLite / ``CHAT_REQUIRE_AUTH`` false): keyless. Binds to the
      ``main`` default workspace via ``ws_manager._get_active_workspace_id()``.
      ``user_id`` is None.
    """
    if not settings.CHAT_REQUIRE_AUTH:
        ws_id = ws_manager._get_active_workspace_id()
        slug = ws_manager.workspace_slug(ws_id) or "main"
        return ws_id, None, slug

    # ── Cloud: fail-closed authentication ──
    from jose import JWTError, jwt
    from apps.api.database import SessionLocal as AppSessionLocal
    from apps.api.models import User

    _unauth = HTTPException(
        status_code=401,
        detail="Could not validate credentials",
        headers={"WWW-Authenticate": "Bearer"},
    )

    auth_header = request.headers.get("Authorization", "")
    scheme, _, token = auth_header.partition(" ")
    if scheme.lower() != "bearer" or not token:
        raise _unauth
    try:
        payload = jwt.decode(
            token, settings.SECRET_KEY, algorithms=[settings.ALGORITHM]
        )
        # Refresh tokens must NOT be accepted as access tokens.
        if payload.get("type") == "refresh":
            raise _unauth
        username = payload.get("sub")
        if not username:
            raise _unauth
    except JWTError:
        raise _unauth

    with AppSessionLocal() as s:
        user = s.query(User).filter(User.username == username).first()
        if user is None or not user.is_active:
            raise _unauth
        user_id = user.id

    ws_id = request.headers.get("X-Workspace-Id") or ws_manager.get_user_active_workspace(user_id)
    if not ws_id:
        raise HTTPException(
            status_code=403,
            detail="No accessible workspace. Ask an admin to add you to one.",
        )
    if not ws_manager.is_member(ws_id, user_id):
        # Don't leak existence — same response whether missing or not the caller's.
        raise HTTPException(status_code=403, detail="Workspace access denied")
    slug = ws_manager.workspace_slug(ws_id)
    if not slug:
        raise HTTPException(status_code=404, detail="Workspace not found")
    return ws_id, user_id, slug


def _describe_action(fn_name: str, fn_args: dict, workspace_id: Optional[str] = None) -> str:
    """Generate a human-readable description of a tool action for the confirmation dialog."""
    meta = DANGEROUS_TOOLS.get(fn_name, {})
    label = meta.get("label", fn_name)
    reason = meta.get("reason", "This action modifies data.")

    # The autopilot plan describes itself (one step per line) — render the
    # SERVER-STORED plan (by plan_id), NOT the client args, so the gate displays
    # exactly what will execute (approval-integrity). Fall back to the echoed
    # plan body for older clients that don't thread plan_id.
    if fn_name == "execute_plan":
        from apps.api.services.agent import autopilot, autopilot_plan_store
        plan_id = fn_args.get("plan_id")
        plan = autopilot_plan_store.peek(workspace_id, plan_id) if plan_id else None
        if not plan:
            plan = fn_args.get("plan") or {}
        return autopilot.describe_plan(plan) if plan.get("steps") else f"{label}\n{reason}"

    details = ""
    if fn_name == "start_collection":
        details = f"Query: \"{fn_args.get('query', '?')}\""
    elif fn_name == "update_lead_status":
        details = f"Lead #{fn_args.get('lead_id', '?')} → {fn_args.get('status', '?')}"
    elif fn_name == "enrich_lead":
        details = f"Lead #{fn_args.get('lead_id', '?')}"
    elif fn_name == "import_ambitionbox_to_workbook":
        details = (
            f"{fn_args.get('limit', 10)} companies"
            + (f" · Industry: {fn_args['industry']}" if fn_args.get("industry") else "")
            + (f" · Workbook: {fn_args['name']}" if fn_args.get("name") else "")
        )

    return f"{label}\n{reason}\n{details}"


def _needs_confirmation(fn_name: str) -> bool:
    """Only explicitly read-only tools bypass confirmation.

    Unknown/hallucinated tool names fail closed and are never executed inline.
    """
    return fn_name not in SAFE_TOOLS


# ── Server-side tool definitions ─────────────────────────────────

def _build_tools():
    """Define the backend tools available to the copilot."""
    return [
        {
            "type": "function",
            "function": {
                "name": "search_leads",
                "description": "Search for leads in the database by company name, city, status, tier, or any keyword. Returns matching leads with contact info and scores.",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "query": {"type": "string", "description": "Search query (company name, keyword, industry)"},
                        "city": {"type": "string", "description": "Filter by city"},
                        "status": {"type": "string", "description": "Filter by status: new, contacted, qualified, dead"},
                        "score_tier": {"type": "string", "description": "Filter by tier: hot, warm, cold, unqualified"},
                        "limit": {"type": "integer", "description": "Max results to return", "default": 10},
                    },
                    "required": ["query"],
                },
            },
        },
        {
            "type": "function",
            "function": {
                "name": "get_lead_detail",
                "description": "Get complete detailed profile of a specific lead by ID, including decision makers, enrichment data, all contact info, and company details",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "lead_id": {"type": "integer", "description": "The lead ID"},
                    },
                    "required": ["lead_id"],
                },
            },
        },
        {
            "type": "function",
            "function": {
                "name": "get_lead_stats",
                "description": "Get comprehensive pipeline statistics: total leads, breakdown by tier/status/city/source, enrichment coverage",
                "parameters": {"type": "object", "properties": {}},
            },
        },
        {
            "type": "function",
            "function": {
                "name": "update_lead_status",
                "description": "Update the status of a lead by ID",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "lead_id": {"type": "integer", "description": "The lead ID"},
                        "status": {"type": "string", "enum": ["new", "contacted", "qualified", "negotiating", "converted", "dead"]},
                        "note": {"type": "string", "description": "Optional note about the status change"},
                    },
                    "required": ["lead_id", "status"],
                },
            },
        },
        {
            "type": "function",
            "function": {
                "name": "start_collection",
                "description": "Start collecting new leads for a search query. Runs 6 strategies in parallel: Maps, Web, Directories, LinkedIn, Job Boards, Review Sites. Example: 'IT staffing companies in Pune'",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "query": {"type": "string", "description": "The lead collection query"},
                    },
                    "required": ["query"],
                },
            },
        },
        {
            "type": "function",
            "function": {
                "name": "enrich_lead",
                "description": "Trigger on-demand enrichment for a specific lead: scrape their website for contact info, find social profiles",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "lead_id": {"type": "integer", "description": "The lead ID to enrich"},
                    },
                    "required": ["lead_id"],
                },
            },
        },
        {
            "type": "function",
            "function": {
                "name": "find_similar_leads",
                "description": "Find leads similar to a given lead by matching industry/specialization and city",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "lead_id": {"type": "integer", "description": "The reference lead ID"},
                        "limit": {"type": "integer", "description": "Max similar leads to return", "default": 10},
                    },
                    "required": ["lead_id"],
                },
            },
        },
        {
            "type": "function",
            "function": {
                "name": "get_enrichment_gaps",
                "description": "Show leads that are missing key data: email, phone, LinkedIn, or decision makers. Helps prioritize enrichment.",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "missing_field": {"type": "string", "enum": ["email", "phone", "linkedin", "decision_makers", "any"], "description": "Which field is missing"},
                        "min_score": {"type": "integer", "description": "Only show leads with score >= this", "default": 30},
                        "limit": {"type": "integer", "description": "Max results", "default": 15},
                    },
                    "required": ["missing_field"],
                },
            },
        },
        {
            "type": "function",
            "function": {
                "name": "suggest_outreach",
                "description": "Generate a personalized cold outreach message (email or LinkedIn) for a specific lead",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "lead_id": {"type": "integer", "description": "The lead ID"},
                        "channel": {"type": "string", "enum": ["email", "linkedin"], "description": "Outreach channel", "default": "email"},
                    },
                    "required": ["lead_id"],
                },
            },
        },
        {
            "type": "function",
            "function": {
                "name": "compare_leads",
                "description": "Compare 2-3 leads side-by-side with scoring breakdown, data completeness, and recommendation",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "lead_ids": {"type": "array", "items": {"type": "integer"}, "description": "List of 2-3 lead IDs to compare"},
                    },
                    "required": ["lead_ids"],
                },
            },
        },
        {
            "type": "function",
            "function": {
                "name": "ambitionbox_search",
                "description": "Search up to 100 AmbitionBox companies with ratings, reviews, employee counts, industry, and job data. For 'top N' requests, set limit=N; results are fetched across multiple 20-company pages. HR/Human Resources/Staffing map to Recruitment, and SaaS maps to Software Product. Supports filters: industry, rating. NOTE: there is no location/city filter — AmbitionBox's gateway ignores it. Do not claim results are scoped to a city.",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "industry": {"type": "string", "description": "Industry filter, e.g. 'Recruitment', 'Human Resources', 'Software Product', 'IT Services & Consulting', 'Banking', 'BPO'"},
                        "rating": {"type": "string", "description": "Minimum rating, e.g. '4.5' for 4.5+ rated companies"},
                        "sort_by": {"type": "string", "enum": ["popular", "rating", "reviews"], "description": "Sort order", "default": "popular"},
                        "page": {"type": "integer", "description": "Page number (1-indexed)", "default": 1},
                        "limit": {"type": "integer", "description": "Total number of companies to return across pages (1-100). Set this to the count requested by the user.", "minimum": 1, "maximum": 100, "default": 10},
                    },
                    "required": [],
                },
            },
        },
        {
            "type": "function",
            "function": {
                "name": "ambitionbox_jobs",
                "description": "Get current job listings for a specific company from AmbitionBox. Includes job titles, skills, experience, locations.",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "company_id": {"type": "integer", "description": "AmbitionBox company ID (get from ambitionbox_search results)"},
                        "page": {"type": "integer", "description": "Page number", "default": 1},
                    },
                    "required": ["company_id"],
                },
            },
        },
        {
            "type": "function",
            "function": {
                "name": "import_ambitionbox_to_workbook",
                "description": "Start a durable, checkpointed import of AmbitionBox company results into a new workbook. Use this after ambitionbox_search when the user asks to add/save/import 'them' or 'those companies'. Repeat the same industry, rating, sort_by, and limit; page retries resume without duplicate rows. Do NOT use create_source_workbook for AmbitionBox results. Requires user approval.",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "industry": {"type": "string", "description": "The same industry used in ambitionbox_search, e.g. 'Human Resources'"},
                        "rating": {"type": "string", "description": "The same minimum rating used in ambitionbox_search, if any"},
                        "sort_by": {"type": "string", "enum": ["popular", "rating", "reviews"], "description": "The same sort order used in ambitionbox_search", "default": "popular"},
                        "limit": {"type": "integer", "description": "Number of matching companies to snapshot (1-100)", "minimum": 1, "maximum": 100, "default": 10},
                        "name": {"type": "string", "description": "Workbook name"},
                    },
                    "required": [],
                },
            },
        },
        {
            "type": "function",
            "function": {
                "name": "create_source_workbook",
                "description": "Create a LIVE-sourcing workbook that finds NEW leads from scratch via the 91-source engine. Use when the user wants to FIND/SOURCE companies (not save a list they already found). Example: 'Build a workbook of IT staffing companies in Pune'. For results already returned by ambitionbox_search, use import_ambitionbox_to_workbook instead. Optionally auto-runs sourcing immediately.",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "icp_description": {"type": "string", "description": "Who to find, incl. geo. e.g. 'IT staffing companies in Pune, 50-500 employees'"},
                        "name": {"type": "string", "description": "Workbook name"},
                        "target_rows": {"type": "integer", "description": "Max rows to source (0 = unlimited)"},
                        "auto_run": {"type": "boolean", "description": "Start sourcing immediately (default true)"},
                        "auto_enrich": {"type": "boolean", "description": "After sourcing, automatically run enrichment/agent columns (default false)"},
                    },
                    "required": ["icp_description"],
                },
            },
        },
        {
            "type": "function",
            "function": {
                "name": "set_workbook_refresh",
                "description": "Make a workbook 'living' — re-source new matches and re-enrich stale data on a schedule. Example: 'refresh this weekly'.",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "workbook_id": {"type": "string"},
                        "interval": {"type": "string", "enum": ["hourly", "daily", "weekly"], "description": "Refresh cadence"},
                    },
                    "required": ["workbook_id", "interval"],
                },
            },
        },
        {
            "type": "function",
            "function": {
                "name": "add_agent_column",
                "description": "Add a goal-directed AGENT column that dynamically picks tools to achieve a goal per row (e.g. 'find the verified CEO email'), with a cost/step budget and reasoning trace.",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "workbook_id": {"type": "string"},
                        "column_name": {"type": "string"},
                        "goal": {"type": "string", "description": "What the agent should find, e.g. 'Find the verified email of the CEO'"},
                        "target_field": {"type": "string", "description": "Field to populate, e.g. 'email'"},
                        "max_cost_usd": {"type": "number", "description": "Per-cell spend cap (default 0.10)"},
                    },
                    "required": ["workbook_id", "column_name", "goal", "target_field"],
                },
            },
        },
        {
            "type": "function",
            "function": {
                "name": "add_signal_trigger",
                "description": "Trigger a workbook to refresh when buying signals fire (hiring, funding, tech change, news). Example: 'when any of these start hiring, refresh and find the hiring manager'.",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "workbook_id": {"type": "string"},
                        "signals": {"type": "array", "items": {"type": "string", "enum": ["hiring", "funding", "tech_change", "news"]}, "description": "Signal types that trigger a refresh"},
                    },
                    "required": ["workbook_id", "signals"],
                },
            },
        },
        {
            "type": "function",
            "function": {
                "name": "draft_plan",
                "description": "For COMPOUND goals that need several steps (e.g. 'build a list of 50 IT staffing firms in Pune and find their founders' emails'), draft a step-by-step plan WITHOUT executing it. Returns the plan for the user to review. Do not use for single actions.",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "goal": {"type": "string", "description": "The high-level goal in the user's words"},
                        "target_count": {"type": "integer", "description": "How many companies/leads, if specified"},
                    },
                    "required": ["goal"],
                },
            },
        },
        {
            "type": "function",
            "function": {
                "name": "execute_plan",
                "description": "Execute a plan previously produced by draft_plan. Pass the plan_id and nonce returned by draft_plan (the server executes the plan it stored under that plan_id). This builds the workbook, sources companies, and runs agent-column enrichment. Requires user approval.",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "plan_id": {"type": "string", "description": "The plan_id returned by draft_plan"},
                        "nonce": {"type": "string", "description": "The single-use nonce returned by draft_plan"},
                        "plan": {"type": "object", "description": "(Optional, for display only) the plan object returned by draft_plan; the server executes the stored plan by plan_id, not this body"},
                    },
                    "required": ["plan_id", "nonce"],
                },
            },
        },
    ]


async def _execute_tool(name: str, args: dict, *, store, workspace_id: str, slug: str,
                        user_id: Optional[int] = None) -> str:
    """Execute a backend tool and return the result as a string.

    Tenant-scoped: ``store`` is the request's workspace-scoped lead store
    (``PgLeadStore`` on cloud, per-workspace ``LeadDB`` on self-host) — there is
    NO bare ``LeadDB()`` here, so every lead/signal read/write is filtered and
    stamped by ``workspace_id`` (RLS is the DB backstop on PG). Workbook ORM
    tools (which have no RLS backstop) explicitly filter + stamp ``workspace_id``.
    The caller runs this inside ``workspace_scope(workspace_id)`` so PG sessions
    get the RLS GUC; ``store`` is shared across a turn, so we never close it here.
    """
    db = store
    try:
        if name in DANGEROUS_TOOLS and user_id is not None:
            role = ws_manager.member_role(workspace_id, user_id)
            if role not in ("owner", "admin", "editor"):
                return json.dumps({"error": "Insufficient workspace role"})
        if name == "search_leads":
            leads = db.get_leads(
                search=args.get("query"),
                city=args.get("city"),
                status=args.get("status"),
                score_tier=args.get("score_tier"),
                limit=args.get("limit", 10),
            )
            result = [
                {
                    "id": l.id, "company": l.company, "city": l.city,
                    "email": l.email, "phone": l.phone, "score": l.score,
                    "tier": l.score_tier, "status": l.status,
                    "website": l.website, "specialization": l.specialization,
                    "company_size": l.company_size, "contact_person": l.contact_person,
                    "linkedin": l.linkedin_url, "industry_tags": l.industry_tags,
                    "founded_year": l.founded_year,
                }
                for l in leads
            ]
            return json.dumps({"leads": result, "count": len(result)})

        elif name == "get_lead_detail":
            lead = db.get_lead(args["lead_id"])
            if not lead:
                return json.dumps({"error": "Lead not found"})
            d = lead.to_dict()
            # Parse decision makers JSON if present
            if d.get("decision_makers"):
                try:
                    d["decision_makers"] = json.loads(d["decision_makers"])
                except (json.JSONDecodeError, TypeError):
                    pass
            return json.dumps({"lead": d})

        elif name == "get_lead_stats":
            stats = db.get_stats()
            # default=str so any backend-specific scalar (e.g. PG Decimal) is
            # serializable rather than blowing up the whole turn.
            return json.dumps(stats, default=str)

        elif name == "update_lead_status":
            db.update_status(args["lead_id"], args["status"], args.get("note", ""))
            return json.dumps({"ok": True, "lead_id": args["lead_id"], "new_status": args["status"]})

        elif name == "start_collection":
            from apps.api.services.leadgen.db import LeadDB as _LeadDB
            from apps.api.services.workspace.manager import workspace_leads_db_path
            from apps.api.services.queue_service import queue_service
            from apps.api.services.leadgen.progress import progress
            from apps.api.database import SessionLocal
            job_id = uuid.uuid4().hex
            query = args["query"]
            ws_id = workspace_id
            if not ws_id or not slug:
                return json.dumps({"error": "Workspace context is required"})

            # Create + stamp the job row NOW (request thread, own connection) so a
            # client can poll /api/jobs/{id} immediately. Job/stage bookkeeping
            # remains a per-workspace SQLite ledger even when leads live in the
            # shared RLS-protected Postgres store.
            _jobdb = _LeadDB(workspace_leads_db_path(slug))
            _jobdb.create_job(job_id, query)
            _jobdb.conn.execute(
                "UPDATE jobs SET workspace_id = ? WHERE id = ?", (ws_id, job_id)
            )
            _jobdb.conn.commit()
            _jobdb.close()

            try:
                with SessionLocal() as qdb:
                    queued = queue_service.add_job(
                        qdb,
                        "collect",
                        {
                            "job_id": job_id,
                            "query": query,
                            "workspace_id": ws_id,
                            "slug": slug,
                        },
                        fire_key=f"collect:{ws_id}:{job_id}",
                    )
            except Exception as exc:
                _jobdb = _LeadDB(workspace_leads_db_path(slug))
                _jobdb.conn.execute(
                    "UPDATE jobs SET status = 'failed', error = ?, completed_at = ? WHERE id = ?",
                    (f"Queue enqueue failed: {exc}", datetime.now(timezone.utc).isoformat(), job_id),
                )
                _jobdb.conn.commit()
                _jobdb.close()
                return json.dumps({"error": "Collection queue unavailable"})

            progress.bind_job(job_id, ws_id)
            progress.emit(
                "job_created",
                {
                    "job_id": job_id,
                    "query": query,
                    "workspace_id": ws_id,
                    "message": f"Collection queued: {query}",
                },
            )
            return json.dumps({"ok": True, "job_id": job_id, "query": query,
                               "queue_job_id": queued.id,
                               "message": "Collection started with 6 strategies: Maps, Web, Directories, LinkedIn, Job Boards, Review Sites"})

        elif name == "enrich_lead":
            lead = db.get_lead(args["lead_id"])
            if not lead:
                return json.dumps({"error": "Lead not found"})

            enriched_fields = []

            # Website enrichment.
            # NOTE: _execute_tool runs ON the request's event loop. The previous
            # code spun up a NEW event loop and ran it synchronously
            # (loop.run_until_complete), which blocks the *entire* server loop —
            # freezing all requests — for the scrape duration. Awaiting the
            # coroutine directly (with a timeout guard so a slow site can't hang
            # the turn) is correct and only suspends this turn.
            if lead.has_website and (not lead.has_email or not lead.has_phone):
                try:
                    from apps.api.services.leadgen.enrichment.website_scraper import _scrape_via_http
                    from apps.api.services.leadgen.http import StealthClient

                    client = StealthClient()
                    url = lead.website if lead.website.startswith("http") else f"https://{lead.website}"

                    result = await asyncio.wait_for(_scrape_via_http(client, url), timeout=30.0)

                    if result.get("emails") and not lead.has_email:
                        lead.email = result["emails"][0]
                        enriched_fields.append("email")
                    if result.get("phones") and not lead.has_phone:
                        lead.phone = result["phones"][0]
                        enriched_fields.append("phone")
                    if result.get("social", {}).get("linkedin") and not lead.has_linkedin:
                        lead.linkedin_url = result["social"]["linkedin"]
                        enriched_fields.append("linkedin")
                    if result.get("description") and not lead.description:
                        lead.description = result["description"]
                        enriched_fields.append("description")
                except Exception as e:
                    enriched_fields.append(f"website_error: {e}")

            # Save enriched data
            if enriched_fields:
                db.update_lead_fields(lead.id, {
                    "email": lead.email, "phone": lead.phone,
                    "linkedin_url": lead.linkedin_url, "description": lead.description,
                    "last_enriched_at": datetime.now(timezone.utc).isoformat(),
                })

            return json.dumps({
                "ok": True, "lead_id": lead.id, "company": lead.company,
                "enriched_fields": enriched_fields,
                "email": lead.email, "phone": lead.phone,
                "linkedin": lead.linkedin_url,
            })

        elif name == "find_similar_leads":
            lead = db.get_lead(args["lead_id"])
            if not lead:
                return json.dumps({"error": "Lead not found"})

            similar = []
            if lead.specialization:
                similar = db.get_leads(search=lead.specialization, limit=args.get("limit", 10) + 1)
            if not similar and lead.city:
                similar = db.get_leads(city=lead.city, limit=args.get("limit", 10) + 1)

            similar = [l for l in similar if l.id != lead.id]

            result = [
                {
                    "id": l.id, "company": l.company, "city": l.city,
                    "score": l.score, "tier": l.score_tier,
                    "specialization": l.specialization, "company_size": l.company_size,
                    "email": l.email, "phone": l.phone,
                }
                for l in similar[:args.get("limit", 10)]
            ]
            return json.dumps({
                "reference": lead.company,
                "similar_leads": result, "count": len(result),
            })

        elif name == "get_enrichment_gaps":
            missing = args.get("missing_field", "any")
            min_score = args.get("min_score", 30)
            limit = args.get("limit", 15)

            all_leads = db.get_leads(limit=500)

            gaps = []
            for l in all_leads:
                if l.score < min_score:
                    continue
                is_gap = False
                if missing == "email" and not l.has_email:
                    is_gap = True
                elif missing == "phone" and not l.has_phone:
                    is_gap = True
                elif missing == "linkedin" and not l.has_linkedin:
                    is_gap = True
                elif missing == "decision_makers" and not l.decision_makers:
                    is_gap = True
                elif missing == "any" and (not l.has_email or not l.has_phone or not l.has_linkedin):
                    is_gap = True

                if is_gap:
                    gaps.append({
                        "id": l.id, "company": l.company, "city": l.city,
                        "score": l.score, "tier": l.score_tier,
                        "has_email": l.has_email, "has_phone": l.has_phone,
                        "has_linkedin": l.has_linkedin, "website": l.website,
                    })
                if len(gaps) >= limit:
                    break

            return json.dumps({
                "gaps": gaps, "count": len(gaps),
                "missing_field": missing,
                "message": f"Found {len(gaps)} leads (score>={min_score}) missing {missing}",
            })

        elif name == "suggest_outreach":
            lead = db.get_lead(args["lead_id"])
            if not lead:
                return json.dumps({"error": "Lead not found"})

            channel = args.get("channel", "email")
            try:
                from apps.api.services.leadgen.config import ICP
                value_prop = ICP.get("value_proposition", "our B2B SaaS solution")
            except Exception:
                value_prop = "our B2B SaaS solution"

            contact = lead.contact_person or "there"
            company = lead.company
            spec = lead.specialization or "your industry"

            if channel == "email":
                message = {
                    "subject": f"Streamline {company}'s {spec} operations",
                    "body": (
                        f"Hi {contact},\n\n"
                        f"I noticed {company} specializes in {spec}"
                        f"{' in ' + lead.city if lead.city else ''}. "
                        f"Companies like yours often struggle with fragmented candidate tracking, "
                        f"attendance management, and client reporting.\n\n"
                        f"We built {value_prop} — specifically for {spec} firms. "
                        f"It consolidates everything into one unified console.\n\n"
                        f"Would you be open to a quick 15-min walkthrough this week?\n\n"
                        f"Best regards"
                    ),
                    "channel": "email",
                    "to": lead.email or "(email not found — enrich this lead first)",
                }
            else:
                message = {
                    "body": (
                        f"Hi {contact}, I came across {company}'s profile and was impressed "
                        f"by your work in {spec}. "
                        f"We've been helping similar {spec} companies streamline operations "
                        f"with {value_prop}. "
                        f"Would love to connect and share how we've helped firms like yours. "
                        f"Open to a quick chat?"
                    ),
                    "channel": "linkedin",
                    "profile": lead.linkedin_url or "(LinkedIn not found — enrich first)",
                }

            return json.dumps({"outreach": message, "lead": lead.company})

        elif name == "compare_leads":
            lead_ids = args.get("lead_ids", [])[:3]
            leads_data = []
            for lid in lead_ids:
                lead = db.get_lead(lid)
                if lead:
                    leads_data.append({
                        "id": lead.id, "company": lead.company, "city": lead.city,
                        "score": lead.score, "tier": lead.score_tier,
                        "specialization": lead.specialization,
                        "company_size": lead.company_size,
                        "has_email": lead.has_email, "has_phone": lead.has_phone,
                        "has_linkedin": lead.has_linkedin,
                        "has_contact": lead.has_contact_person,
                        "founded_year": lead.founded_year,
                        "revenue_range": lead.revenue_range,
                        "data_completeness": round(sum([
                            lead.has_email, lead.has_phone, lead.has_linkedin,
                            lead.has_contact_person, bool(lead.website),
                            bool(lead.description), bool(lead.company_size),
                        ]) / 7 * 100),
                    })

            return json.dumps({"comparison": leads_data, "count": len(leads_data)})

        elif name == "ambitionbox_search":
            from apps.api.services.leadgen.ambitionbox import ambitionbox

            industry = [args["industry"]] if args.get("industry") else None
            requested_limit = max(1, min(int(args.get("limit", 10)), 100))
            # No location: search_companies raises on it, because the gateway
            # silently drops the filter and returns unscoped results.
            if requested_limit > 20:
                max_pages = min(25, (requested_limit + 19) // 20 + 5)
                collection = await ambitionbox.collect_companies(
                    requested_count=requested_limit,
                    max_pages=max_pages,
                    industry=industry,
                    sort_by=args.get("sort_by", "popular"),
                    rating=args.get("rating"),
                )
                result = collection.summary(include_records=False)
                result["companies"] = [
                    record.as_dict() for record in collection.records
                ]
                result["total"] = len(collection.records)
                result["page"] = 1
                result["requested_limit"] = requested_limit
            else:
                result = await ambitionbox.search_companies(
                    page=args.get("page", 1),
                    limit=requested_limit,
                    sort_by=args.get("sort_by", "popular"),
                    industry=industry,
                    rating=args.get("rating"),
                )

            return json.dumps(result)

        elif name == "ambitionbox_jobs":
            from apps.api.services.leadgen.ambitionbox import ambitionbox
            result = await ambitionbox.get_company_jobs(
                        company_id=args["company_id"],
                        page=args.get("page", 1),
                    )

            return json.dumps(result)

        elif name == "import_ambitionbox_to_workbook":
            from apps.api.services.workbook.ambitionbox_import import (
                start_ambitionbox_import,
            )

            requested_limit = max(1, min(int(args.get("limit", 10)), 100))
            industry_name = args.get("industry")
            sort_by = args.get("sort_by", "popular")
            rating = args.get("rating")
            workbook_name = args.get("name") or (
                f"AmbitionBox — {industry_name} Companies"
                if industry_name else "AmbitionBox Companies"
            )
            result = start_ambitionbox_import(
                workspace_id=workspace_id,
                name=workbook_name,
                industry=industry_name,
                rating=rating,
                sort_by=sort_by,
                requested_limit=requested_limit,
            )
            result["message"] = (
                f"Started a durable import of {requested_limit} AmbitionBox "
                f"companies into '{result['name']}'. Progress is checkpointed "
                f"under run {result['run_id']}. Open at {result['url']}"
            )
            return json.dumps(result)

        # ── P5: chat authors the source engine (ORM + P0–P4 services) ──
        elif name == "create_source_workbook":
            from apps.api.database import SessionLocal
            from apps.api.services.workbook.models import Workbook
            from apps.api.services.queue_service import queue_service
            icp_desc = args["icp_description"]
            wb_name = args.get("name") or f"Source — {icp_desc[:40]}"
            target_rows = int(args.get("target_rows", 0) or 0)
            auto_run = args.get("auto_run", True)
            auto_enrich = bool(args.get("auto_enrich", False))
            src_col = {
                "id": f"src_{uuid.uuid4().hex[:8]}", "name": "Source", "type": "source",
                "icp": {"description": icp_desc}, "channels": {}, "target_rows": target_rows,
            }
            base_cols = [
                {"id": "company", "name": "Company", "type": "lead_field", "lead_field": "company"},
                {"id": "website", "name": "Website", "type": "lead_field", "lead_field": "website"},
                {"id": "city", "name": "City", "type": "lead_field", "lead_field": "city"},
                src_col,
            ]
            with SessionLocal() as wdb:
                # STAMP workspace_id on the row AND in source_config so the
                # downstream source engine sources into the right tenant. This
                # runs inside workspace_scope, so the workbooks RLS policy +
                # WITH CHECK (migration e5f6a7b8c9d0) back this app-layer stamp.
                wb = Workbook(name=wb_name, description=icp_desc, status="draft",
                              source_type="empty", columns_config=base_cols,
                              workspace_id=workspace_id,
                              source_config={"workspace_id": workspace_id})
                wdb.add(wb); wdb.commit(); wdb.refresh(wb)
                wb_id = wb.id
                if auto_run:
                    # OD-4: stamp the tenant into the payload so the source worker
                    # enters workspace_scope (never reads the row to learn its ws).
                    queue_service.add_job(wdb, "source_workbook",
                                          {"workbook_id": wb_id, "column_id": src_col["id"],
                                           "enrich_after": auto_enrich,
                                           "workspace_id": workspace_id})
            return json.dumps({
                "workbook_id": wb_id, "name": wb_name, "sourcing": bool(auto_run),
                "message": f"Created live-sourcing workbook '{wb_name}'."
                           + (" Sourcing started — rows will stream in." if auto_run else "")
                           + f" Open at /workbooks/{wb_id}",
            })

        elif name == "set_workbook_refresh":
            from apps.api.database import SessionLocal
            from apps.api.services.workbook.models import Workbook
            from apps.api.services.workbook.refresh import set_refresh_policy
            with SessionLocal() as wdb:
                # FILTER by workspace_id (no RLS on workbooks) — a miss is "not
                # found", never an existence leak / cross-tenant mutation.
                wb = wdb.query(Workbook).filter(
                    Workbook.id == args["workbook_id"],
                    Workbook.workspace_id == workspace_id,
                ).first()
                if not wb:
                    return json.dumps({"error": "Workbook not found"})
                res = set_refresh_policy(wdb, args["workbook_id"],
                                         {"enabled": True, "interval": args["interval"]})
            if "error" in res:
                return json.dumps(res)
            return json.dumps({"message": f"Workbook will refresh {args['interval']}.", **res})

        elif name == "add_agent_column":
            from apps.api.database import SessionLocal
            from apps.api.services.workbook.models import Workbook
            with SessionLocal() as wdb:
                wb = wdb.query(Workbook).filter(
                    Workbook.id == args["workbook_id"],
                    Workbook.workspace_id == workspace_id,
                ).first()
                if not wb:
                    return json.dumps({"error": "Workbook not found"})
                col = {
                    "id": f"agent_{uuid.uuid4().hex[:8]}", "name": args["column_name"],
                    "type": "agent", "goal": args["goal"], "target_field": args["target_field"],
                    "policy": {"max_steps": 6, "max_cost_usd": float(args.get("max_cost_usd", 0.10))},
                }
                cfg = list(wb.columns_config or []); cfg.append(col)
                wb.columns_config = cfg; wdb.commit()
            return json.dumps({"added": args["column_name"], "type": "agent",
                               "message": f"Added agent column '{args['column_name']}' (goal: {args['goal']})."})

        elif name == "add_signal_trigger":
            from apps.api.database import SessionLocal
            from apps.api.services.workbook.models import Workbook
            from apps.api.services.workbook.refresh import set_refresh_policy
            with SessionLocal() as wdb:
                wb = wdb.query(Workbook).filter(
                    Workbook.id == args["workbook_id"],
                    Workbook.workspace_id == workspace_id,
                ).first()
                if not wb:
                    return json.dumps({"error": "Workbook not found"})
                policy = dict(wb.refresh_policy or {})
                policy["enabled"] = True
                policy["on_signal"] = args["signals"]
                res = set_refresh_policy(wdb, args["workbook_id"], policy)
            return json.dumps({"message": f"Workbook will refresh on signals: {', '.join(args['signals'])}.", **res})

        # ── Autopilot: goal → plan → execute (orchestrates the tools above) ──
        elif name == "draft_plan":
            from apps.api.services.agent import autopilot, autopilot_plan_store
            ws_id = workspace_id
            plan = await autopilot.draft_plan(
                args["goal"], int(args.get("target_count", 0) or 0), workspace_id=ws_id,
            )
            # Persist server-side at draft time → the gate executes EXACTLY this
            # plan (approval-integrity). Client echoes plan_id + nonce on approve.
            plan_id, nonce = autopilot_plan_store.put(ws_id, user_id, plan)
            return json.dumps({
                "plan": plan,
                "plan_id": plan_id,
                "nonce": nonce,
                "message": "Drafted a plan. Call execute_plan with {plan_id, nonce} to run it "
                           "(the user will be asked to approve).",
            })

        elif name == "execute_plan":
            # Reached via the gate replay (_resolve_approved_calls), which
            # resolves the server-stored plan by (workspace_id, user_id, plan_id,
            # nonce) and re-validates. A call WITHOUT a valid nonce is rejected —
            # we never execute a client-supplied plan body.
            from apps.api.services.agent import autopilot, autopilot_plan_store
            ws_id = workspace_id
            plan_id = args.get("plan_id")
            nonce = args.get("nonce")
            stored = autopilot_plan_store.consume(ws_id, user_id, plan_id, nonce) if plan_id else None
            if not stored:
                return json.dumps({"error": "Plan not found or already executed. "
                                            "Re-draft and approve the plan."})
            plan = autopilot._validate_plan(stored) or stored
            # Bind the tenant-scoped tools so Autopilot's recursion into
            # _execute_tool inherits this workspace's store/scope (no cross-tenant
            # reach even with the LLM planner on).
            bound_execute_tool = functools.partial(
                _execute_tool,
                store=store,
                workspace_id=workspace_id,
                slug=slug,
                user_id=user_id,
            )
            result = await autopilot.execute_plan(plan, bound_execute_tool)
            # Best-effort memory write (idempotent on (ws, workbook_id)).
            try:
                from apps.api.services.agent import autopilot_memory
                autopilot_memory.record(
                    ws_id, plan.get("goal", ""), plan,
                    result.get("workbook_id"), "ok" if result.get("ok") else "failed",
                )
            except Exception:
                pass
            return json.dumps(result)

        return json.dumps({"error": f"Unknown tool: {name}"})
    except Exception as e:
        return json.dumps({"error": f"Tool '{name}' failed: {str(e)[:300]}"})


# ── Chat completion proxy ────────────────────────────────────────

async def _stream_chat(
    messages: list,
    tools: list,
    provider: dict,
    fallback_providers: list = None,
    round_idx: int = 0,
    max_rounds: int = None,
    seen_calls: dict = None,
    *,
    store=None,
    workspace_id: str = "",
    slug: str = "",
    user_id: Optional[int] = None,
) -> AsyncGenerator[str, None]:
    """Stream chat completion from the configured AI provider.

    On 429/rate-limit errors, automatically fails over to the next provider
    in fallback_providers (failover does NOT consume the tool-round budget).

    The agentic tool loop is bounded by `max_rounds`: each round of tool calls
    increments `round_idx`, and on the final round the model is re-issued with
    no tools so it cannot loop again. `seen_calls` dedupes identical tool calls
    within a turn to break no-progress cycles.
    """
    if max_rounds is None:
        max_rounds = _max_tool_rounds()
    if seen_calls is None:
        seen_calls = {}
    api_key = provider["api_key"]
    base_url = provider["base_url"].rstrip("/")
    model = provider["model"]
    provider_name = provider.get("name", provider.get("id", "unknown"))

    if not api_key:
        yield f'data: {json.dumps({"error": "No AI provider configured. Go to Settings to add an API key."})}\n\n'
        return

    headers = {
        "Authorization": f"Bearer {api_key}",
        "Content-Type": "application/json",
    }

    body = {
        "model": model,
        "messages": messages,
        "tools": tools,
        "stream": True,
        "temperature": 0.7,
        # Generative-UI responses embed lead data as OpenUI Lang and can be long;
        # give the model room so the DSL isn't cut off mid-structure (a truncated
        # response can't be parsed and renders as nothing).
        "max_tokens": 4096,
    }

    url = f"{base_url}/chat/completions"

    try:
        async with httpx.AsyncClient(timeout=90.0) as client:
            async with client.stream("POST", url, json=body, headers=headers) as response:
                # ── Handle rate limits with auto-failover ────────
                if response.status_code == 429:
                    error_body = await response.aread()
                    error_text = error_body.decode()[:200]

                    # Try fallback providers
                    if fallback_providers:
                        next_prov = fallback_providers[0]
                        remaining = fallback_providers[1:]
                        next_name = next_prov.get("name", next_prov.get("id", "?"))

                        msg = f"⚡ {provider_name} rate limited — switching to {next_name}..."
                        yield f'data: {json.dumps({"warning": msg})}\n\n'

                        async for chunk in _stream_chat(messages, tools, next_prov, remaining,
                                                        round_idx, max_rounds, seen_calls,
                                                        store=store, workspace_id=workspace_id, slug=slug, user_id=user_id):
                            yield chunk
                        return

                    # No fallbacks left
                    yield f'data: {json.dumps({"error": f"Rate limited by {provider_name} and no fallback providers available. Add more API keys in Settings, or wait and retry. ({error_text})"})}\n\n'
                    return

                if response.status_code != 200:
                    error_body = await response.aread()
                    error_text = error_body.decode()[:200]

                    # Also try failover on 5xx server errors
                    if response.status_code >= 500 and fallback_providers:
                        next_prov = fallback_providers[0]
                        remaining = fallback_providers[1:]
                        next_name = next_prov.get("name", next_prov.get("id", "?"))

                        msg = f"⚡ {provider_name} error — switching to {next_name}..."
                        yield f'data: {json.dumps({"warning": msg})}\n\n'

                        async for chunk in _stream_chat(messages, tools, next_prov, remaining,
                                                        round_idx, max_rounds, seen_calls,
                                                        store=store, workspace_id=workspace_id, slug=slug, user_id=user_id):
                            yield chunk
                        return

                    yield f'data: {json.dumps({"error": f"Provider error {response.status_code}: {error_text}"})}\n\n'
                    return

                accumulated_tool_calls = {}

                async for line in response.aiter_lines():
                    if not line.startswith("data: "):
                        continue
                    data = line[6:]
                    if data == "[DONE]":
                        yield "data: [DONE]\n\n"
                        return

                    try:
                        chunk = json.loads(data)
                        delta = chunk.get("choices", [{}])[0].get("delta", {})

                        # Handle tool calls
                        if "tool_calls" in delta:
                            for tc in delta["tool_calls"]:
                                idx = tc.get("index", 0)
                                if idx not in accumulated_tool_calls:
                                    accumulated_tool_calls[idx] = {
                                        "id": tc.get("id", ""),
                                        "type": "function",
                                        "function": {"name": "", "arguments": ""},
                                    }
                                if "id" in tc and tc["id"]:
                                    accumulated_tool_calls[idx]["id"] = tc["id"]
                                func = tc.get("function", {})
                                if "name" in func:
                                    accumulated_tool_calls[idx]["function"]["name"] += func["name"]
                                if "arguments" in func:
                                    accumulated_tool_calls[idx]["function"]["arguments"] += func["arguments"]

                        # Forward content chunks to client
                        if delta.get("content"):
                            yield f"data: {json.dumps({'content': delta['content']})}\n\n"

                        # Detect finish_reason for tool calls
                        finish = chunk.get("choices", [{}])[0].get("finish_reason")
                        if finish == "tool_calls" and accumulated_tool_calls:
                            calls = [accumulated_tool_calls[i] for i in sorted(accumulated_tool_calls.keys())]

                            # Parse args once; partition into safe (run inline) and
                            # dangerous (gate — do NOT execute, end the turn for approval).
                            parsed = []          # (tc, fn_name, fn_args)
                            dangerous = []       # subset needing confirmation
                            for tc in calls:
                                fn_name = tc["function"]["name"]
                                if not tc["function"]["arguments"]:
                                    tc["function"]["arguments"] = "{}"
                                try:
                                    fn_args = json.loads(tc["function"]["arguments"])
                                except json.JSONDecodeError:
                                    fn_args = {}
                                parsed.append((tc, fn_name, fn_args))
                                if _needs_confirmation(fn_name):
                                    dangerous.append((tc, fn_name, fn_args))

                            # Execute SAFE (read-only) calls inline, with per-turn dedup.
                            tool_results = []
                            for tc, fn_name, fn_args in parsed:
                                if _needs_confirmation(fn_name):
                                    continue  # gated below — never executed here
                                sig = fn_name + "|" + json.dumps(fn_args, sort_keys=True, default=str)
                                yield f"data: {json.dumps({'tool_call': {'name': fn_name, 'args': fn_args}})}\n\n"
                                if sig in seen_calls:
                                    result = seen_calls[sig]  # no-progress guard: reuse prior result
                                else:
                                    # Run the tool inside the request's tenant scope so PG
                                    # txns (PgLeadStore + workbook SessionLocal) get the RLS GUC.
                                    with workspace_scope(workspace_id):
                                        result = await _execute_tool(
                                            fn_name, fn_args, store=store,
                                            workspace_id=workspace_id, slug=slug,
                                            user_id=user_id,
                                        )
                                    seen_calls[sig] = result
                                yield f"data: {json.dumps({'tool_result': {'name': fn_name, 'result': json.loads(result)}})}\n\n"
                                tool_results.append({
                                    "role": "tool", "tool_call_id": tc["id"],
                                    "name": fn_name, "content": result,
                                })

                            # ── Real human-in-the-loop gate ──
                            # If any dangerous call is proposed, do NOT execute it.
                            # Emit a confirmation event per call and END the turn; the
                            # frontend resubmits with approved_tool_calls to execute.
                            if dangerous:
                                for tc, fn_name, fn_args in dangerous:
                                    meta = DANGEROUS_TOOLS.get(fn_name, {})
                                    approval_id = chat_history.create_tool_approval(
                                        workspace_id, user_id, tc
                                    )
                                    display_call = json.loads(json.dumps(tc))
                                    display_call["id"] = approval_id
                                    yield f"data: {json.dumps({'confirmation_required': {'confirmation_id': approval_id, 'tool_call': display_call, 'name': fn_name, 'args': fn_args, 'description': _describe_action(fn_name, fn_args, workspace_id), 'level': meta.get('level', 'high'), 'label': meta.get('label', 'Unrecognized write tool')}})}\n\n"
                                yield f"data: {json.dumps({'awaiting_confirmation': True})}\n\n"
                                yield "data: [DONE]\n\n"
                                return

                            # No dangerous calls — continue the loop under the round budget.
                            follow_up = messages + [
                                {"role": "assistant", "content": "", "tool_calls": calls},
                                *tool_results,
                            ]
                            next_round = round_idx + 1
                            if next_round >= max_rounds:
                                # Hard stop: re-issue with NO tools so the model must
                                # answer from what it has and cannot loop again.
                                follow_up.append({
                                    "role": "system",
                                    "content": (
                                        f"You have reached the maximum of {max_rounds} tool-use rounds. "
                                        "Do NOT call any more tools. Summarize what you found and the "
                                        "next step the user can take."
                                    ),
                                })
                                async for chunk_line in _stream_chat(
                                    follow_up, [], provider, fallback_providers,
                                    next_round, max_rounds, seen_calls,
                                    store=store, workspace_id=workspace_id, slug=slug, user_id=user_id):
                                    yield chunk_line
                                return
                            async for chunk_line in _stream_chat(
                                follow_up, tools, provider, fallback_providers,
                                next_round, max_rounds, seen_calls,
                                    store=store, workspace_id=workspace_id, slug=slug, user_id=user_id):
                                yield chunk_line
                            return

                    except json.JSONDecodeError:
                        continue

    except (httpx.ConnectError, httpx.TimeoutException) as e:
        # Connection/timeout errors — try failover before giving up
        if fallback_providers:
            next_prov = fallback_providers[0]
            remaining = fallback_providers[1:]
            next_name = next_prov.get("name", next_prov.get("id", "?"))
            error_type = "unreachable" if isinstance(e, httpx.ConnectError) else "timed out"

            nl = "\n"
            msg = f"{nl}{nl}> ⚡ *{provider_name} {error_type} — switching to {next_name}...*{nl}{nl}"
            yield f'data: {json.dumps({"content": msg})}\n\n'

            async for chunk in _stream_chat(messages, tools, next_prov, remaining,
                                            round_idx, max_rounds, seen_calls,
                                                        store=store, workspace_id=workspace_id, slug=slug, user_id=user_id):
                yield chunk
            return

        if isinstance(e, httpx.ConnectError):
            yield f'data: {json.dumps({"error": "Cannot reach AI provider. Check your API key and network connection in Settings."})}\n\n'
        else:
            yield f'data: {json.dumps({"error": "AI provider timed out. The service may be overloaded — try again in a moment."})}\n\n'
    except Exception as e:
        yield f'data: {json.dumps({"error": f"AI provider error: {str(e)[:200]}"})}\n\n'


async def _resolve_approved_calls(
    cleaned_messages: list, approved: list, *, store=None, workspace_id: str = "", slug: str = "",
    user_id: Optional[int] = None,
) -> AsyncGenerator[str, None]:
    """Execute user-approved (or denied) dangerous tool calls before resuming.

    Part of the human-in-the-loop gate: when the previous turn ended awaiting
    confirmation, the frontend resubmits with `approved_tool_calls`. We replay
    the assistant tool_calls message the model proposed (synthesized from the
    echoed tool_call so OpenAI message ordering stays valid), then for each call
    either execute it (approve) or record a denial (deny), appending tool-role
    messages to `cleaned_messages`. Yields SSE lines for tool_call/tool_result so
    the client sees the action happen.
    """
    if not approved:
        return

    resolved = []
    for item in approved:
        echoed = item.get("tool_call") or {}
        approval_id = str(echoed.get("id") or "")
        decision = item.get("decision", "approve")
        stored_call = chat_history.consume_tool_approval(
            approval_id, workspace_id, user_id, decision
        )
        resolved.append((decision, stored_call, approval_id))

    if not resolved:
        return
    tool_calls = [
        stored_call or {
            "id": approval_id or f"invalid-{idx}",
            "type": "function",
            "function": {"name": "invalid_approval", "arguments": "{}"},
        }
        for idx, (_decision, stored_call, approval_id) in enumerate(resolved)
    ]
    cleaned_messages.append({"role": "assistant", "content": "", "tool_calls": tool_calls})

    for decision, tc, approval_id in resolved:
        if tc is None:
            content = json.dumps({
                "denied": True,
                "reason": "Approval is invalid, expired, already consumed, or belongs to another workspace.",
            })
            yield f"data: {json.dumps({'tool_denied': {'name': 'invalid_approval'}})}\n\n"
            cleaned_messages.append({
                "role": "tool",
                "tool_call_id": approval_id or "invalid-approval",
                "name": "invalid_approval",
                "content": content,
            })
            continue
        fn_name = tc["function"]["name"]
        try:
            fn_args = json.loads(tc["function"].get("arguments") or "{}")
        except json.JSONDecodeError:
            fn_args = {}

        if decision == "approve" and fn_name in DANGEROUS_TOOLS:
            yield f"data: {json.dumps({'tool_call': {'name': fn_name, 'args': fn_args}})}\n\n"
            with workspace_scope(workspace_id):
                result = await _execute_tool(
                    fn_name, fn_args, store=store, workspace_id=workspace_id, slug=slug,
                    user_id=user_id,
                )
            try:
                parsed_result = json.loads(result)
            except json.JSONDecodeError:
                parsed_result = {"raw": result}
            yield f"data: {json.dumps({'tool_result': {'name': fn_name, 'result': parsed_result}})}\n\n"
            content = result
        else:
            reason = (
                "User declined this action."
                if decision == "deny"
                else "Tool is not an explicitly classified mutation."
            )
            content = json.dumps({"denied": True, "reason": reason})
            yield f"data: {json.dumps({'tool_denied': {'name': fn_name}})}\n\n"

        cleaned_messages.append({
            "role": "tool", "tool_call_id": tc.get("id", ""),
            "name": fn_name, "content": content,
        })


# ── Main chat endpoint ───────────────────────────────────────────

@router.post("")
async def copilot_chat(request: Request):
    """Main CopilotKit chat endpoint — streams AI responses with tool use.

    Accepts:
        messages: list of {role, content}
        conversation_id: optional, to continue an existing conversation
        context: optional application context
    """
    # Resolve + authorize the tenant FIRST (before any side effects). In cloud
    # this fails closed (401/403) — no conversation is created and no tool runs
    # for an unauthenticated / non-member request. Self-host binds to `main`.
    workspace_id, _chat_user_id, slug = _resolve_chat_workspace(request)
    # One tenant-scoped store for the whole turn (PgLeadStore on cloud, the
    # per-workspace LeadDB on self-host). Threaded into every tool call.
    store = get_lead_store(workspace_id, slug)

    body = await request.json()

    user_messages = body.get("messages", [])
    context = body.get("context", "")
    conv_id = body.get("conversation_id")
    # Pre-approved (or denied) dangerous tool calls from a confirmation resubmit.
    approved_tool_calls = body.get("approved_tool_calls", []) or []

    # Get the last user message for memory operations
    last_user_msg = ""
    for m in reversed(user_messages):
        if m.get("role") == "user":
            last_user_msg = m.get("content", "")
            break

    # Create or continue conversation — both scoped to (workspace, user). A
    # body-supplied conversation_id that isn't owned by this tenant is treated
    # as missing and a fresh conversation is started, so a forged id can never
    # append to or read another tenant's chat.
    if conv_id and not chat_history.get_conversation(conv_id, workspace_id, _chat_user_id):
        conv_id = None
    if not conv_id:
        title = chat_history.auto_title_from_message(last_user_msg)
        conv = chat_history.create_conversation(workspace_id, _chat_user_id, title=title)
        conv_id = conv["id"]

    # Store user message in history
    if last_user_msg:
        chat_history.add_message(conv_id, "user", last_user_msg)

    # Search memory for relevant context
    memory_context = ""
    if last_user_msg and memory.is_available():
        memories = memory.search_memory(last_user_msg, workspace_id, user_id=_chat_user_id, limit=5)
        if memories:
            memory_texts = []
            for m in memories:
                if isinstance(m, dict):
                    memory_texts.append(m.get("memory", m.get("text", str(m))))
                else:
                    memory_texts.append(str(m))
            if memory_texts:
                memory_context = "Relevant memories from past conversations:\n" + "\n".join(f"- {t}" for t in memory_texts)

    # Build provider chain: active + all configured fallbacks
    provider_chain = _get_provider_chain()
    if not provider_chain:
        provider_chain = [_get_active_provider()]

    provider = provider_chain[0]
    fallbacks = provider_chain[1:]  # Remaining providers for failover

    tools = _build_tools()

    messages = [{"role": "system", "content": _build_system_prompt(store)}]

    if memory_context:
        messages.append({
            "role": "system",
            "content": memory_context,
        })

    if context:
        messages.append({
            "role": "system",
            "content": f"Current application context:\n{context}",
        })

    messages.extend(user_messages)

    # Sanitize messages for strict providers (like Gemini)
    is_gemini = "google" in provider.get("id", "") or "gemini" in provider.get("model", "").lower()
    cleaned_messages = []
    
    for m in messages:
        role = m.get("role", "user")
        
        # Only keep fields allowed by OpenAI specification
        clean_m = {"role": role}
        
        if "content" in m and m["content"] is not None:
            content = str(m["content"])
        else:
            content = ""

        # Gemini rejects completely empty content strings unless it's a tool call
        if not content and not m.get("tool_calls") and is_gemini:
            content = " "

        clean_m["content"] = content
            
        if "tool_calls" in m and m["tool_calls"]:
            clean_m["tool_calls"] = m["tool_calls"]
            
        if "tool_call_id" in m:
            clean_m["tool_call_id"] = m["tool_call_id"]
            
        if "name" in m:
            clean_m["name"] = m["name"]
            
        # Map function role to tool if needed
        if role == "function":
            clean_m["role"] = "tool"
            if "name" in m and "tool_call_id" not in clean_m:
                clean_m["tool_call_id"] = m["name"] # Fake it for old format
                
        # Skip tool messages without tool_call_id
        if clean_m["role"] == "tool" and "tool_call_id" not in clean_m:
            continue
            
        cleaned_messages.append(clean_m)

    # Collect the full response for storage
    full_response = []

    async def _stream_with_storage():
        """Wrap the stream to capture the full response."""
        nonlocal full_response

        # Send conversation_id first
        yield f"data: {json.dumps({'conversation_id': conv_id})}\n\n"

        async def _events():
            # Resolve any pre-approved dangerous tool calls from a confirmation
            # resubmit, then run the normal bounded agentic loop.
            async for line in _resolve_approved_calls(
                cleaned_messages, approved_tool_calls,
                store=store, workspace_id=workspace_id, slug=slug, user_id=_chat_user_id,
            ):
                yield line
            async for line in _stream_chat(
                cleaned_messages, tools, provider, fallbacks,
                store=store, workspace_id=workspace_id, slug=slug, user_id=_chat_user_id,
            ):
                yield line

        async for chunk in _events():
            yield chunk

            # Parse content from the chunk for storage
            if chunk.startswith("data: ") and chunk.strip() != "data: [DONE]":
                try:
                    data = json.loads(chunk[6:])
                    if "content" in data:
                        full_response.append(data["content"])
                    # Persist every structured tool result. Follow-up turns such
                    # as "save those results" need the actual prior result, not
                    # only collection jobs that happen to expose a job_id.
                    if "tool_result" in data:
                        tr = data["tool_result"]
                        result_data = tr.get("result", {})
                        chat_history.add_message(
                            conv_id,
                            "tool",
                            f"{tr.get('name', 'tool')} result",
                            tool_data=json.dumps({
                                "name": tr.get("name"),
                                "result": result_data,
                            }),
                        )
                except (json.JSONDecodeError, KeyError):
                    pass

        # After stream completes, store the assistant response and extract memories
        response_text = "".join(full_response)
        if response_text:
            chat_history.add_message(conv_id, "assistant", response_text)

            # Extract and store memories from the conversation
            if memory.is_available() and last_user_msg:
                try:
                    # Store the exchange as episodic memory
                    memory.add_memory(
                        f"User asked: {last_user_msg[:200]}\nAssistant answered about: {response_text[:200]}",
                        workspace_id,
                        user_id=_chat_user_id,
                        metadata={"conversation_id": conv_id, "type": "episodic"},
                    )
                except Exception:
                    pass  # Memory is best-effort

    return StreamingResponse(
        _stream_with_storage(),
        media_type="text/event-stream",
        headers={
            "Cache-Control": "no-cache",
            "Connection": "keep-alive",
            "X-Accel-Buffering": "no",
        },
    )
