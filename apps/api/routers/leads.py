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

from fastapi import APIRouter, Query, HTTPException, Request
from fastapi.responses import StreamingResponse, Response
from pydantic import BaseModel

from apps.api.services.leadgen.db import LeadDB
from apps.api.services.leadgen.models import Lead, LEAD_STATUSES

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
):
    db = _get_db()
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
def lead_stats():
    db = _get_db()
    stats = db.get_stats()
    db.close()
    return stats


@router.get("/filters")
# Mounted at /api/filters — matches frontend expectation
def lead_filters():
    db = _get_db()
    data = {
        "cities": db.get_cities(),
        "sources": db.get_sources(),
        "statuses": LEAD_STATUSES,
        "tiers": ["hot", "warm", "cold", "unqualified"],
    }
    db.close()
    return data


@router.get("/lead/{lead_id}")
def get_lead(lead_id: int):
    db = _get_db()
    lead = db.get_lead(lead_id)
    db.close()
    if lead:
        return lead.to_dict()
    raise HTTPException(status_code=404, detail="Lead not found")


class StatusUpdate(BaseModel):
    status: str
    note: str = ""


@router.post("/lead/{lead_id}/status")
def update_lead_status(lead_id: int, body: StatusUpdate):
    db = _get_db()
    db.update_status(lead_id, body.status, body.note)
    db.close()
    return {"ok": True}


@router.put("/lead/{lead_id}")
def update_lead(lead_id: int, body: dict):
    db = _get_db()
    db.update_lead_fields(lead_id, body)
    db.close()
    return {"ok": True}


@router.delete("/lead/{lead_id}")
def delete_lead(lead_id: int):
    db = _get_db()
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
def add_lead(body: AddLeadRequest):
    lead = Lead.from_dict(body.model_dump())
    lead.source = body.source
    db = _get_db()
    lead_id = db.upsert_lead(lead)
    db.close()
    return {"ok": True, "id": lead_id}


# ── CSV Export ─────────────────────────────────────────────────


@router.get("/export/csv")
# Mounted at /api/export/csv — matches frontend expectation
def export_csv(
    status: Optional[str] = None,
    city: Optional[str] = None,
    tier: Optional[str] = None,
    score_min: Optional[int] = None,
):
    db = _get_db()
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
        "Company", "Website", "Email", "Phone", "City", "Specialization",
        "Score", "Tier", "Status", "Source", "LinkedIn", "Contact", "Notes",
    ])
    for l in leads:
        writer.writerow([
            l.company, l.website, l.email, l.phone, l.city,
            l.specialization, l.score, l.score_tier, l.status,
            l.source, l.linkedin_url, l.contact_person, l.notes,
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
def start_collection(body: CollectRequest):
    query = body.query.strip()
    if not query:
        raise HTTPException(status_code=400, detail="query is required")

    job_id = str(uuid.uuid4())[:8]
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
def list_jobs(status: Optional[str] = None):
    db = _get_db()
    jobs = db.get_jobs(status=_clean(status))
    db.close()
    return jobs


@jobs_router.get("/system-stats")
def system_stats():
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
def list_workspaces():
    db = _get_db()
    workspaces = db.get_workspaces()
    db.close()
    return workspaces


class CreateWorkspaceRequest(BaseModel):
    name: str
    description: str = ""


@workspace_router.post("")
def create_workspace(body: CreateWorkspaceRequest):
    name = body.name.strip()
    if not name:
        raise HTTPException(status_code=400, detail="name is required")
    db = _get_db()
    ws_id = db.create_workspace(name, body.description)
    db.close()
    return {"ok": True, "id": ws_id, "name": name}


@workspace_router.delete("/{ws_id}")
def delete_workspace(ws_id: str):
    db = _get_db()
    db.delete_workspace(ws_id)
    db.close()
    return {"ok": True}


# ── SSE Events ─────────────────────────────────────────────────


@events_router.get("/api/events")
def sse_events():
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

# ── Public Search & Scraper (no auth) ─────────────────────────

@search_router.get("/v2/search/unified")
async def public_unified_search(
    q: str = "",
    sources: str = "",
    limit: int = 20,
    sort: str = "relevance",
    file_type: str = "",
):
    """Public endpoint — unified multi-source document search."""
    if not q:
        return {"query": "", "total": 0, "results": []}
    from apps.api.sources.registry import SourceRegistry
    registry = SourceRegistry()
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
async def public_scrape(body: dict):
    """Public endpoint — scrape a single URL."""
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


@search_router.post("/v2/person-intel/search")
async def public_person_search(body: dict):
    """Public endpoint — person intelligence search."""
    query = body.get("query", "").strip()
    if not query:
        return {"results": []}
    from apps.api.services.person_intel import PersonIntelService
    service = PersonIntelService()
    try:
        results = await service.search(query, max_results=body.get("max_results", 10))
        return {"results": results}
    except Exception as e:
        return {"results": [], "error": str(e)}

