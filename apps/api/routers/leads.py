"""
Lead Management API Router — FastAPI

All lead CRUD, filtering, workspaces, collection jobs, SSE events, and CSV export.
Migrated from the Flask dashboard/app.py to FastAPI.
"""

import csv
import io
import json
import uuid
import asyncio
import threading
import time as _time
from typing import Optional

from fastapi import APIRouter, Query, HTTPException, Request, Depends
from fastapi.responses import StreamingResponse, Response
from pydantic import BaseModel

from apps.api.services.leadgen.db import LeadDB
from apps.api.services.leadgen.models import Lead, LEAD_STATUSES
from apps.api.core.tenancy import WorkspaceCtx, current_workspace

router = APIRouter(prefix="/api", tags=["Leads"])

# Additional routers for workspace/job/events/search (mounted separately)
workspace_router = APIRouter(prefix="/api/workspaces", tags=["Workspaces"])
jobs_router = APIRouter(prefix="/api", tags=["Lead Jobs"])
events_router = APIRouter(tags=["SSE Events"])
search_router = APIRouter(prefix="/api", tags=["Search"])


def _get_db() -> LeadDB:
    return LeadDB()


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


class StatusUpdate(BaseModel):
    status: str
    note: str = ""


@router.post("/lead/{lead_id}/status")
def update_lead_status(lead_id: int, body: StatusUpdate, ctx: WorkspaceCtx = Depends(current_workspace)):
    db = ctx.lead_db()
    db.update_status(lead_id, body.status, body.note)
    db.close()
    return {"ok": True}


@router.put("/lead/{lead_id}")
def update_lead(lead_id: int, body: dict, ctx: WorkspaceCtx = Depends(current_workspace)):
    db = ctx.lead_db()
    db.update_lead_fields(lead_id, body)
    db.close()
    return {"ok": True}


@router.delete("/lead/{lead_id}")
def delete_lead(lead_id: int, ctx: WorkspaceCtx = Depends(current_workspace)):
    db = ctx.lead_db()
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
def add_lead(body: AddLeadRequest, ctx: WorkspaceCtx = Depends(current_workspace)):
    lead = Lead.from_dict(body.model_dump())
    lead.source = body.source
    db = ctx.lead_db()
    lead_id = db.upsert_lead(lead)
    db.close()
    return {"ok": True, "id": lead_id}


# ── AI Enrichment ──────────────────────────────────────────────


@router.post("/lead/{lead_id}/enrich")
async def enrich_lead(
    lead_id: int,
    action: str = "web_research",
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    """AI-powered lead enrichment. Streams SSE progress events.

    Actions:
    - web_research: Use LLM + web search to gather company intel
    - find_emails: Discover email patterns for the company
    - scrape_website: Extract data from the company's website
    """
    from apps.api.services.workspace.manager import workspace_leads_db_path as _wpath
    _ws_path = _wpath(ctx.slug)
    db = LeadDB(_ws_path)
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
                    edb = LeadDB(_ws_path)
                    edb.update_lead_fields(lead_id, {
                        "description": full_content[:2000],
                        "last_enriched_at": "now",
                    })
                    edb.close()
                    yield f"data: {json.dumps({'step': 'saved', 'message': 'Research saved to lead'})}\n\n"

                yield f"data: {json.dumps({'step': 'done', 'action': 'web_research'})}\n\n"

            elif action == "find_emails":
                yield f"data: {json.dumps({'step': 'start', 'action': 'find_emails', 'message': 'Finding emails...'})}\n\n"

                from apps.api.services.leadgen.enrichment.email_finder import enrich_emails
                enriched = enrich_emails([lead], delay=0.5)
                if enriched and enriched[0].email:
                    edb = LeadDB(_ws_path)
                    edb.update_lead_fields(lead_id, {"email": enriched[0].email, "last_enriched_at": "now"})
                    edb.close()
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
                        edb = LeadDB(_ws_path)
                        edb.update_lead_fields(lead_id, fields)
                        edb.close()
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
                    edb = LeadDB(_ws_path)
                    edb.update_lead_fields(lead_id, {"phone": found_phone, "last_enriched_at": "now"})
                    edb.close()
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

                    edb = LeadDB(_ws_path)
                    edb.update_lead_fields(lead_id, fields_to_update)
                    edb.close()
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
        headers={"Content-Disposition": "attachment; filename=yupcha_leads.csv"},
    )


# ── Collection Jobs ─────────────────────────────────────────────


class CollectRequest(BaseModel):
    query: str
    workspace_id: str = ""


@jobs_router.post("/collect")
def start_collection(body: CollectRequest, ctx: WorkspaceCtx = Depends(current_workspace)):
    query = body.query.strip()
    if not query:
        raise HTTPException(status_code=400, detail="query is required")

    job_id = str(uuid.uuid4())[:8]
    # Jobs run through the shared pipeline DB (the background runner is bound to
    # it); tag the job with the caller's workspace for later scoping.
    body.workspace_id = body.workspace_id or ctx.workspace_id
    db = _get_db()
    db.create_job(job_id, query)
    if body.workspace_id:
        db.conn.execute(
            "UPDATE jobs SET workspace_id = ? WHERE id = ?",
            (body.workspace_id, job_id),
        )
        db.conn.commit()
    db.close()

    def _run_job():
        from apps.api.services.leadgen.job_runner import JobRunner
        runner = JobRunner()
        asyncio.run(runner._process_job({
            "id": job_id, "query": query, "tier": 1,
            "workspace_id": body.workspace_id,
        }))

    thread = threading.Thread(target=_run_job, daemon=True)
    thread.start()

    return {"ok": True, "job_id": job_id, "query": query, "workspace_id": body.workspace_id}


@jobs_router.get("/jobs")
def list_jobs(status: Optional[str] = None, ctx: WorkspaceCtx = Depends(current_workspace)):
    db = _get_db()
    jobs = db.get_jobs(status=_clean(status))
    db.close()
    return jobs


@jobs_router.get("/jobs/{job_id}")
def get_job_detail(job_id: str, ctx: WorkspaceCtx = Depends(current_workspace)):
    """Get a single job with all its pipeline stages."""
    db = _get_db()
    job = db.get_job_detail(job_id)
    db.close()
    if not job:
        raise HTTPException(status_code=404, detail="Job not found")
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
    db = _get_db()
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
    db = _get_db()
    leads = db.get_job_leads(job_id)
    db.close()
    return leads


@jobs_router.post("/jobs/{job_id}/cancel")
def cancel_job(job_id: str, ctx: WorkspaceCtx = Depends(current_workspace)):
    """Cancel a running or pending job."""
    db = _get_db()
    db.cancel_job(job_id)
    db.close()
    return {"ok": True, "message": f"Job {job_id} cancelled"}


@jobs_router.delete("/jobs/{job_id}")
def delete_job(job_id: str, keep_leads: bool = False, ctx: WorkspaceCtx = Depends(current_workspace)):
    """Delete a job and its data. If keep_leads=true, keeps the leads."""
    db = _get_db()
    db.delete_job(job_id, keep_leads=keep_leads)
    db.close()
    return {"ok": True, "message": f"Job {job_id} deleted", "leads_kept": keep_leads}


@jobs_router.post("/jobs/{job_id}/retry")
def retry_job(job_id: str, ctx: WorkspaceCtx = Depends(current_workspace)):
    """Reset a failed/cancelled job to pending for re-processing."""
    db = _get_db()
    db.retry_job(job_id)
    db.close()
    return {"ok": True, "message": f"Job {job_id} queued for retry"}


@jobs_router.get("/system-stats")
def system_stats(ctx: WorkspaceCtx = Depends(current_workspace)):
    from apps.api.services.leadgen.proxy_pool import ProxyPool
    from apps.api.services.leadgen.rate_limiter import RateLimiter

    pp = ProxyPool()
    rl = RateLimiter()
    db = _get_db()
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
    if not token:
        raise HTTPException(status_code=401, detail="Missing token")
    from jose import jwt, JWTError
    from apps.api.core.config import settings as _settings
    from apps.api.database import SessionLocal
    from apps.api.models import User as _User

    try:
        payload = jwt.decode(token, _settings.SECRET_KEY, algorithms=[_settings.ALGORITHM])
        username = payload.get("sub")
        if not username:
            raise HTTPException(status_code=401, detail="Invalid token")
    except JWTError:
        raise HTTPException(status_code=401, detail="Invalid token")

    _db = SessionLocal()
    try:
        user = _db.query(_User).filter(_User.username == username).first()
        if not user or not user.is_active:
            raise HTTPException(status_code=401, detail="Invalid token")
        return user
    finally:
        _db.close()


@events_router.get("/api/events")
def sse_events(token: Optional[str] = Query(default=None)):
    _authenticate_query_token(token)
    from apps.api.services.leadgen.progress import progress

    def stream():
        q = progress.subscribe()
        try:
            while True:
                if q:
                    while q:
                        event = q.popleft()
                        yield f"data: {json.dumps(event)}\n\n"
                else:
                    yield f"data: {json.dumps({'type': 'heartbeat'})}\n\n"
                _time.sleep(0.5)
        finally:
            progress.unsubscribe(q)

    return StreamingResponse(
        stream(),
        media_type="text/event-stream",
        headers={"Cache-Control": "no-cache", "X-Accel-Buffering": "no"},
    )

# ── Search & Scraper (authenticated) ─────────────────────────

@search_router.get("/v2/search/unified")
async def public_unified_search(
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
async def public_scrape(body: dict, ctx: WorkspaceCtx = Depends(current_workspace)):
    """Scrape a single URL (auth required). NOTE: SSRF allowlist still TODO."""
    url = body.get("url", "").strip()
    if not url:
        raise HTTPException(status_code=400, detail="url is required")
    from apps.api.services.scraper import UniversalScraper
    scraper = UniversalScraper()
    try:
        result = await scraper.scrape(url)
        return result
    except Exception as e:
        return {"url": url, "status": "error", "error": str(e)}


# ── Deduplication ─────────────────────────────────────────────────────────

@router.post("/leads/dedup")
def run_dedup(threshold: float = Query(0.85, ge=0.5, le=1.0), ctx: WorkspaceCtx = Depends(current_workspace)):
    """Analyze leads for duplicates using fuzzy matching."""
    from apps.api.services.dedup import LeadDeduplicator

    db = ctx.lead_db()
    leads = db.get_all()  # Returns list of dicts
    if not leads:
        return {"stats": {"total_leads": 0, "duplicates_found": 0}, "pairs": [], "clusters": {}}

    dedup = LeadDeduplicator(threshold=threshold)
    result = dedup.find_duplicates(leads)
    return result


@router.post("/leads/dedup/merge")
def merge_duplicates(body: dict, ctx: WorkspaceCtx = Depends(current_workspace)):
    """Merge duplicate leads — keep master, delete duplicates."""
    from apps.api.services.dedup import LeadDeduplicator

    master_id = body.get("master_id")
    duplicate_ids = body.get("duplicate_ids", [])
    if not master_id or not duplicate_ids:
        raise HTTPException(400, "master_id and duplicate_ids required")

    db = ctx.lead_db()
    master = db.get(master_id)
    if not master:
        raise HTTPException(404, f"Master lead {master_id} not found")

    duplicates = [db.get(did) for did in duplicate_ids if db.get(did)]
    if not duplicates:
        raise HTTPException(404, "No valid duplicate leads found")

    dedup = LeadDeduplicator()
    merged = dedup.merge_leads(master, duplicates)

    # Update master with merged data
    db.update(master_id, merged)

    # Delete duplicates
    deleted = 0
    for dup in duplicates:
        dup_id = dup.get("id")
        if dup_id:
            db.delete(dup_id)
            deleted += 1

    return {
        "status": "merged",
        "master_id": master_id,
        "duplicates_deleted": deleted,
        "merged_lead": merged,
    }



# ── Data Collector Import ─────────────────────────────────────────────────

@router.post("/leads/import/data-collector")
def import_data_collector(
    module: str = Query("all", description="Module to import: cnpj, github, or all"),
    limit: Optional[int] = Query(None, description="Max leads to import (for testing)"),
    reset: bool = Query(False, description="Reset checkpoint to start from scratch"),
    ctx: WorkspaceCtx = Depends(current_workspace),
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

    from apps.api.services.leadgen.scrapers.data_collector_import import (
        run_import_background, reset_checkpoint, _load_checkpoint,
    )

    if reset:
        reset_checkpoint()

    checkpoint = _load_checkpoint()
    job_id = run_import_background(module=module, limit=limit)

    return {
        "ok": True,
        "job_id": job_id,
        "module": module,
        "limit": limit,
        "resuming_from_row": checkpoint.get("cnpj_row", 0),
        "message": f"Import started in background (module={module}, limit={limit})",
    }


@router.get("/leads/import/data-collector/status")
def import_data_collector_status(ctx: WorkspaceCtx = Depends(current_workspace)):
    """Check the current checkpoint status of the data_collector import."""
    from apps.api.services.leadgen.scrapers.data_collector_import import (
        _load_checkpoint, CNPJ_CSV,
    )
    checkpoint = _load_checkpoint()
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
async def domain_intelligence(body: dict, ctx: WorkspaceCtx = Depends(current_workspace)):
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
