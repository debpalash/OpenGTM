"""
CopilotKit Runtime — FastAPI endpoint

Provides the /api/copilotkit endpoint that CopilotKit's frontend connects to.
Uses the configured AI provider from the settings database.
Supports the CopilotKit protocol: /info (GET+POST) and chat (POST).
Includes: conversation history, OpenMemory integration, tool execution.
"""

import json
import os
import httpx
import uuid
import asyncio
import threading
from typing import AsyncGenerator
from fastapi import APIRouter, Request
from fastapi.responses import StreamingResponse, JSONResponse

from apps.api.routers.settings import _db_get, PROVIDERS
from apps.api.services.leadgen.db import LeadDB
from apps.api.services import chat_history, memory

router = APIRouter(prefix="/api/copilotkit", tags=["CopilotKit"])


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
def list_conversations():
    """List all chat conversations."""
    convs = chat_history.list_conversations()
    return JSONResponse(content={"conversations": convs})


@router.get("/conversations/{conv_id}")
def get_conversation(conv_id: str):
    """Get messages for a conversation."""
    conv = chat_history.get_conversation(conv_id)
    if not conv:
        return JSONResponse(content={"error": "Not found"}, status_code=404)
    messages = chat_history.get_messages(conv_id)
    return JSONResponse(content={"conversation": conv, "messages": messages})


@router.delete("/conversations/{conv_id}")
def delete_conversation(conv_id: str):
    """Delete a conversation."""
    chat_history.delete_conversation(conv_id)
    return JSONResponse(content={"ok": True})


@router.get("/memories")
def list_memories():
    """List all stored memories (debug/transparency)."""
    memories = memory.get_all_memories()
    return JSONResponse(content={
        "memories": memories,
        "available": memory.is_available(),
    })


def _build_system_prompt() -> str:
    """Build a dynamic system prompt with ICP and live pipeline stats."""
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
        db = LeadDB()
        stats = db.get_stats()
        db.close()
        total = stats.get("total", 0)
        by_tier = stats.get("by_tier", {})
        stats_text = (
            f"**Total Leads:** {total}\n"
            f"**By Tier:** Hot: {by_tier.get('hot', 0)}, Warm: {by_tier.get('warm', 0)}, "
            f"Cold: {by_tier.get('cold', 0)}, Unqualified: {by_tier.get('unqualified', 0)}"
        )
    except Exception:
        stats_text = "Pipeline stats unavailable"

    return f"""You are Yupcha Sales AI, an expert B2B sales intelligence assistant for Yupcha.

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
- **ambitionbox_search** — Search AmbitionBox for Indian companies with ratings, reviews, employee counts, industry data. Use for market research, competitor analysis, finding hiring companies
- **ambitionbox_jobs** — Get current job listings for a company from AmbitionBox (requires company_id from ambitionbox_search)

## Response Guidelines
- Use **markdown formatting**: tables for data, bold for metrics, bullet lists for recommendations
- When showing leads, format as a table with company, city, score, tier, contact info
- Be **actionable**: don't just show data, suggest specific next steps
- When asked to find/collect leads, use start_collection tool
- Keep responses concise but data-rich
- Score context: Hot (75-100), Warm (50-74), Cold (25-49), Unqualified (0-24)
"""


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
                "description": "Search AmbitionBox for Indian companies with ratings, reviews, employee counts, industry, and job data. Use for company research, market analysis, competitor intel. Supports filters: industry, location, rating.",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "industry": {"type": "string", "description": "Industry filter, e.g. 'IT Services & Consulting', 'Banking', 'BPO'"},
                        "location": {"type": "string", "description": "City filter, e.g. 'Bangalore/Bengaluru', 'Mumbai', 'Pune'"},
                        "sort_by": {"type": "string", "enum": ["popular", "rating", "reviews"], "description": "Sort order", "default": "popular"},
                        "page": {"type": "integer", "description": "Page number (1-indexed)", "default": 1},
                        "limit": {"type": "integer", "description": "Results per page (max 20)", "default": 10},
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
                "name": "create_workbook",
                "description": "Create a new workbook from a natural language description. Auto-generates columns based on the user's intent. Example: 'Find SaaS CTOs in SF with email and LinkedIn' → workbook with company, contact, email, linkedin, title columns.",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "description": {"type": "string", "description": "Natural language description of what the workbook should do"},
                        "name": {"type": "string", "description": "Name for the workbook"},
                    },
                    "required": ["description"],
                },
            },
        },
        {
            "type": "function",
            "function": {
                "name": "add_workbook_column",
                "description": "Add a new column to an existing workbook. Supports enrichment columns (email finder, phone validator, etc.) and computed columns.",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "workbook_id": {"type": "string", "description": "The workbook UUID"},
                        "column_name": {"type": "string", "description": "Display name for the column"},
                        "column_type": {"type": "string", "enum": ["text", "email", "phone", "url", "number", "enrichment"], "description": "Column data type"},
                        "provider": {"type": "string", "description": "For enrichment columns: provider name (hunter_io, apollo_io, etc.)"},
                    },
                    "required": ["workbook_id", "column_name", "column_type"],
                },
            },
        },
    ]


async def _execute_tool(name: str, args: dict) -> str:
    """Execute a backend tool and return the result as a string."""
    db = LeadDB()
    try:
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
            return json.dumps(stats)

        elif name == "update_lead_status":
            db.update_status(args["lead_id"], args["status"], args.get("note", ""))
            return json.dumps({"ok": True, "lead_id": args["lead_id"], "new_status": args["status"]})

        elif name == "start_collection":
            job_id = str(uuid.uuid4())[:8]
            query = args["query"]
            db.create_job(job_id, query)

            def _run():
                from apps.api.services.leadgen.job_runner import JobRunner
                runner = JobRunner()
                asyncio.run(runner._process_job({"id": job_id, "query": query, "tier": 1}))

            threading.Thread(target=_run, daemon=True).start()
            return json.dumps({"ok": True, "job_id": job_id, "query": query,
                               "message": "Collection started with 6 strategies: Maps, Web, Directories, LinkedIn, Job Boards, Review Sites"})

        elif name == "enrich_lead":
            lead = db.get_lead(args["lead_id"])
            if not lead:
                return json.dumps({"error": "Lead not found"})

            enriched_fields = []

            # Website enrichment
            if lead.has_website and (not lead.has_email or not lead.has_phone):
                try:
                    from apps.api.services.leadgen.enrichment.website_scraper import _scrape_via_http
                    from apps.api.services.leadgen.http import StealthClient
                    import asyncio as _aio

                    client = StealthClient()
                    url = lead.website if lead.website.startswith("http") else f"https://{lead.website}"

                    loop = _aio.new_event_loop()
                    result = loop.run_until_complete(_scrape_via_http(client, url))
                    loop.close()

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
                from datetime import datetime, timezone
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
            location = [args["location"]] if args.get("location") else None

            result = await ambitionbox.search_companies(
                        page=args.get("page", 1),
                        limit=args.get("limit", 10),
                        sort_by=args.get("sort_by", "popular"),
                        industry=industry,
                        location=location,
                    )

            return json.dumps(result)

        elif name == "ambitionbox_jobs":
            from apps.api.services.leadgen.ambitionbox import ambitionbox
            result = await ambitionbox.get_company_jobs(
                        company_id=args["company_id"],
                        page=args.get("page", 1),
                    )

            return json.dumps(result)

        elif name == "create_workbook":
            description = args["description"]
            wb_name = args.get("name", f"Workbook — {description[:40]}")

            # Infer columns from description using keyword matching
            column_defs = [
                {"key": "company", "name": "Company", "type": "text"},
            ]

            desc_lower = description.lower()

            if any(w in desc_lower for w in ["email", "contact", "reach"]):
                column_defs.append({"key": "email", "name": "Email", "type": "email"})
            if any(w in desc_lower for w in ["phone", "call", "number"]):
                column_defs.append({"key": "phone", "name": "Phone", "type": "phone"})
            if any(w in desc_lower for w in ["linkedin", "social", "profile"]):
                column_defs.append({"key": "linkedin_url", "name": "LinkedIn", "type": "url"})
            if any(w in desc_lower for w in ["title", "cto", "ceo", "vp", "founder", "decision maker", "role"]):
                column_defs.append({"key": "contact_person", "name": "Contact", "type": "text"})
                column_defs.append({"key": "contact_title", "name": "Title", "type": "text"})
            if any(w in desc_lower for w in ["website", "domain", "url"]):
                column_defs.append({"key": "website", "name": "Website", "type": "url"})
            if any(w in desc_lower for w in ["city", "location", "where"]):
                column_defs.append({"key": "city", "name": "City", "type": "text"})
            if any(w in desc_lower for w in ["score", "qualify", "rank"]):
                column_defs.append({"key": "score", "name": "Score", "type": "number"})
            if any(w in desc_lower for w in ["size", "employees", "headcount"]):
                column_defs.append({"key": "company_size", "name": "Size", "type": "text"})

            # Ensure at least email + contact columns
            keys = [c["key"] for c in column_defs]
            if "email" not in keys:
                column_defs.append({"key": "email", "name": "Email", "type": "email"})
            if "contact_person" not in keys:
                column_defs.append({"key": "contact_person", "name": "Contact", "type": "text"})

            wb_id = str(uuid.uuid4())
            from apps.api.routers.workbooks import _get_db as get_wb_db
            conn = get_wb_db()
            import time as _time
            now = _time.time()
            conn.execute(
                "INSERT INTO workbooks (id, name, description, columns_config, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)",
                (wb_id, wb_name, description, json.dumps(column_defs), now, now),
            )
            conn.commit()
            conn.close()

            return json.dumps({
                "workbook_id": wb_id,
                "name": wb_name,
                "columns": [c["name"] for c in column_defs],
                "message": f"Created workbook '{wb_name}' with {len(column_defs)} columns. Open it at /workbooks/{wb_id}",
            })

        elif name == "add_workbook_column":
            wb_id = args["workbook_id"]
            col_name = args["column_name"]
            col_type = args["column_type"]
            provider = args.get("provider", "")

            from apps.api.routers.workbooks import _get_db as get_wb_db
            conn = get_wb_db()
            row = conn.execute("SELECT columns_config FROM workbooks WHERE id = ?", (wb_id,)).fetchone()
            if not row:
                conn.close()
                return json.dumps({"error": "Workbook not found"})

            columns = json.loads(row["columns_config"] or "[]")
            new_key = col_name.lower().replace(" ", "_").replace("-", "_")
            new_col = {"key": new_key, "name": col_name, "type": col_type}
            if provider:
                new_col["provider"] = provider
            columns.append(new_col)

            conn.execute(
                "UPDATE workbooks SET columns_config = ?, updated_at = ? WHERE id = ?",
                (json.dumps(columns), __import__("time").time(), wb_id),
            )
            conn.commit()
            conn.close()

            return json.dumps({
                "added": col_name,
                "type": col_type,
                "total_columns": len(columns),
            })

        return json.dumps({"error": f"Unknown tool: {name}"})
    finally:
        db.close()


# ── Chat completion proxy ────────────────────────────────────────

async def _stream_chat(
    messages: list,
    tools: list,
    provider: dict,
    fallback_providers: list = None,
) -> AsyncGenerator[str, None]:
    """Stream chat completion from the configured AI provider.

    On 429/rate-limit errors, automatically fails over to the next provider
    in fallback_providers.
    """
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

                        nl = "\n"
                        msg = f"{nl}{nl}> ⚡ *{provider_name} rate limited — switching to {next_name}...*{nl}{nl}"
                        yield f'data: {json.dumps({"content": msg})}\n\n'

                        async for chunk in _stream_chat(messages, tools, next_prov, remaining):
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

                        nl = "\n"
                        msg = f"{nl}{nl}> ⚡ *{provider_name} is down — switching to {next_name}...*{nl}{nl}"
                        yield f'data: {json.dumps({"content": msg})}\n\n'

                        async for chunk in _stream_chat(messages, tools, next_prov, remaining):
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
                            tool_results = []
                            for idx in sorted(accumulated_tool_calls.keys()):
                                tc = accumulated_tool_calls[idx]
                                fn_name = tc["function"]["name"]
                                if not tc["function"]["arguments"]:
                                    tc["function"]["arguments"] = "{}"
                                try:
                                    fn_args = json.loads(tc["function"]["arguments"])
                                except json.JSONDecodeError:
                                    fn_args = {}

                                yield f"data: {json.dumps({'tool_call': {'name': fn_name, 'args': fn_args}})}\n\n"
                                result = await _execute_tool(fn_name, fn_args)
                                yield f"data: {json.dumps({'tool_result': {'name': fn_name, 'result': json.loads(result)}})}\n\n"

                                tool_results.append({
                                    "role": "tool",
                                    "tool_call_id": tc["id"],
                                    "name": fn_name,
                                    "content": result,
                                })

                            follow_up = messages + [
                                {"role": "assistant", "content": "", "tool_calls": list(accumulated_tool_calls.values())},
                                *tool_results,
                            ]

                            async for chunk_line in _stream_chat(follow_up, tools, provider, fallback_providers):
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

            async for chunk in _stream_chat(messages, tools, next_prov, remaining):
                yield chunk
            return

        if isinstance(e, httpx.ConnectError):
            yield f'data: {json.dumps({"error": "Cannot reach AI provider. Check your API key and network connection in Settings."})}\n\n'
        else:
            yield f'data: {json.dumps({"error": "AI provider timed out. The service may be overloaded — try again in a moment."})}\n\n'
    except Exception as e:
        yield f'data: {json.dumps({"error": f"AI provider error: {str(e)[:200]}"})}\n\n'


# ── Main chat endpoint ───────────────────────────────────────────

@router.post("")
async def copilot_chat(request: Request):
    """Main CopilotKit chat endpoint — streams AI responses with tool use.

    Accepts:
        messages: list of {role, content}
        conversation_id: optional, to continue an existing conversation
        context: optional application context
    """
    body = await request.json()

    user_messages = body.get("messages", [])
    context = body.get("context", "")
    conv_id = body.get("conversation_id")

    # Get the last user message for memory operations
    last_user_msg = ""
    for m in reversed(user_messages):
        if m.get("role") == "user":
            last_user_msg = m.get("content", "")
            break

    # Create or continue conversation
    if not conv_id:
        title = chat_history.auto_title_from_message(last_user_msg)
        conv = chat_history.create_conversation(title)
        conv_id = conv["id"]

    # Store user message in history
    if last_user_msg:
        chat_history.add_message(conv_id, "user", last_user_msg)

    # Search memory for relevant context
    memory_context = ""
    if last_user_msg and memory.is_available():
        memories = memory.search_memory(last_user_msg, user_id="default", limit=5)
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

    messages = [{"role": "system", "content": _build_system_prompt()}]

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

        async for chunk in _stream_chat(cleaned_messages, tools, provider, fallbacks):
            yield chunk

            # Parse content from the chunk for storage
            if chunk.startswith("data: ") and chunk.strip() != "data: [DONE]":
                try:
                    data = json.loads(chunk[6:])
                    if "content" in data:
                        full_response.append(data["content"])
                    # Persist tool results that contain a job_id (for task progress cards)
                    if "tool_result" in data:
                        tr = data["tool_result"]
                        result_data = tr.get("result", {})
                        if isinstance(result_data, dict) and result_data.get("job_id"):
                            chat_history.add_message(
                                conv_id, "tool",
                                f"Started collection: {result_data.get('query', '')}",
                                tool_data=json.dumps(result_data),
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
                        user_id="default",
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
