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


def _get_active_provider() -> dict:
    """Get the currently configured AI provider and its credentials."""
    default_id = _db_get("LLM_DEFAULT_PROVIDER", "openrouter")
    prov = PROVIDERS.get(default_id)
    if not prov:
        prov = PROVIDERS.get("openrouter", PROVIDERS[list(PROVIDERS.keys())[0]])
        default_id = "openrouter"

    api_key = _db_get(prov["env_key"], "")
    base_url = _db_get(prov.get("env_url", ""), "") or prov.get("default_url", "")
    model = _db_get(prov.get("env_model", ""), "") or prov.get("default_model", "")

    return {
        "id": default_id,
        "api_key": api_key,
        "base_url": base_url,
        "model": model,
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

    return f"""You are LeadEngine AI, an expert B2B sales intelligence assistant for Yupcha.

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
    ]


def _execute_tool(name: str, args: dict) -> str:
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
                from datetime import datetime
                db.update_lead_fields(lead.id, {
                    "email": lead.email, "phone": lead.phone,
                    "linkedin_url": lead.linkedin_url, "description": lead.description,
                    "last_enriched_at": datetime.utcnow().isoformat(),
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

        return json.dumps({"error": f"Unknown tool: {name}"})
    finally:
        db.close()


# ── Chat completion proxy ────────────────────────────────────────

async def _stream_chat(messages: list, tools: list, provider: dict) -> AsyncGenerator[str, None]:
    """Stream chat completion from the configured AI provider."""
    api_key = provider["api_key"]
    base_url = provider["base_url"].rstrip("/")
    model = provider["model"]

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
                if response.status_code != 200:
                    error_body = await response.aread()
                    yield f'data: {json.dumps({"error": f"Provider error {response.status_code}: {error_body.decode()[:200]}"})}\n\n'
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
                                try:
                                    fn_args = json.loads(tc["function"]["arguments"])
                                except json.JSONDecodeError:
                                    fn_args = {}

                                yield f"data: {json.dumps({'tool_call': {'name': fn_name, 'args': fn_args}})}\n\n"
                                result = _execute_tool(fn_name, fn_args)
                                yield f"data: {json.dumps({'tool_result': {'name': fn_name, 'result': json.loads(result)}})}\n\n"

                                tool_results.append({
                                    "role": "tool",
                                    "tool_call_id": tc["id"],
                                    "content": result,
                                })

                            follow_up = messages + [
                                {"role": "assistant", "tool_calls": list(accumulated_tool_calls.values())},
                                *tool_results,
                            ]

                            async for chunk_line in _stream_chat(follow_up, tools, provider):
                                yield chunk_line
                            return

                    except json.JSONDecodeError:
                        continue

    except httpx.ConnectError:
        yield f'data: {json.dumps({"error": "Cannot reach AI provider. Check your API key and network connection in Settings."})}\n\n'
    except httpx.TimeoutException:
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

    provider = _get_active_provider()
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

    # Collect the full response for storage
    full_response = []

    async def _stream_with_storage():
        """Wrap the stream to capture the full response."""
        nonlocal full_response

        # Send conversation_id first
        yield f"data: {json.dumps({'conversation_id': conv_id})}\n\n"

        async for chunk in _stream_chat(messages, tools, provider):
            yield chunk

            # Parse content from the chunk for storage
            if chunk.startswith("data: ") and chunk.strip() != "data: [DONE]":
                try:
                    data = json.loads(chunk[6:])
                    if "content" in data:
                        full_response.append(data["content"])
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
