"""
Workbook API router — Hybrid model where workbooks are filtered views on the Leads DB.

Key principles:
  1. Leads DB is the source of truth — workbook rows ARE leads
  2. filter_criteria determines which leads appear in a workbook
  3. Enrichment of known Lead fields writes BACK to the Lead record
  4. AI/computed columns store results in WorkbookEnrichment overlay
  5. CSV import creates new leads in the DB
"""

from datetime import datetime
from typing import Optional
import json
import logging

from fastapi import APIRouter, Depends, HTTPException, WebSocket, WebSocketDisconnect, Query
from pydantic import BaseModel, Field
from sqlalchemy.orm import Session

from apps.api.database import get_db
from apps.api.services.workbook.models import (
    Workbook, WorkbookEnrichment, COLUMN_TYPES, LEAD_FIELD_MAP,
)
from apps.api.services.workbook.schemas import (
    WorkbookCreate, WorkbookUpdate, WorkbookResponse,
    WorkbookListResponse, WorkbookWithLeadsResponse,
    WorkbookLeadRow, EnrichmentOverlay,
    RunWorkbookRequest, RunWorkbookResponse,
    AddColumnRequest, ExportRequest,
)
from apps.api.services.leadgen.db import LeadDB

logger = logging.getLogger("workbook.api")
router = APIRouter(prefix="/api/workbooks", tags=["workbooks"])


# ── Helpers ───────────────────────────────────────────────────────────────

def _get_lead_db() -> LeadDB:
    """Get a LeadDB connection."""
    return LeadDB()


def _query_leads(db: LeadDB, filter_criteria: dict, page: int = 1, page_size: int = 100) -> tuple[list[dict], int]:
    """Query leads matching the workbook's filter criteria.

    Returns (leads_list, total_count).
    """
    fc = filter_criteria or {}

    # Build WHERE clauses
    conditions = []
    params = []

    if fc.get("city"):
        conditions.append("city = ?")
        params.append(fc["city"])
    if fc.get("state"):
        conditions.append("state = ?")
        params.append(fc["state"])
    if fc.get("score_tier"):
        conditions.append("score_tier = ?")
        params.append(fc["score_tier"])
    if fc.get("status"):
        conditions.append("status = ?")
        params.append(fc["status"])
    if fc.get("source"):
        conditions.append("source = ?")
        params.append(fc["source"])
    if fc.get("job_ids"):
        # Filter leads from multiple jobs: source IN ('job:xxx', 'job:yyy')
        job_sources = [f"job:{jid}" for jid in fc["job_ids"]]
        placeholders = ",".join("?" * len(job_sources))
        conditions.append(f"source IN ({placeholders})")
        params.extend(job_sources)
    if fc.get("specialization"):
        conditions.append("specialization LIKE ?")
        params.append(f"%{fc['specialization']}%")
    if fc.get("company_size"):
        conditions.append("company_size = ?")
        params.append(fc["company_size"])
    if fc.get("has_email") is True:
        conditions.append("email != '' AND email IS NOT NULL")
    elif fc.get("has_email") is False:
        conditions.append("(email = '' OR email IS NULL)")
    if fc.get("has_phone") is True:
        conditions.append("phone != '' AND phone IS NOT NULL")
    elif fc.get("has_phone") is False:
        conditions.append("(phone = '' OR phone IS NULL)")
    if fc.get("has_website") is True:
        conditions.append("website != '' AND website IS NOT NULL")
    elif fc.get("has_website") is False:
        conditions.append("(website = '' OR website IS NULL)")
    if fc.get("min_score") is not None:
        conditions.append("score >= ?")
        params.append(fc["min_score"])
    if fc.get("max_score") is not None:
        conditions.append("score <= ?")
        params.append(fc["max_score"])

    where_clause = " AND ".join(conditions) if conditions else "1=1"

    # Full-text search
    if fc.get("search"):
        # Use FTS
        fts_query = fc["search"].replace('"', '""')
        where_clause = f"id IN (SELECT rowid FROM leads_fts WHERE leads_fts MATCH '\"{fts_query}\"') AND {where_clause}"

    # Count
    count_sql = f"SELECT COUNT(*) FROM leads WHERE {where_clause}"
    total = db.conn.execute(count_sql, params).fetchone()[0]

    # Fetch page
    offset = (page - 1) * page_size
    select_sql = f"SELECT * FROM leads WHERE {where_clause} ORDER BY score DESC LIMIT ? OFFSET ?"
    rows = db.conn.execute(select_sql, params + [page_size, offset]).fetchall()

    leads = [dict(row) for row in rows]
    return leads, total


def _workbook_response(wb: Workbook, lead_db: LeadDB = None) -> WorkbookResponse:
    """Build a WorkbookResponse, computing total_rows from the filter."""
    total = 0
    if lead_db:
        try:
            _, total = _query_leads(lead_db, wb.filter_criteria or {}, page=1, page_size=1)
        except Exception:
            pass

    return WorkbookResponse(
        id=wb.id,
        name=wb.name,
        description=wb.description or "",
        status=wb.status or "draft",
        filter_criteria=wb.filter_criteria,
        columns_config=wb.columns_config or [],
        total_rows=total,
        completed_rows=wb.completed_rows or 0,
        created_at=wb.created_at,
        updated_at=wb.updated_at,
        last_run_at=wb.last_run_at,
    )


# ── CRUD ──────────────────────────────────────────────────────────────────

@router.get("/", response_model=WorkbookListResponse)
async def list_workbooks(db: Session = Depends(get_db)):
    """List all workbooks with live lead counts."""
    workbooks = db.query(Workbook).order_by(Workbook.updated_at.desc()).all()
    lead_db = _get_lead_db()
    try:
        result = [_workbook_response(wb, lead_db) for wb in workbooks]
    finally:
        lead_db.close()
    return WorkbookListResponse(workbooks=result, total=len(result))


@router.post("/", response_model=WorkbookResponse, status_code=201)
async def create_workbook(body: WorkbookCreate, db: Session = Depends(get_db)):
    """Create a new workbook with filter criteria."""
    wb = Workbook(
        name=body.name,
        description=body.description,
        filter_criteria=body.filter_criteria.model_dump(exclude_none=True) if body.filter_criteria else {},
        columns_config=[c.model_dump(exclude_none=True) for c in body.columns_config],
    )
    db.add(wb)
    db.commit()
    db.refresh(wb)

    lead_db = _get_lead_db()
    try:
        return _workbook_response(wb, lead_db)
    finally:
        lead_db.close()


class CreateFromJobsRequest(BaseModel):
    """Create or merge a workbook from one or more collection jobs."""
    job_ids: list[str] = Field(..., min_length=1, description="Job IDs to include")
    workbook_id: Optional[str] = Field(None, description="Existing workbook ID to merge into (creates new if omitted)")
    name: Optional[str] = Field(None, description="Workbook name (auto-generated if omitted)")


@router.post("/from-jobs", response_model=WorkbookResponse, status_code=201)
async def create_workbook_from_jobs(body: CreateFromJobsRequest, db: Session = Depends(get_db)):
    """Create a workbook from job results, or merge new jobs into an existing workbook.

    The workbook filter_criteria uses job_ids to show leads from those specific jobs.
    If workbook_id is provided, the new job IDs are merged into the existing workbook's filter.
    """

    if body.workbook_id:
        # ── Merge into existing workbook ──
        wb = db.query(Workbook).filter(Workbook.id == body.workbook_id).first()
        if not wb:
            raise HTTPException(status_code=404, detail="Workbook not found")

        # Merge job_ids into existing filter
        fc = wb.filter_criteria or {}
        existing_job_ids = set(fc.get("job_ids", []))
        existing_job_ids.update(body.job_ids)
        fc["job_ids"] = sorted(existing_job_ids)
        wb.filter_criteria = fc
        wb.updated_at = datetime.utcnow()
        db.commit()
        db.refresh(wb)
    else:
        # ── Create new workbook ──
        # Auto-generate name from job queries
        auto_name = body.name
        if not auto_name:
            lead_db = _get_lead_db()
            try:
                job_queries = []
                for jid in body.job_ids[:3]:
                    job = lead_db.conn.execute(
                        "SELECT query FROM jobs WHERE id = ?", (jid,)
                    ).fetchone()
                    if job:
                        job_queries.append(job[0])
                if job_queries:
                    auto_name = " + ".join(job_queries)[:120]
                else:
                    auto_name = f"Task Workbook ({len(body.job_ids)} jobs)"
            finally:
                lead_db.close()

        # Default columns: comprehensive lead view
        default_columns = [
            {"id": "company", "name": "Company", "type": "lead_field", "lead_field": "company", "width": 220},
            {"id": "specialization", "name": "Specialization", "type": "lead_field", "lead_field": "specialization", "width": 180},
            {"id": "contact_person", "name": "Contact", "type": "lead_field", "lead_field": "contact_person", "width": 180},
            {"id": "contact_title", "name": "Title", "type": "lead_field", "lead_field": "contact_title", "width": 150},
            {"id": "email", "name": "Email", "type": "lead_field", "lead_field": "email", "width": 220},
            {"id": "phone", "name": "Phone", "type": "lead_field", "lead_field": "phone", "width": 160},
            {"id": "website", "name": "Website", "type": "lead_field", "lead_field": "website", "width": 180},
            {"id": "linkedin_url", "name": "LinkedIn", "type": "lead_field", "lead_field": "linkedin_url", "width": 180},
            {"id": "city", "name": "City", "type": "lead_field", "lead_field": "city", "width": 130},
            {"id": "state", "name": "State", "type": "lead_field", "lead_field": "state", "width": 120},
            {"id": "company_size", "name": "Size", "type": "lead_field", "lead_field": "company_size", "width": 100},
            {"id": "industry_tags", "name": "Industry", "type": "lead_field", "lead_field": "industry_tags", "width": 180},
            {"id": "description", "name": "Description", "type": "lead_field", "lead_field": "description", "width": 250},
            {"id": "score", "name": "Score", "type": "lead_field", "lead_field": "score", "width": 80},
            {"id": "status", "name": "Status", "type": "lead_field", "lead_field": "status", "width": 100},
            {"id": "source", "name": "Source", "type": "lead_field", "lead_field": "source", "width": 120},
            {"id": "notes", "name": "Notes", "type": "lead_field", "lead_field": "notes", "width": 200},
            {"id": "created_at", "name": "Created", "type": "lead_field", "lead_field": "created_at", "width": 140},
        ]

        wb = Workbook(
            name=auto_name,
            description=f"Created from {len(body.job_ids)} chat task(s)",
            filter_criteria={"job_ids": body.job_ids},
            columns_config=default_columns,
        )
        db.add(wb)
        db.commit()
        db.refresh(wb)

    lead_db = _get_lead_db()
    try:
        return _workbook_response(wb, lead_db)
    finally:
        lead_db.close()


@router.get("/{workbook_id}", response_model=WorkbookWithLeadsResponse)
async def get_workbook(
    workbook_id: str,
    page: int = Query(1, ge=1),
    page_size: int = Query(100, ge=1, le=1000),
    db: Session = Depends(get_db),
):
    """Get workbook with paginated lead rows + enrichment overlay."""
    wb = db.query(Workbook).filter(Workbook.id == workbook_id).first()
    if not wb:
        raise HTTPException(status_code=404, detail="Workbook not found")

    lead_db = _get_lead_db()
    try:
        leads, total = _query_leads(lead_db, wb.filter_criteria or {}, page, page_size)
    finally:
        lead_db.close()

    # Load enrichment overlay for these leads
    lead_ids = [l["id"] for l in leads]
    enrichments = db.query(WorkbookEnrichment).filter(
        WorkbookEnrichment.workbook_id == workbook_id,
        WorkbookEnrichment.lead_id.in_(lead_ids),
    ).all() if lead_ids else []

    # Build enrichment lookup: {lead_id: {col_id: overlay}}
    enrich_map: dict[int, dict[str, EnrichmentOverlay]] = {}
    for e in enrichments:
        if e.lead_id not in enrich_map:
            enrich_map[e.lead_id] = {}
        enrich_map[e.lead_id][e.column_id] = EnrichmentOverlay(
            value=e.value,
            status=e.status or "pending",
            provider=e.provider,
            error=e.error,
        )

    # Build rows
    rows = []
    for lead in leads:
        rows.append(WorkbookLeadRow(
            lead_id=lead["id"],
            lead=lead,
            enrichments=enrich_map.get(lead["id"], {}),
        ))

    return WorkbookWithLeadsResponse(
        workbook=_workbook_response(wb),
        rows=rows,
        total_rows=total,
        page=page,
        page_size=page_size,
    )


@router.put("/{workbook_id}", response_model=WorkbookResponse)
async def update_workbook(workbook_id: str, body: WorkbookUpdate, db: Session = Depends(get_db)):
    """Update workbook metadata, filter, or columns."""
    wb = db.query(Workbook).filter(Workbook.id == workbook_id).first()
    if not wb:
        raise HTTPException(status_code=404, detail="Workbook not found")

    if body.name is not None:
        wb.name = body.name
    if body.description is not None:
        wb.description = body.description
    if body.status is not None:
        wb.status = body.status
    if body.filter_criteria is not None:
        wb.filter_criteria = body.filter_criteria.model_dump(exclude_none=True)
    if body.columns_config is not None:
        wb.columns_config = [c.model_dump(exclude_none=True) for c in body.columns_config]

    db.commit()
    db.refresh(wb)

    lead_db = _get_lead_db()
    try:
        return _workbook_response(wb, lead_db)
    finally:
        lead_db.close()


@router.delete("/{workbook_id}")
async def delete_workbook(workbook_id: str, db: Session = Depends(get_db)):
    """Delete a workbook and its enrichment overlay data."""
    wb = db.query(Workbook).filter(Workbook.id == workbook_id).first()
    if not wb:
        raise HTTPException(status_code=404, detail="Workbook not found")
    db.delete(wb)
    db.commit()
    return {"status": "deleted"}


# ── Lead Field Update (edit a lead from workbook context) ──────────────────

@router.put("/{workbook_id}/leads/{lead_id}")
async def update_lead_field(workbook_id: str, lead_id: int, body: dict):
    """Update a Lead's field from the workbook context.

    Writes directly to the leads DB (source of truth).
    Body: {"field": "value", ...}
    """
    lead_db = _get_lead_db()
    try:
        # Only allow updating known lead fields
        updates = {k: v for k, v in body.items() if k in LEAD_FIELD_MAP}
        if not updates:
            raise HTTPException(status_code=400, detail="No valid lead fields to update")

        set_clause = ", ".join(f"{k} = ?" for k in updates)
        values = list(updates.values()) + [lead_id]
        lead_db.conn.execute(
            f"UPDATE leads SET {set_clause}, updated_at = ? WHERE id = ?",
            list(updates.values()) + [datetime.utcnow().isoformat(), lead_id],
        )
        lead_db.conn.commit()
        return {"status": "updated", "lead_id": lead_id, "fields": list(updates.keys())}
    finally:
        lead_db.close()


# ── CSV Import (creates leads + adds to workbook filter) ──────────────────

@router.post("/{workbook_id}/import")
async def import_csv_leads(workbook_id: str, body: dict, db: Session = Depends(get_db)):
    """Import rows as new leads in the DB.

    Body: {"rows": [{"company": "Acme", "website": "acme.com", ...}, ...]}
    The workbook's filter should match the imported leads.
    """
    wb = db.query(Workbook).filter(Workbook.id == workbook_id).first()
    if not wb:
        raise HTTPException(status_code=404, detail="Workbook not found")

    rows = body.get("rows", [])
    if not rows:
        raise HTTPException(status_code=400, detail="No rows provided")

    lead_db = _get_lead_db()
    created = 0
    try:
        for row in rows:
            from apps.api.services.leadgen.models import Lead
            lead = Lead.from_dict(row)
            # Tag with workbook source
            if not lead.source:
                lead.source = f"workbook:{workbook_id}"
            try:
                lead_db.upsert_lead(lead)
                created += 1
            except Exception as e:
                logger.warning(f"Failed to import lead: {e}")
    finally:
        lead_db.close()

    return {"created": created, "total_rows": created}


# ── Column Management ────────────────────────────────────────────────────

@router.post("/{workbook_id}/columns", response_model=WorkbookResponse)
async def add_column(workbook_id: str, body: AddColumnRequest, db: Session = Depends(get_db)):
    """Add a new column to the workbook."""
    wb = db.query(Workbook).filter(Workbook.id == workbook_id).first()
    if not wb:
        raise HTTPException(status_code=404, detail="Workbook not found")

    cols = list(wb.columns_config or [])
    cols.append(body.column.model_dump(exclude_none=True))
    wb.columns_config = cols
    db.commit()
    db.refresh(wb)
    return _workbook_response(wb)


@router.delete("/{workbook_id}/columns/{column_id}", response_model=WorkbookResponse)
async def remove_column(workbook_id: str, column_id: str, db: Session = Depends(get_db)):
    """Remove a column and its enrichment data."""
    wb = db.query(Workbook).filter(Workbook.id == workbook_id).first()
    if not wb:
        raise HTTPException(status_code=404, detail="Workbook not found")

    wb.columns_config = [c for c in (wb.columns_config or []) if c.get("id") != column_id]

    # Clean up enrichment overlay data
    db.query(WorkbookEnrichment).filter(
        WorkbookEnrichment.workbook_id == workbook_id,
        WorkbookEnrichment.column_id == column_id,
    ).delete()

    db.commit()
    db.refresh(wb)
    return _workbook_response(wb)


# ── Run Enrichment ────────────────────────────────────────────────────────

@router.post("/{workbook_id}/run", response_model=RunWorkbookResponse)
async def run_workbook(workbook_id: str, body: RunWorkbookRequest = None, db: Session = Depends(get_db)):
    """Run enrichment on workbook leads."""
    if body is None:
        body = RunWorkbookRequest()

    wb = db.query(Workbook).filter(Workbook.id == workbook_id).first()
    if not wb:
        raise HTTPException(status_code=404, detail="Workbook not found")

    columns = wb.columns_config or []
    enrichment_cols = [
        c for c in columns
        if c.get("type") in ("enrichment", "waterfall", "ai_formula")
        and (body.column_ids is None or c.get("id") in body.column_ids)
    ]

    if not enrichment_cols:
        return RunWorkbookResponse(status="skipped", total_jobs=0, message="No enrichment columns to run")

    # Get leads matching filter
    lead_db = _get_lead_db()
    try:
        leads, total = _query_leads(lead_db, wb.filter_criteria or {}, page=1, page_size=10000)
    finally:
        lead_db.close()

    if body.lead_ids:
        leads = [l for l in leads if l["id"] in body.lead_ids]

    if not leads:
        return RunWorkbookResponse(status="skipped", total_jobs=0, message="No leads to process")

    # Update workbook status
    wb.status = "running"
    wb.last_run_at = datetime.utcnow()
    db.commit()

    total_jobs = len(leads) * len(enrichment_cols)

    # Try BullMQ, fall back to inline
    try:
        from apps.api.services.workbook.worker import enqueue_enrichment_job
        enqueued = 0
        for lead in leads:
            for col in enrichment_cols:
                chain = col.get("waterfall") or ([col.get("provider")] if col.get("provider") else [])
                success = await enqueue_enrichment_job(
                    workbook_id=workbook_id,
                    row_id=lead["id"],  # lead_id
                    col_id=col["id"],
                    provider_chain=chain,
                )
                if success:
                    enqueued += 1
        return RunWorkbookResponse(
            status="started",
            total_jobs=enqueued,
            message=f"Enqueued {enqueued} enrichment jobs",
        )
    except Exception as e:
        # Inline fallback
        from apps.api.services.workbook.enrichment import enrich_workbook_leads
        result = await enrich_workbook_leads(
            db=db,
            workbook_id=workbook_id,
            leads=leads,
            columns=enrichment_cols,
            columns_config=wb.columns_config or [],
        )
        wb.status = "complete"
        db.commit()
        return RunWorkbookResponse(
            status="complete",
            total_jobs=result.get("total", 0),
            message=f"Inline: {result.get('completed', 0)} completed, {result.get('errors', 0)} errors",
        )


@router.post("/{workbook_id}/stop")
async def stop_workbook(workbook_id: str, db: Session = Depends(get_db)):
    """Stop a running workbook."""
    wb = db.query(Workbook).filter(Workbook.id == workbook_id).first()
    if not wb:
        raise HTTPException(status_code=404, detail="Workbook not found")
    wb.status = "paused"
    db.commit()
    return {"status": "paused"}


# ── WebSocket ─────────────────────────────────────────────────────────────

@router.websocket("/{workbook_id}/ws")
async def workbook_websocket(websocket: WebSocket, workbook_id: str):
    """WebSocket for live enrichment updates."""
    await websocket.accept()
    try:
        import redis.asyncio as aioredis
        redis_client = await aioredis.from_url("redis://localhost:6379")
        pubsub = redis_client.pubsub()
        await pubsub.subscribe(f"workbook:{workbook_id}")

        import asyncio
        async def listen_redis():
            async for message in pubsub.listen():
                if message["type"] == "message":
                    await websocket.send_text(message["data"].decode())

        redis_task = asyncio.create_task(listen_redis())
        try:
            while True:
                data = await websocket.receive_text()
                msg = json.loads(data)
                if msg.get("type") == "ping":
                    await websocket.send_json({"type": "pong"})
        except WebSocketDisconnect:
            pass
        finally:
            redis_task.cancel()
            await pubsub.unsubscribe(f"workbook:{workbook_id}")
            await redis_client.close()
    except Exception:
        # Redis not available — simple echo mode
        try:
            while True:
                data = await websocket.receive_text()
                msg = json.loads(data)
                if msg.get("type") == "ping":
                    await websocket.send_json({"type": "pong"})
        except WebSocketDisconnect:
            pass


# ── Meta ──────────────────────────────────────────────────────────────────

@router.get("/meta/column-types")
async def get_column_types():
    """Get available column types."""
    return COLUMN_TYPES


@router.get("/meta/lead-fields")
async def get_lead_fields():
    """Get available Lead fields for column mapping."""
    return {"fields": list(LEAD_FIELD_MAP.keys())}


@router.get("/meta/providers")
async def get_providers():
    """Get available enrichment providers."""
    from apps.api.services.workbook.providers import list_providers
    providers = list_providers()
    return {"providers": [
        {"name": p["name"], "capabilities": p.get("capabilities", []), "confidence": p.get("confidence", 0.5)}
        for p in providers
    ]}


@router.get("/meta/filter-options")
async def get_filter_options():
    """Get available filter values from the leads DB."""
    lead_db = _get_lead_db()
    try:
        cities = [r[0] for r in lead_db.conn.execute(
            "SELECT DISTINCT city FROM leads WHERE city != '' ORDER BY city"
        ).fetchall()]
        tiers = [r[0] for r in lead_db.conn.execute(
            "SELECT DISTINCT score_tier FROM leads WHERE score_tier != '' ORDER BY score_tier"
        ).fetchall()]
        sources = [r[0] for r in lead_db.conn.execute(
            "SELECT DISTINCT source FROM leads WHERE source != '' ORDER BY source"
        ).fetchall()]
        statuses = [r[0] for r in lead_db.conn.execute(
            "SELECT DISTINCT status FROM leads WHERE status != '' ORDER BY status"
        ).fetchall()]
        specializations = [r[0] for r in lead_db.conn.execute(
            "SELECT DISTINCT specialization FROM leads WHERE specialization != '' ORDER BY specialization LIMIT 50"
        ).fetchall()]
        total_leads = lead_db.conn.execute("SELECT COUNT(*) FROM leads").fetchone()[0]

        return {
            "cities": cities,
            "tiers": tiers,
            "sources": sources,
            "statuses": statuses,
            "specializations": specializations,
            "total_leads": total_leads,
        }
    finally:
        lead_db.close()
