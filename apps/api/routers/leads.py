"""
Lead Management API Router — FastAPI

All lead CRUD, filtering, workspaces, collection jobs, SSE events, and CSV export.
Migrated from the Flask dashboard/app.py to FastAPI.
"""

import csv
import io
import json
import uuid
import time as _time
from datetime import datetime, timezone
from typing import Optional

from fastapi import APIRouter, Query, HTTPException, Request, Depends
from fastapi.responses import StreamingResponse, Response
from pydantic import BaseModel

from apps.api.services.leadgen.db import LeadDB
from apps.api.services.leadgen.models import Lead, LEAD_STATUSES
from apps.api.core.tenancy import (
    WorkspaceCtx,
    current_workspace,
    require_workspace_role,
)
from apps.api.core.ratelimit import limiter

router = APIRouter(prefix="/api", tags=["Leads"])

# Additional routers for workspace/job/events/search (mounted separately)
workspace_router = APIRouter(prefix="/api/workspaces", tags=["Workspaces"])
jobs_router = APIRouter(prefix="/api", tags=["Lead Jobs"])
events_router = APIRouter(tags=["SSE Events"])
search_router = APIRouter(prefix="/api", tags=["Search"])
require_editor = require_workspace_role("editor", "admin")


def _get_db() -> LeadDB:
    return LeadDB()


def _workspace_job_db(ctx: WorkspaceCtx) -> LeadDB:
    """Open the legacy job/stage ledger for exactly one workspace.

    Lead rows may live in shared Postgres behind RLS, but the collection
    pipeline's ``jobs`` and ``job_stages`` tables are still SQLite-backed.  The
    file therefore has to be resolved from the authenticated workspace slug;
    using bare ``LeadDB()`` here would expose the global/main ledger to every
    tenant.
    """
    from apps.api.services.workspace.manager import workspace_leads_db_path

    return LeadDB(workspace_leads_db_path(ctx.slug))


def _collection_fire_key(workspace_id: str, job_id: str) -> str:
    return f"collect:{workspace_id}:{job_id}"


def _cancel_collection_queue_job(workspace_id: str, job_id: str) -> int:
    """Cancel the durable queue half of a collection job, if still active."""
    from apps.api.database import SessionLocal
    from apps.api.models import Job

    now = datetime.now(timezone.utc)
    with SessionLocal() as db:
        changed = (
            db.query(Job)
            .filter(
                Job.fire_key == _collection_fire_key(workspace_id, job_id),
                Job.status.in_(("pending", "processing")),
            )
            .update(
                {
                    Job.status: "cancelled",
                    Job.completed_at: now,
                    Job.error: "Cancelled by user",
                    Job.worker_id: None,
                    Job.locked_at: None,
                },
                synchronize_session=False,
            )
        )
        db.commit()
        return int(changed or 0)


def _clean(val: Optional[str]) -> Optional[str]:
    if val and val != "__all__":
        return val
    return None


# ── Lead CRUD ─────────────────────────────────────────────────


@router.get("/leads")
def list_leads(
    status: Optional[str] = None,
    city: Optional[str] = None,
    source: Optional[str] = None,
    score_min: Optional[int] = None,
    score_max: Optional[int] = None,
    tier: Optional[str] = None,
    search: Optional[str] = None,
    workspace_id: Optional[str] = None,
    has_email: Optional[bool] = None,
    has_phone: Optional[bool] = None,
    company_size: Optional[str] = None,
    limit: int = 200,
    offset: int = 0,
    order_by: str = "score DESC",
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    db = ctx.lead_db()
    status_filter = _clean(status)
    leads = db.get_leads(
        status=status_filter,
        city=_clean(city),
        source=_clean(source),
        score_min=score_min,
        score_max=score_max,
        score_tier=_clean(tier),
        search=_clean(search),
        workspace_id=_clean(workspace_id),
        has_email=has_email,
        has_phone=has_phone,
        company_size=_clean(company_size),
        limit=limit,
        offset=offset,
        order_by=order_by,
    )
    if status_filter != "dead":
        leads = [l for l in leads if l.status != "dead"]
    result = [l.to_dict() for l in leads]
    db.close()
    return result


@router.get("/stats")
# Mounted at /api/stats — matches frontend expectation
def lead_stats(ctx: WorkspaceCtx = Depends(current_workspace)):
    db = ctx.lead_db()
    stats = db.get_stats()
    db.close()
    return stats


@router.get("/filters")
# Mounted at /api/filters — matches frontend expectation
def lead_filters(ctx: WorkspaceCtx = Depends(current_workspace)):
    db = ctx.lead_db()
    data = {
        "cities": db.get_cities(),
        "sources": db.get_sources(),
        "statuses": LEAD_STATUSES,
        "tiers": ["hot", "warm", "cold", "unqualified"],
    }
    db.close()
    return data


@router.get("/lead/{lead_id}")
def get_lead(lead_id: int, ctx: WorkspaceCtx = Depends(current_workspace)):
    db = ctx.lead_db()
    lead = db.get_lead(lead_id)
    db.close()
    if lead:
        return lead.to_dict()
    raise HTTPException(status_code=404, detail="Lead not found")


@router.get("/lead/{lead_id}/similar")
def similar_leads(lead_id: int, limit: int = 10, ctx: WorkspaceCtx = Depends(current_workspace)):
    """Find leads similar to a given lead (by specialization, then city)."""
    db = ctx.lead_db()
    lead = db.get_lead(lead_id)
    if not lead:
        db.close()
        raise HTTPException(status_code=404, detail="Lead not found")
    similar = []
    if lead.specialization:
        similar = db.get_leads(search=lead.specialization, limit=limit + 1)
    if not similar and lead.city:
        similar = db.get_leads(city=lead.city, limit=limit + 1)
    similar = [l for l in similar if l.id != lead.id][:limit]
    db.close()
    return {"reference": lead.company, "count": len(similar),
            "similar_leads": [l.to_dict() for l in similar]}


class StatusUpdate(BaseModel):
    status: str
    note: str = ""


@router.post("/lead/{lead_id}/status")
def update_lead_status(lead_id: int, body: StatusUpdate, ctx: WorkspaceCtx = Depends(require_editor)):
    if body.status not in LEAD_STATUSES:
        raise HTTPException(status_code=400, detail="Invalid lead status")
    db = ctx.lead_db()
    if not db.get_lead(lead_id):
        db.close()
        raise HTTPException(status_code=404, detail="Lead not found")
    db.update_status(lead_id, body.status, body.note)
    db.close()
    return {"ok": True}


@router.put("/lead/{lead_id}")
def update_lead(lead_id: int, body: dict, ctx: WorkspaceCtx = Depends(require_editor)):
    db = ctx.lead_db()
    if not db.get_lead(lead_id):
        db.close()
        raise HTTPException(status_code=404, detail="Lead not found")
    db.update_lead_fields(lead_id, body)
    db.close()
    return {"ok": True}


@router.delete("/lead/{lead_id}")
def delete_lead(lead_id: int, ctx: WorkspaceCtx = Depends(require_editor)):
    db = ctx.lead_db()
    if not db.get_lead(lead_id):
        db.close()
        raise HTTPException(status_code=404, detail="Lead not found")
    db.delete_lead(lead_id)
    db.close()
    return {"ok": True}


class AddLeadRequest(BaseModel):
    company: str
    city: str = ""
    website: str = ""
    email: str = ""
    phone: str = ""
    specialization: str = ""
    contact_person: str = ""
    linkedin_url: str = ""
    notes: str = ""
    source: str = "manual"


@router.post("/lead")
def add_lead(body: AddLeadRequest, ctx: WorkspaceCtx = Depends(require_editor)):
    lead = Lead.from_dict(body.model_dump())
    lead.source = body.source
    lead.workspace_id = ctx.workspace_id
    db = ctx.lead_db()
    lead_id = db.upsert_lead(lead)
    db.close()
    return {"ok": True, "id": lead_id}


# ── AI Enrichment ──────────────────────────────────────────────


@router.post("/lead/{lead_id}/enrich")
@limiter.limit("30/minute")
async def enrich_lead(
    request: Request,
    lead_id: int,
    action: str = "web_research",
    ctx: WorkspaceCtx = Depends(require_editor),
):
    """AI-powered lead enrichment. Streams SSE progress events.

    Actions:
    - web_research: Use LLM + web search to gather company intel
    - find_emails: Discover email patterns for the company
    - scrape_website: Extract data from the company's website
    """
    db = ctx.lead_db()
    lead = db.get_lead(lead_id)
    if not lead:
        db.close()
        raise HTTPException(status_code=404, detail="Lead not found")

    async def _stream():
        try:
            if action == "web_research":
                yield f"data: {json.dumps({'step': 'start', 'action': 'web_research', 'message': f'Researching {lead.company}...'})}\n\n"

                # Use the configured LLM to research
                from apps.api.routers.copilotkit import _get_active_provider
                import httpx

                provider = _get_active_provider()
                if not provider["api_key"]:
                    yield f"data: {json.dumps({'step': 'error', 'message': 'No AI provider configured. Go to Settings.'})}\n\n"
                    return

                prompt = f"""Research this company and provide a comprehensive analysis:

Company: {lead.company}
City: {lead.city or 'Unknown'}
Website: {lead.website or 'Unknown'}
Specialization: {lead.specialization or 'Unknown'}
Current description: {lead.description or 'None'}

Provide:
1. **Company Overview** (2-3 sentences about what they do)
2. **Key Services/Products** they offer
3. **Company Size** estimate if possible
4. **Industry/Niche** they operate in
5. **Potential Needs** — what problems they likely face
6. **Outreach Angle** — best way to approach them as a lead

Format as clean text with section headers. Be specific and actionable."""

                headers = {
                    "Authorization": f"Bearer {provider['api_key']}",
                    "Content-Type": "application/json",
                }
                body = {
                    "model": provider["model"],
                    "messages": [
                        {"role": "system", "content": "You are a business analyst who researches companies for sales intelligence. Be concise and specific."},
                        {"role": "user", "content": prompt},
                    ],
                    "stream": True,
                    "temperature": 0.5,
                }
                url = f"{provider['base_url'].rstrip('/')}/chat/completions"

                full_content = ""
                async with httpx.AsyncClient(timeout=60.0) as client:
                    async with client.stream("POST", url, json=body, headers=headers) as resp:
                        if resp.status_code != 200:
                            err = await resp.aread()
                            yield f"data: {json.dumps({'step': 'error', 'message': f'LLM error: {err.decode()[:200]}'})}\n\n"
                            return

                        async for line in resp.aiter_lines():
                            if not line.startswith("data: "):
                                continue
                            data = line[6:]
                            if data == "[DONE]":
                                break
                            try:
                                chunk = json.loads(data)
                                delta = chunk.get("choices", [{}])[0].get("delta", {})
                                if delta.get("content"):
                                    token = delta["content"]
                                    full_content += token
                                    yield f"data: {json.dumps({'step': 'token', 'content': token})}\n\n"
                            except json.JSONDecodeError:
                                continue

                # Save to lead
                if full_content:
                    db.update_lead_fields(lead_id, {
                        "description": full_content[:2000],
                        "last_enriched_at": "now",
                    })
                    yield f"data: {json.dumps({'step': 'saved', 'message': 'Research saved to lead'})}\n\n"

                yield f"data: {json.dumps({'step': 'done', 'action': 'web_research'})}\n\n"

            elif action == "find_emails":
                yield f"data: {json.dumps({'step': 'start', 'action': 'find_emails', 'message': 'Finding emails...'})}\n\n"

                from apps.api.services.leadgen.enrichment.email_finder import enrich_emails
                enriched = enrich_emails([lead], delay=0.5)
                if enriched and enriched[0].email:
                    db.update_lead_fields(lead_id, {"email": enriched[0].email, "last_enriched_at": "now"})
                    yield f"data: {json.dumps({'step': 'result', 'email': enriched[0].email})}\n\n"
                else:
                    yield f"data: {json.dumps({'step': 'result', 'message': 'No email found'})}\n\n"

                yield f"data: {json.dumps({'step': 'done', 'action': 'find_emails'})}\n\n"

            elif action == "scrape_website":
                if not lead.website:
                    yield f"data: {json.dumps({'step': 'error', 'message': 'No website URL on this lead'})}\n\n"
                    return

                yield f"data: {json.dumps({'step': 'start', 'action': 'scrape_website', 'message': f'Scraping {lead.website}...'})}\n\n"

                from apps.api.services.leadgen.enrichment.website_scraper import enrich_leads_from_websites
                enriched = await enrich_leads_from_websites([lead])
                if enriched:
                    updated = enriched[0]
                    fields = {}
                    if updated.email and updated.email != lead.email:
                        fields["email"] = updated.email
                    if updated.phone and updated.phone != lead.phone:
                        fields["phone"] = updated.phone
                    if updated.description and updated.description != lead.description:
                        fields["description"] = updated.description
                    if updated.contact_person and updated.contact_person != lead.contact_person:
                        fields["contact_person"] = updated.contact_person

                    if fields:
                        fields["last_enriched_at"] = "now"
                        db.update_lead_fields(lead_id, fields)
                        yield f"data: {json.dumps({'step': 'result', 'fields_updated': list(fields.keys())})}\n\n"
                    else:
                        yield f"data: {json.dumps({'step': 'result', 'message': 'No new data found'})}\n\n"

                yield f"data: {json.dumps({'step': 'done', 'action': 'scrape_website'})}\n\n"

            elif action == "find_phone":
                yield f"data: {json.dumps({'step': 'start', 'action': 'find_phone', 'message': f'Searching phone for {lead.company}...'})}\n\n"

                from apps.api.services.leadgen.enrichment.search_enricher import _extract_phone
                from ddgs import DDGS

                queries = [
                    f"{lead.company} {lead.city or ''} contact number phone",
                    f"{lead.company} {lead.city or ''} office phone number",
                ]
                found_phone = ""
                for q in queries:
                    try:
                        with DDGS() as ddgs:
                            results = list(ddgs.text(q.strip(), max_results=5))
                            for r in results:
                                text = f"{r.get('title', '')} {r.get('body', '')}"
                                phone = _extract_phone(text)
                                if phone:
                                    found_phone = phone
                                    break
                        if found_phone:
                            break
                    except Exception:
                        continue

                if found_phone:
                    db.update_lead_fields(lead_id, {"phone": found_phone, "last_enriched_at": "now"})
                    yield f"data: {json.dumps({'step': 'result', 'message': f'Phone found: {found_phone}', 'phone': found_phone})}\n\n"
                else:
                    yield f"data: {json.dumps({'step': 'result', 'message': 'No phone number found'})}\n\n"

                yield f"data: {json.dumps({'step': 'done', 'action': 'find_phone'})}\n\n"

            elif action == "find_address":
                yield f"data: {json.dumps({'step': 'start', 'action': 'find_address', 'message': f'Looking up address for {lead.company}...'})}\n\n"

                import aiohttp

                address_found = ""
                lat, lon = "", ""

                # Strategy 1: OpenStreetMap Nominatim (free, no auth)
                search_q = f"{lead.company}, {lead.city or 'India'}"
                try:
                    async with aiohttp.ClientSession() as session:
                        async with session.get(
                            "https://nominatim.openstreetmap.org/search",
                            params={"q": search_q, "format": "json", "limit": 3, "addressdetails": 1},
                            headers={"User-Agent": "LeadEngine/1.0"},
                            timeout=aiohttp.ClientTimeout(total=10),
                        ) as resp:
                            if resp.status == 200:
                                results = await resp.json()
                                if results:
                                    best = results[0]
                                    address_found = best.get("display_name", "")
                                    lat = str(best.get("lat", ""))
                                    lon = str(best.get("lon", ""))
                                    yield f"data: {json.dumps({'step': 'result', 'message': f'OSM: {address_found[:100]}', 'source': 'openstreetmap'})}\n\n"
                except Exception as e:
                    yield f"data: {json.dumps({'step': 'result', 'message': f'OSM lookup failed: {str(e)[:80]}'})}\n\n"

                # Strategy 2: DDG search fallback
                if not address_found:
                    try:
                        from ddgs import DDGS
                        import re as _re2
                        with DDGS() as ddgs:
                            q = f"{lead.company} {lead.city or ''} office address location"
                            results = list(ddgs.text(q.strip(), max_results=5))
                            for r in results:
                                body = r.get("body", "")
                                addr_match = _re2.search(
                                    r'(?:address|located|office)[:\s]+([^.]+)',
                                    body, _re2.IGNORECASE
                                )
                                if addr_match:
                                    address_found = addr_match.group(1).strip()[:200]
                                    yield f"data: {json.dumps({'step': 'result', 'message': f'Search: {address_found[:100]}', 'source': 'web_search'})}\n\n"
                                    break
                    except Exception:
                        pass

                if address_found:
                    fields_to_update = {"last_enriched_at": "now"}
                    addr_note = f"\n\n📍 Address: {address_found[:300]}"
                    if lat and lon:
                        addr_note += f"\n🗺️ Map: https://www.openstreetmap.org/?mlat={lat}&mlon={lon}#map=16/{lat}/{lon}"
                    fields_to_update["notes"] = (lead.notes or "").rstrip() + addr_note
                    if not lead.state:
                        indian_states = ["Maharashtra", "Karnataka", "Tamil Nadu", "Delhi", "Telangana",
                                         "Gujarat", "Rajasthan", "Uttar Pradesh", "West Bengal", "Kerala",
                                         "Madhya Pradesh", "Haryana", "Punjab", "Andhra Pradesh", "Bihar"]
                        for st in indian_states:
                            if st.lower() in address_found.lower():
                                fields_to_update["state"] = st
                                break

                    db.update_lead_fields(lead_id, fields_to_update)
                    yield f"data: {json.dumps({'step': 'saved', 'message': 'Address saved'})}\n\n"
                else:
                    yield f"data: {json.dumps({'step': 'result', 'message': 'No address found'})}\n\n"

                yield f"data: {json.dumps({'step': 'done', 'action': 'find_address'})}\n\n"

            else:
                yield f"data: {json.dumps({'step': 'error', 'message': f'Unknown action: {action}'})}\n\n"

        except Exception as e:
            yield f"data: {json.dumps({'step': 'error', 'message': str(e)})}\n\n"
        finally:
            db.close()

    return StreamingResponse(
        _stream(),
        media_type="text/event-stream",
        headers={"Cache-Control": "no-cache", "X-Accel-Buffering": "no"},
    )


# ── CSV Export ─────────────────────────────────────────────────


@router.get("/export/csv")
# Mounted at /api/export/csv — matches frontend expectation
def export_csv(
    status: Optional[str] = None,
    city: Optional[str] = None,
    tier: Optional[str] = None,
    score_min: Optional[int] = None,
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    db = ctx.lead_db()
    leads = db.get_leads(
        status=status,
        city=city,
        score_tier=tier,
        score_min=score_min,
        limit=10000,
    )
    db.close()

    output = io.StringIO()
    writer = csv.writer(output)
    writer.writerow([
        "Company", "Website", "Email", "Phone", "City", "State", "Address",
        "Specialization", "Description", "Industry Tags",
        "Score", "Tier", "Status", "Source",
        "LinkedIn", "Contact Person", "Company Size", "Employee Count",
        "Founded Year", "Technologies", "Funding Stage", "Revenue Range",
        "Glassdoor Rating", "Decision Makers",
        "Secondary Emails", "Secondary Phones", "Notes",
    ])
    for l in leads:
        writer.writerow([
            l.company, l.website, l.email, l.phone, l.city,
            l.state, l.address, l.specialization, l.description,
            l.industry_tags, l.score, l.score_tier, l.status,
            l.source, l.linkedin_url, l.contact_person,
            l.company_size, getattr(l, 'employee_count_exact', ''),
            l.founded_year, l.technologies, l.funding_stage,
            l.revenue_range, l.glassdoor_rating,
            getattr(l, 'decision_makers', ''),
            getattr(l, 'secondary_emails', ''),
            getattr(l, 'secondary_phones', ''),
            l.notes,
        ])

    return Response(
        content=output.getvalue(),
        media_type="text/csv",
        headers={"Content-Disposition": "attachment; filename=opengtm_leads.csv"},
    )


# ── Collection Jobs ─────────────────────────────────────────────


class CollectRequest(BaseModel):
    query: str
    intent: Optional[str] = None
    # Accepted for backwards compatibility only. Tenant identity always comes
    # from the authenticated WorkspaceCtx and this value is never trusted.
    workspace_id: str = ""


@jobs_router.post("/collect")
@limiter.limit("20/minute")
def start_collection(request: Request, body: CollectRequest, ctx: WorkspaceCtx = Depends(require_editor)):
    query = body.query.strip()
    if not query:
        raise HTTPException(status_code=400, detail="query is required")

    from apps.api.services.leadgen.collection_intent import decide_collection

    decision = decide_collection(query, body.intent)
    if decision.clarification_required:
        # This is a successful interpretation response, not an exception: the
        # client must present the user with the safe specialized routes.  Most
        # importantly, no job row and no durable queue entry exist yet.
        return {
            "ok": False,
            **decision.to_dict(),
            "message": decision.reason,
        }

    job_id = uuid.uuid4().hex
    # Job/stage bookkeeping is per workspace. Never accept a tenant identifier
    # from the body: that was an IDOR primitive when paired with the old global
    # LeadDB ledger.
    db = _workspace_job_db(ctx)
    db.create_job(
        job_id,
        query,
        intent=decision.intent,
        intent_details=json.dumps(decision.to_dict()),
    )
    db.conn.execute(
        "UPDATE jobs SET workspace_id = ? WHERE id = ?",
        (ctx.workspace_id, job_id),
    )
    db.conn.commit()
    db.close()

    # Run the collection on the DURABLE queue (heartbeat-tracked, reaper-recovered,
    # retried) instead of a fire-and-forget daemon thread. The old thread lost the
    # entire ~9-minute run on any API reload/restart and left the job stuck at
    # status='running', leads_found=0 with nothing persisted. The queue worker
    # picks up the "collect" job and runs the same JobRunner pipeline via
    # job_runner.handle_collect.
    from apps.api.services.queue_service import queue_service
    from apps.api.database import SessionLocal
    try:
        with SessionLocal() as qdb:
            queued = queue_service.add_job(
                qdb,
                "collect",
                {
                    "job_id": job_id,
                    "query": query,
                    "intent": decision.intent,
                    "workspace_id": ctx.workspace_id,
                    "slug": ctx.slug,
                },
                fire_key=_collection_fire_key(ctx.workspace_id, job_id),
            )
    except Exception as exc:
        # Cross-database creation cannot be atomic. Compensate visibly so the UI
        # never shows a permanently pending job that was not actually queued.
        db = _workspace_job_db(ctx)
        db.conn.execute(
            "UPDATE jobs SET status = 'failed', error = ?, completed_at = ? WHERE id = ?",
            (f"Queue enqueue failed: {exc}", datetime.now(timezone.utc).isoformat(), job_id),
        )
        db.conn.commit()
        db.close()
        raise HTTPException(status_code=503, detail="Collection queue unavailable") from exc

    from apps.api.services.leadgen.progress import progress
    progress.bind_job(job_id, ctx.workspace_id)
    progress.emit(
        "job_created",
        {
            "job_id": job_id,
            "query": query,
            "workspace_id": ctx.workspace_id,
            "message": f"Collection queued: {query}",
        },
    )
    return {
        "ok": True,
        "job_id": job_id,
        "queue_job_id": queued.id,
        "query": query,
        "intent": decision.intent,
        "workspace_id": ctx.workspace_id,
    }


@jobs_router.get("/jobs")
def list_jobs(status: Optional[str] = None, ctx: WorkspaceCtx = Depends(current_workspace)):
    db = _workspace_job_db(ctx)
    jobs = db.get_jobs(status=_clean(status))
    db.close()
    return jobs


@jobs_router.get("/jobs/{job_id}")
def get_job_detail(job_id: str, ctx: WorkspaceCtx = Depends(current_workspace)):
    """Get a single job with all its pipeline stages."""
    db = _workspace_job_db(ctx)
    job = db.get_job_detail(job_id)
    db.close()
    if not job:
        raise HTTPException(status_code=404, detail="Job not found")
    try:
        job["intent_details"] = json.loads(job.get("intent_details", "{}"))
    except Exception:
        job["intent_details"] = {}
    # Parse JSON details in stages
    for stage in job.get("stages", []):
        try:
            stage["details"] = json.loads(stage.get("details", "{}"))
        except Exception:
            stage["details"] = {}
    return job


@jobs_router.get("/jobs/{job_id}/stages")
def get_job_stages(job_id: str, ctx: WorkspaceCtx = Depends(current_workspace)):
    """Get pipeline stages for a job."""
    db = _workspace_job_db(ctx)
    if not db.get_job_detail(job_id):
        db.close()
        raise HTTPException(status_code=404, detail="Job not found")
    stages = db.get_job_stages(job_id)
    db.close()
    for s in stages:
        try:
            s["details"] = json.loads(s.get("details", "{}"))
        except Exception:
            s["details"] = {}
    return stages


@jobs_router.get("/jobs/{job_id}/leads")
def get_job_leads(job_id: str, ctx: WorkspaceCtx = Depends(current_workspace)):
    """Get leads produced by a specific job."""
    job_db = _workspace_job_db(ctx)
    if not job_db.get_job_detail(job_id):
        job_db.close()
        raise HTTPException(status_code=404, detail="Job not found")
    job_db.close()

    store = ctx.lead_db()
    try:
        return [
            lead.to_dict()
            for lead in store.get_leads(collection_job_id=job_id, limit=10_000)
        ]
    finally:
        close = getattr(store, "close", None)
        if close:
            close()


@jobs_router.get("/jobs/{job_id}/events")
def get_job_events(job_id: str, limit: int = 100, ctx: WorkspaceCtx = Depends(current_workspace)):
    """Recent progress events for a job — backfills the live activity feed so a
    freshly opened task page shows what already happened, not just future events."""
    from apps.api.services.leadgen.progress import progress
    db = _workspace_job_db(ctx)
    exists = db.get_job_detail(job_id)
    db.close()
    if not exists:
        raise HTTPException(status_code=404, detail="Job not found")
    events = [
        e for e in progress.recent(500, workspace_id=ctx.workspace_id)
        if e.get("job_id") == job_id and e.get("message")
    ]
    return {"events": events[-limit:]}


@jobs_router.post("/jobs/{job_id}/cancel")
def cancel_job(job_id: str, ctx: WorkspaceCtx = Depends(require_editor)):
    """Cancel a running or pending job."""
    db = _workspace_job_db(ctx)
    if not db.get_job_detail(job_id):
        db.close()
        raise HTTPException(status_code=404, detail="Job not found")
    db.cancel_job(job_id)
    db.close()
    _cancel_collection_queue_job(ctx.workspace_id, job_id)
    return {"ok": True, "message": f"Job {job_id} cancelled"}


@jobs_router.delete("/jobs/{job_id}")
def delete_job(job_id: str, keep_leads: bool = False, ctx: WorkspaceCtx = Depends(require_editor)):
    """Delete a job and its data. If keep_leads=true, keeps the leads."""
    db = _workspace_job_db(ctx)
    if not db.get_job_detail(job_id):
        db.close()
        raise HTTPException(status_code=404, detail="Job not found")
    _cancel_collection_queue_job(ctx.workspace_id, job_id)
    if not keep_leads:
        store = ctx.lead_db()
        try:
            for lead in store.get_leads(collection_job_id=job_id, limit=10_000):
                if lead.id is not None:
                    store.delete_lead(lead.id)
        finally:
            close = getattr(store, "close", None)
            if close:
                close()
    # Lead deletion above uses the actual tenant store (PG or SQLite). Only
    # remove the per-workspace job/stage metadata here.
    db.delete_job(job_id, keep_leads=True)
    db.close()
    return {"ok": True, "message": f"Job {job_id} deleted", "leads_kept": keep_leads}


@jobs_router.post("/jobs/{job_id}/retry")
def retry_job(job_id: str, ctx: WorkspaceCtx = Depends(require_editor)):
    """Reset a failed/cancelled job to pending for re-processing."""
    db = _workspace_job_db(ctx)
    job = db.get_job_detail(job_id)
    if not job:
        db.close()
        raise HTTPException(status_code=404, detail="Job not found")
    if job.get("status") not in ("failed", "cancelled"):
        db.close()
        raise HTTPException(status_code=409, detail="Only failed or cancelled jobs can be retried")
    db.retry_job(job_id)
    db.close()

    from apps.api.services.queue_service import queue_service
    from apps.api.database import SessionLocal
    try:
        with SessionLocal() as qdb:
            queued = queue_service.add_job(
                qdb,
                "collect",
                {
                    "job_id": job_id,
                    "query": job["query"],
                    "workspace_id": ctx.workspace_id,
                    "slug": ctx.slug,
                },
                fire_key=_collection_fire_key(ctx.workspace_id, job_id),
            )
    except Exception as exc:
        db = _workspace_job_db(ctx)
        db.conn.execute(
            "UPDATE jobs SET status = 'failed', error = ?, completed_at = ? WHERE id = ?",
            (f"Retry enqueue failed: {exc}", datetime.now(timezone.utc).isoformat(), job_id),
        )
        db.conn.commit()
        db.close()
        raise HTTPException(status_code=503, detail="Collection queue unavailable") from exc
    return {
        "ok": True,
        "queue_job_id": queued.id,
        "message": f"Job {job_id} queued for retry",
    }


@jobs_router.get("/system-stats")
def system_stats(ctx: WorkspaceCtx = Depends(current_workspace)):
    from apps.api.services.leadgen.proxy_pool import ProxyPool
    from apps.api.services.leadgen.rate_limiter import RateLimiter

    pp = ProxyPool()
    rl = RateLimiter()
    db = _workspace_job_db(ctx)
    jobs = db.get_jobs(limit=100)
    db.close()

    job_stats = {"total": len(jobs)}
    for s in ["pending", "running", "done", "failed"]:
        job_stats[s] = sum(1 for j in jobs if j["status"] == s)

    return {
        "proxy_pool": pp.stats(),
        "rate_limiter": rl.stats(),
        "jobs": job_stats,
    }


# ── Workspaces ─────────────────────────────────────────────────


@workspace_router.get("")
def list_workspaces(ctx: WorkspaceCtx = Depends(current_workspace)):
    db = _get_db()
    workspaces = db.get_workspaces()
    db.close()
    return workspaces


class CreateWorkspaceRequest(BaseModel):
    name: str
    description: str = ""


@workspace_router.post("")
def create_workspace(body: CreateWorkspaceRequest, ctx: WorkspaceCtx = Depends(current_workspace)):
    name = body.name.strip()
    if not name:
        raise HTTPException(status_code=400, detail="name is required")
    db = _get_db()
    ws_id = db.create_workspace(name, body.description)
    db.close()
    return {"ok": True, "id": ws_id, "name": name}


@workspace_router.delete("/{ws_id}")
def delete_workspace(ws_id: str, ctx: WorkspaceCtx = Depends(current_workspace)):
    db = _get_db()
    db.delete_workspace(ws_id)
    db.close()
    return {"ok": True}


# ── SSE Events ─────────────────────────────────────────────────


def _authenticate_query_token(token: Optional[str]):
    """Authenticate an SSE/WS request whose token rides in the query string
    (EventSource/WebSocket cannot send an Authorization header). Returns the
    User on success; raises 401 otherwise."""
    from apps.api.core.security import authenticate_query_token

    return authenticate_query_token(token)


@events_router.get("/api/events")
def sse_events(
    token: Optional[str] = Query(default=None),
    workspace_id: Optional[str] = Query(default=None),
):
    user = _authenticate_query_token(token)
    from apps.api.services.workspace import manager as ws_manager

    target_workspace = workspace_id or ws_manager.get_user_active_workspace(user.id)
    if not target_workspace or not ws_manager.is_member(target_workspace, user.id):
        # Fail closed and do not reveal whether the requested workspace exists.
        raise HTTPException(status_code=403, detail="Workspace access denied")
    from apps.api.services.leadgen.progress import progress

    def stream():
        pubsub = progress.open_redis_subscription(target_workspace)
        q = None if pubsub is not None else progress.subscribe(target_workspace)
        last_heartbeat = _time.monotonic()
        try:
            while True:
                event = None
                if pubsub is not None:
                    try:
                        message = pubsub.get_message(
                            ignore_subscribe_messages=True, timeout=0.5
                        )
                        if message and message.get("type") == "message":
                            event = json.loads(message["data"])
                    except Exception:
                        try:
                            pubsub.close()
                        except Exception:
                            pass
                        pubsub = None
                        q = progress.subscribe(target_workspace)
                elif q:
                    while q:
                        event = q.popleft()
                        yield f"data: {json.dumps(event)}\n\n"
                if event is not None:
                    yield f"data: {json.dumps(event)}\n\n"
                    last_heartbeat = _time.monotonic()
                elif _time.monotonic() - last_heartbeat >= 15:
                    yield f"data: {json.dumps({'type': 'heartbeat'})}\n\n"
                    last_heartbeat = _time.monotonic()
                if pubsub is None:
                    _time.sleep(0.5)
        finally:
            if pubsub is not None:
                try:
                    pubsub.close()
                except Exception:
                    pass
            if q is not None:
                progress.unsubscribe(q)

    return StreamingResponse(
        stream(),
        media_type="text/event-stream",
        headers={"Cache-Control": "no-cache", "X-Accel-Buffering": "no"},
    )

# ── Search & Scraper (authenticated) ─────────────────────────

@search_router.get("/v2/search/unified")
@limiter.limit("60/minute")
async def public_unified_search(
    request: Request,
    q: str = "",
    sources: str = "",
    limit: int = 20,
    sort: str = "relevance",
    file_type: str = "",
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    """Unified multi-source document search (auth required)."""
    if not q:
        return {"query": "", "total": 0, "results": []}
    from apps.api.sources.registry import SourceRegistry
    if not hasattr(public_unified_search, "_registry"):
        public_unified_search._registry = SourceRegistry()
    registry = public_unified_search._registry
    source_list = [s.strip() for s in sources.split(",") if s.strip()] or None
    result = await registry.search_unified(
        query=q,
        sources=source_list,
        limit_per_source=max(5, limit // max(len(source_list or [1]), 1)),
        sort_by=sort,
        file_type=file_type or None,
    )
    # Convert SearchResult pydantic models to dicts
    serialized = []
    for r in result.get("results", []):
        d = r.model_dump() if hasattr(r, 'model_dump') else r.__dict__
        # Normalize field names for frontend
        d["description"] = d.get("snippet", "")
        serialized.append(d)
    return {"query": q, "total": len(serialized), "results": serialized}


@search_router.post("/v2/scraper/scrape")
@limiter.limit("30/minute")
async def public_scrape(request: Request, body: dict, ctx: WorkspaceCtx = Depends(require_editor)):
    """Scrape a single user-supplied URL (auth required).

    The scraper validates the initial URL and every redirect/navigation target,
    blocking internal, loopback, and metadata endpoints before each request.
    """
    url = body.get("url", "").strip()
    if not url:
        raise HTTPException(status_code=400, detail="url is required")
    from apps.api.core.url_guard import check_url, BlockedUrlError
    try:
        check_url(url, resolve=True)
    except BlockedUrlError as e:
        raise HTTPException(status_code=400, detail=f"url not allowed: {e}")
    from apps.api.services.scraper import UniversalScraper
    scraper = UniversalScraper()
    try:
        result = await scraper.scrape(url)
        return result
    except BlockedUrlError as e:
        raise HTTPException(status_code=400, detail=f"url not allowed: {e}") from e
    except Exception as e:
        return {"url": url, "status": "error", "error": str(e)}


# ── Deduplication ─────────────────────────────────────────────────────────

@router.post("/leads/dedup")
def run_dedup(
    threshold: float = Query(0.85, ge=0.5, le=1.0),
    city: Optional[str] = None,
    source: Optional[str] = None,
    tier: Optional[str] = None,
    search: Optional[str] = None,
    limit: int = Query(2000, ge=1, le=5000),
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    """Analyze a BOUNDED, filtered subset of leads for duplicates (fuzzy match).

    Fuzzy dedup is ~O(n^2); running it over the full DB (hundreds of thousands
    of rows) would hang/OOM the server. Callers scope it with filters
    (city/source/tier/search) and a hard `limit` (default 2000, max 5000), e.g.
    "dedup within Pune" or "dedup this source". The UI nudges users to filter
    first for large datasets.
    """
    from apps.api.services.dedup import LeadDeduplicator

    db = ctx.lead_db()
    leads = [l.to_dict() for l in db.get_leads(
        city=_clean(city), source=_clean(source), score_tier=_clean(tier),
        search=_clean(search), limit=limit,
    )]
    db.close()
    if not leads:
        return {"stats": {"total_leads": 0, "duplicates_found": 0}, "pairs": [], "clusters": {}, "scanned": 0}

    dedup = LeadDeduplicator(threshold=threshold)
    result = dedup.find_duplicates(leads)
    result["scanned"] = len(leads)
    return result


@router.post("/leads/dedup/merge")
def merge_duplicates(body: dict, ctx: WorkspaceCtx = Depends(require_editor)):
    """Merge duplicate leads — keep master, delete duplicates."""
    from apps.api.services.dedup import LeadDeduplicator

    master_id = body.get("master_id")
    duplicate_ids = body.get("duplicate_ids", [])
    if not master_id or not duplicate_ids:
        raise HTTPException(400, "master_id and duplicate_ids required")

    db = ctx.lead_db()
    master_lead = db.get_lead(master_id)
    master = master_lead.to_dict() if master_lead else None
    if not master:
        db.close()
        raise HTTPException(404, f"Master lead {master_id} not found")

    duplicates = []
    for duplicate_id in duplicate_ids:
        duplicate = db.get_lead(duplicate_id)
        if duplicate:
            duplicates.append(duplicate.to_dict())
    if not duplicates:
        db.close()
        raise HTTPException(404, "No valid duplicate leads found")

    dedup = LeadDeduplicator()
    merged = dedup.merge_leads(master, duplicates)

    # Update master with merged data
    db.update_lead_fields(master_id, merged)

    # Delete duplicates
    deleted = 0
    for dup in duplicates:
        dup_id = dup.get("id")
        if dup_id:
            db.delete_lead(dup_id)
            deleted += 1

    db.close()

    return {
        "status": "merged",
        "master_id": master_id,
        "duplicates_deleted": deleted,
        "merged_lead": merged,
    }


# ── Bulk enrichment ───────────────────────────────────────────────────────

class BulkEnrichBody(BaseModel):
    lead_ids: list[int]
    action: str = "find_emails"   # find_emails | scrape_website


@router.post("/leads/bulk-enrich")
def bulk_enrich(body: BulkEnrichBody, ctx: WorkspaceCtx = Depends(require_editor)):
    """Enrich a batch of leads in the background; returns a job_id immediately.

    Reuses the batchable enrichment services (email_finder / website_scraper).
    Capped at 100 leads per call so a huge selection can't hammer providers.
    Progress streams over ProgressBus (/api/events) keyed by job_id.
    """
    import uuid as _uuid

    ids = list(dict.fromkeys(body.lead_ids))[:100]
    action = body.action
    if not ids:
        raise HTTPException(400, "lead_ids required")
    if action not in ("find_emails", "scrape_website"):
        raise HTTPException(400, "action must be 'find_emails' or 'scrape_website'")

    job_id = _uuid.uuid4().hex

    # Run bulk enrichment on the DURABLE queue (reaper-recovered, retried) instead
    # of a fire-and-forget daemon thread that abandoned the batch on any API
    # reload. The worker runs job_runner.handle_bulk_enrich, which reopens the
    # tenant's lead store from slug/workspace_id in the payload.
    from apps.api.services.queue_service import queue_service
    from apps.api.database import SessionLocal
    with SessionLocal() as qdb:
        queue_service.add_job(qdb, "bulk_enrich", {
            "job_id": job_id, "ids": ids, "action": action,
            "workspace_id": ctx.workspace_id, "slug": ctx.slug,
        })

    return {"ok": True, "job_id": job_id, "count": len(ids), "action": action}



# ── Data Collector Import ─────────────────────────────────────────────────

@router.post("/leads/import/data-collector")
def import_data_collector(
    module: str = Query("all", description="Module to import: cnpj, github, or all"),
    limit: Optional[int] = Query(None, ge=1, le=1_000_000, description="Max leads to import"),
    reset: bool = Query(False, description="Reset checkpoint to start from scratch"),
    ctx: WorkspaceCtx = Depends(require_editor),
):
    """Import leads from the Brazil data_collector submodule.

    Streams CSV row-by-row (low memory). Checkpoints after every batch.
    Auto-resumes from last position on crash/restart.

    - module=cnpj  — Import ~666K CNPJ government leads
    - module=github — Import ~30 GitHub miner leads
    - module=all   — Import both
    - reset=true   — Clear checkpoint and start from row 0
    """
    if module not in ("cnpj", "github", "all"):
        raise HTTPException(status_code=400, detail="module must be cnpj, github, or all")

    from sqlalchemy.exc import IntegrityError
    from apps.api.database import SessionLocal
    from apps.api.services.queue_service import queue_service
    from apps.api.services.leadgen.scrapers.data_collector_import import (
        _load_checkpoint,
        checkpoint_path_for,
    )

    checkpoint = _load_checkpoint(checkpoint_path_for(ctx.slug))
    job_id = f"br-import-{uuid.uuid4().hex}"
    try:
        with SessionLocal() as qdb:
            queued = queue_service.add_job(
                qdb,
                "data_collector_import",
                {
                    "job_id": job_id,
                    "module": module,
                    "limit": limit,
                    "reset": reset,
                    "workspace_id": ctx.workspace_id,
                    "slug": ctx.slug,
                },
                priority=1,
                fire_key=f"data-collector:{ctx.workspace_id}",
            )
    except IntegrityError as exc:
        raise HTTPException(
            status_code=409,
            detail="A data collector import is already active in this workspace",
        ) from exc

    return {
        "ok": True,
        "job_id": job_id,
        "queue_job_id": queued.id,
        "module": module,
        "limit": limit,
        "resuming_from_row": checkpoint.get("cnpj_row", 0),
        "message": f"Import queued durably (module={module}, limit={limit})",
    }


@router.get("/leads/import/data-collector/status")
def import_data_collector_status(ctx: WorkspaceCtx = Depends(current_workspace)):
    """Check the current checkpoint status of the data_collector import."""
    from apps.api.services.leadgen.scrapers.data_collector_import import (
        _load_checkpoint, checkpoint_path_for, CNPJ_CSV,
    )
    checkpoint = _load_checkpoint(checkpoint_path_for(ctx.slug))
    total_rows = 0
    if CNPJ_CSV.exists():
        # Fast line count without loading file contents
        with open(CNPJ_CSV, "rb") as f:
            total_rows = sum(1 for _ in f) - 1  # minus header

    return {
        "cnpj_rows_processed": checkpoint.get("cnpj_row", 0),
        "cnpj_total_rows": total_rows,
        "cnpj_pct": round(checkpoint.get("cnpj_row", 0) / max(total_rows, 1) * 100, 1),
        "github_done": checkpoint.get("github_done", False),
    }


# ── Domain Intelligence ───────────────────────────────────────────────────

@router.post("/leads/domain-intel")
async def domain_intelligence(body: dict, ctx: WorkspaceCtx = Depends(require_editor)):
    """Analyze a domain — RDAP registration, DNS, hosting, email provider, legitimacy score."""
    domain = body.get("domain", "").strip()
    if not domain:
        raise HTTPException(400, "domain is required")

    # Strip protocol/path if provided as URL
    if "://" in domain:
        from urllib.parse import urlparse
        domain = urlparse(domain).hostname or domain
    domain = domain.replace("www.", "").strip("/")

    from apps.api.services.leadgen.enrichment.domain_intel import analyze_domain
    result = await analyze_domain(domain)
    return result
