"""Workbook API router — Clay-style self-contained tables with WorkbookRow."""

from datetime import datetime, timezone
from typing import Optional
import json
import logging

from fastapi import APIRouter, Depends, HTTPException, WebSocket, WebSocketDisconnect, Query
from pydantic import BaseModel, Field
from sqlalchemy.orm import Session
from sqlalchemy import func as sa_func

from apps.api.database import get_db
from apps.api.services.workbook.models import (
    Workbook, WorkbookEnrichment, WorkbookRow, COLUMN_TYPES, LEAD_FIELD_MAP,
)
from apps.api.services.workbook.schemas import (
    WorkbookCreate, WorkbookUpdate, WorkbookResponse,
    WorkbookListResponse, WorkbookWithLeadsResponse,
    WorkbookLeadRow, EnrichmentOverlay,
    RunWorkbookRequest, RunWorkbookResponse,
    AddColumnRequest, ExportRequest,
    AddRowsRequest, DeleteRowsRequest,
)
from apps.api.services.leadgen.db import LeadDB
from apps.api.core.tenancy import WorkspaceCtx, current_workspace

logger = logging.getLogger("workbook.api")
router = APIRouter(prefix="/api/workbooks", tags=["workbooks"])


# ── Helpers ───────────────────────────────────────────────────────────────

def _get_lead_db() -> LeadDB:
    """Get a LeadDB connection (default/main workspace file)."""
    return LeadDB()


def _owned_workbook(db: Session, workbook_id: str, ctx: WorkspaceCtx) -> Workbook:
    """Fetch a workbook scoped to the caller's workspace.

    Returns 404 (not 403) for workbooks in other workspaces so we don't leak
    which ids exist outside the caller's tenant.
    """
    wb = db.query(Workbook).filter(Workbook.id == workbook_id).first()
    if not wb or wb.workspace_id != ctx.workspace_id:
        raise HTTPException(status_code=404, detail="Workbook not found")
    return wb


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
        # Use parameterized FTS query, wrapping in quotes to prevent syntax errors with special chars
        fts_query = fc["search"].replace('"', '""')
        where_clause = f"id IN (SELECT rowid FROM leads_fts WHERE leads_fts MATCH ?) AND {where_clause}"
        params.insert(0, f'"{fts_query}"')

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
    """Build a WorkbookResponse, computing total_rows from WorkbookRow or leads DB."""
    from apps.api.database import SessionLocal
    total = 0
    session = SessionLocal()
    try:
        row_count = session.query(sa_func.count(WorkbookRow.id)).filter(
            WorkbookRow.workbook_id == wb.id
        ).scalar() or 0
        if row_count > 0:
            total = row_count
        elif lead_db:
            try:
                _, total = _query_leads(lead_db, wb.filter_criteria or {}, page=1, page_size=1)
            except Exception:
                pass
    except Exception:
        total = wb.total_rows or 0
    finally:
        session.close()

    return WorkbookResponse(
        id=wb.id,
        name=wb.name,
        description=wb.description or "",
        status=wb.status or "draft",
        source_type=getattr(wb, 'source_type', None) or "leads_filter",
        source_config=getattr(wb, 'source_config', None) or {},
        filter_criteria=wb.filter_criteria,
        columns_config=wb.columns_config or [],
        total_rows=total,
        completed_rows=wb.completed_rows or 0,
        sync_to_leads=getattr(wb, 'sync_to_leads', True),
        created_at=wb.created_at,
        updated_at=wb.updated_at,
        last_run_at=wb.last_run_at,
    )


# ── CRUD ──────────────────────────────────────────────────────────────────

@router.get("/", response_model=WorkbookListResponse)
async def list_workbooks(
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    """List workbooks in the caller's active workspace, with live lead counts."""
    workbooks = (
        db.query(Workbook)
        .filter(Workbook.workspace_id == ctx.workspace_id)
        .order_by(Workbook.updated_at.desc())
        .all()
    )
    lead_db = ctx.lead_db()
    try:
        result = [_workbook_response(wb, lead_db) for wb in workbooks]
    finally:
        lead_db.close()
    return WorkbookListResponse(workbooks=result, total=len(result))


@router.post("/", response_model=WorkbookResponse, status_code=201)
async def create_workbook(
    body: WorkbookCreate,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    """Create a new workbook — Clay-style with source selection.

    source="empty": blank table
    source="leads_filter": snapshot leads matching filter into WorkbookRow
    source="csv": rows provided in source_config.rows
    source="job_results": snapshot leads from specific jobs
    """
    # Determine source type
    source = body.source or "empty"
    filter_criteria = body.filter_criteria.model_dump(exclude_none=True) if body.filter_criteria else {}
    source_config = body.source_config or {}

    # Legacy compat: if filter_criteria provided but source not set, treat as leads_filter
    if filter_criteria and source == "empty":
        source = "leads_filter"
        source_config = filter_criteria

    wb = Workbook(
        name=body.name,
        description=body.description,
        workspace_id=ctx.workspace_id,
        source_type=source,
        source_config=source_config,
        filter_criteria=filter_criteria,
        columns_config=[c.model_dump(exclude_none=True) for c in body.columns_config],
    )
    db.add(wb)
    db.commit()
    db.refresh(wb)

    # Snapshot rows based on source type
    max_rows = body.max_rows or 1000

    if source == "leads_filter":
        lead_db = ctx.lead_db()
        try:
            leads, total = _query_leads(lead_db, filter_criteria, page=1, page_size=max_rows)
            for i, lead in enumerate(leads):
                db.add(WorkbookRow(
                    workbook_id=wb.id,
                    position=i,
                    data=lead,
                    lead_id=lead.get("id"),
                    enrichments={},
                ))
            db.commit()
            logger.info(f"Snapshotted {len(leads)} leads into workbook {wb.id}")
        finally:
            lead_db.close()

    elif source == "csv" and source_config.get("rows"):
        for i, row in enumerate(source_config["rows"][:max_rows]):
            db.add(WorkbookRow(
                workbook_id=wb.id,
                position=i,
                data=row,
                enrichments={},
            ))
        db.commit()

    elif source == "job_results":
        job_ids = source_config.get("job_ids", [])
        if job_ids:
            lead_db = ctx.lead_db()
            try:
                leads, _ = _query_leads(lead_db, {"job_ids": job_ids}, page=1, page_size=max_rows)
                for i, lead in enumerate(leads):
                    db.add(WorkbookRow(
                        workbook_id=wb.id,
                        position=i,
                        data=lead,
                        lead_id=lead.get("id"),
                        enrichments={},
                    ))
                db.commit()
            finally:
                lead_db.close()

    # source="empty" → no rows created

    return _workbook_response(wb)


class CreateFromJobsRequest(BaseModel):
    """Create or merge a workbook from one or more collection jobs."""
    job_ids: list[str] = Field(..., min_length=1, description="Job IDs to include")
    workbook_id: Optional[str] = Field(None, description="Existing workbook ID to merge into (creates new if omitted)")
    name: Optional[str] = Field(None, description="Workbook name (auto-generated if omitted)")


@router.post("/from-jobs", response_model=WorkbookResponse, status_code=201)
async def create_workbook_from_jobs(
    body: CreateFromJobsRequest,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    """Create a workbook from job results, or merge new jobs into an existing workbook.

    The workbook filter_criteria uses job_ids to show leads from those specific jobs.
    If workbook_id is provided, the new job IDs are merged into the existing workbook's filter.
    """

    if body.workbook_id:
        # ── Merge into existing workbook ──
        wb = _owned_workbook(db, body.workbook_id, ctx)

        # Merge job_ids into existing filter
        fc = wb.filter_criteria or {}
        existing_job_ids = set(fc.get("job_ids", []))
        existing_job_ids.update(body.job_ids)
        fc["job_ids"] = sorted(existing_job_ids)
        wb.filter_criteria = fc
        wb.updated_at = datetime.now(timezone.utc)
        db.commit()
        db.refresh(wb)
    else:
        # ── Create new workbook ──
        # Auto-generate name from job queries
        auto_name = body.name
        if not auto_name:
            lead_db = ctx.lead_db()
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
            workspace_id=ctx.workspace_id,
            source_type="job_results",
            source_config={"job_ids": body.job_ids},
            filter_criteria={"job_ids": body.job_ids},
            columns_config=default_columns,
        )
        db.add(wb)
        db.commit()
        db.refresh(wb)

    # Snapshot leads into WorkbookRow
    lead_db = ctx.lead_db()
    try:
        fc = wb.filter_criteria or {}
        leads, _ = _query_leads(lead_db, fc, page=1, page_size=5000)
        # Get existing row count for position offset
        existing_count = db.query(sa_func.count(WorkbookRow.id)).filter(
            WorkbookRow.workbook_id == wb.id
        ).scalar() or 0
        for i, lead in enumerate(leads):
            # Skip if already exists (dedup by lead_id)
            exists = db.query(WorkbookRow.id).filter(
                WorkbookRow.workbook_id == wb.id,
                WorkbookRow.lead_id == lead.get("id"),
            ).first()
            if not exists:
                db.add(WorkbookRow(
                    workbook_id=wb.id,
                    position=existing_count + i,
                    data=lead,
                    lead_id=lead.get("id"),
                    enrichments={},
                ))
        db.commit()
        return _workbook_response(wb, lead_db)
    finally:
        lead_db.close()


@router.get("/{workbook_id}", response_model=WorkbookWithLeadsResponse)
async def get_workbook(
    workbook_id: str,
    page: int = Query(1, ge=1),
    page_size: int = Query(100, ge=1, le=5000),
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    """Get workbook with paginated rows. Uses WorkbookRow (v2) or falls back to leads DB."""
    wb = _owned_workbook(db, workbook_id, ctx)

    # ── v2: Read from WorkbookRow table ──
    v2_count = db.query(sa_func.count(WorkbookRow.id)).filter(
        WorkbookRow.workbook_id == workbook_id
    ).scalar() or 0

    if v2_count > 0:
        offset = (page - 1) * page_size
        wb_rows = db.query(WorkbookRow).filter(
            WorkbookRow.workbook_id == workbook_id
        ).order_by(WorkbookRow.position).offset(offset).limit(page_size).all()

        rows = []
        for r in wb_rows:
            enrichments_dict = {}
            for col_id, overlay in (r.enrichments or {}).items():
                if isinstance(overlay, dict):
                    enrichments_dict[col_id] = EnrichmentOverlay(**overlay)
                else:
                    enrichments_dict[col_id] = EnrichmentOverlay(value=overlay, status="complete")

            rows.append(WorkbookLeadRow(
                lead_id=r.lead_id or r.id,
                row_id=r.id,
                position=r.position,
                lead=r.data or {},
                data=r.data or {},
                enrichments=enrichments_dict,
                canonical_entity_id=r.canonical_entity_id,
                corroboration_count=r.corroboration_count,
            ))

        return WorkbookWithLeadsResponse(
            workbook=_workbook_response(wb),
            rows=rows,
            total_rows=v2_count,
            page=page,
            page_size=page_size,
        )

    # ── v1 Legacy: Read from leads DB ──
    lead_db = ctx.lead_db()
    try:
        leads, total = _query_leads(lead_db, wb.filter_criteria or {}, page, page_size)
    finally:
        lead_db.close()

    lead_ids = [l["id"] for l in leads]
    enrichments = db.query(WorkbookEnrichment).filter(
        WorkbookEnrichment.workbook_id == workbook_id,
        WorkbookEnrichment.lead_id.in_(lead_ids),
    ).all() if lead_ids else []

    enrich_map: dict[int, dict[str, EnrichmentOverlay]] = {}
    for e in enrichments:
        if e.lead_id not in enrich_map:
            enrich_map[e.lead_id] = {}
        enrich_map[e.lead_id][e.column_id] = EnrichmentOverlay(
            value=e.value, status=e.status or "pending",
            provider=e.provider, error=e.error,
        )

    rows = []
    for lead in leads:
        rows.append(WorkbookLeadRow(
            lead_id=lead["id"], lead=lead, data=lead,
            enrichments=enrich_map.get(lead["id"], {}),
        ))

    return WorkbookWithLeadsResponse(
        workbook=_workbook_response(wb),
        rows=rows, total_rows=total,
        page=page, page_size=page_size,
    )


@router.put("/{workbook_id}", response_model=WorkbookResponse)
async def update_workbook(
    workbook_id: str,
    body: WorkbookUpdate,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    """Update workbook metadata, filter, or columns."""
    wb = _owned_workbook(db, workbook_id, ctx)

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

    lead_db = ctx.lead_db()
    try:
        return _workbook_response(wb, lead_db)
    finally:
        lead_db.close()


@router.delete("/{workbook_id}")
async def delete_workbook(
    workbook_id: str,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    """Delete a workbook and its enrichment overlay data."""
    wb = _owned_workbook(db, workbook_id, ctx)
    db.delete(wb)
    db.commit()
    return {"status": "deleted"}


# ── Lead Field Update (edit a lead from workbook context) ──────────────────

@router.put("/{workbook_id}/leads/{lead_id}")
async def update_lead_field(
    workbook_id: str,
    lead_id: int,
    body: dict,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    """Update a Lead's field from the workbook context.

    Writes directly to the leads DB (source of truth).
    Body: {"field": "value", ...}
    """
    _owned_workbook(db, workbook_id, ctx)  # authorize: workbook must be in caller's workspace
    lead_db = ctx.lead_db()
    try:
        # Only allow updating known lead fields
        updates = {k: v for k, v in body.items() if k in LEAD_FIELD_MAP}
        if not updates:
            raise HTTPException(status_code=400, detail="No valid lead fields to update")

        set_clause = ", ".join(f"{k} = ?" for k in updates)
        values = list(updates.values()) + [lead_id]
        lead_db.conn.execute(
            f"UPDATE leads SET {set_clause}, updated_at = ? WHERE id = ?",
            list(updates.values()) + [datetime.now(timezone.utc).isoformat(), lead_id],
        )
        lead_db.conn.commit()
        return {"status": "updated", "lead_id": lead_id, "fields": list(updates.keys())}
    finally:
        lead_db.close()


# ── CSV Import (creates leads + adds to workbook filter) ──────────────────

@router.post("/{workbook_id}/import")
async def import_csv_leads(
    workbook_id: str,
    body: dict,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    """Import rows as new leads in the DB.

    Body: {"rows": [{"company": "Acme", "website": "acme.com", ...}, ...]}
    The workbook's filter should match the imported leads.
    """
    wb = _owned_workbook(db, workbook_id, ctx)

    rows = body.get("rows", [])
    if not rows:
        raise HTTPException(status_code=400, detail="No rows provided")

    lead_db = ctx.lead_db()
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
async def add_column(
    workbook_id: str,
    body: AddColumnRequest,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    """Add a new column to the workbook."""
    wb = _owned_workbook(db, workbook_id, ctx)

    cols = list(wb.columns_config or [])
    cols.append(body.column.model_dump(exclude_none=True))
    wb.columns_config = cols
    db.commit()
    db.refresh(wb)
    return _workbook_response(wb)


@router.delete("/{workbook_id}/columns/{column_id}", response_model=WorkbookResponse)
async def remove_column(
    workbook_id: str,
    column_id: str,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    """Remove a column and its enrichment data."""
    wb = _owned_workbook(db, workbook_id, ctx)

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
async def run_workbook(
    workbook_id: str,
    body: RunWorkbookRequest = None,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    """Run enrichment on workbook rows (v2: WorkbookRow, v1 fallback: leads DB)."""
    if body is None:
        body = RunWorkbookRequest()

    wb = _owned_workbook(db, workbook_id, ctx)

    columns = wb.columns_config or []
    enrichment_cols = [
        c for c in columns
        if c.get("type") in ("enrichment", "waterfall", "ai_formula", "output", "research", "agent")
        and (body.column_ids is None or c.get("id") in body.column_ids)
    ]
    # Output columns push the (enriched) row somewhere, so run them last.
    enrichment_cols.sort(key=lambda c: 1 if c.get("type") == "output" else 0)

    if not enrichment_cols:
        return RunWorkbookResponse(status="skipped", total_jobs=0, message="No enrichment columns to run")

    # ── Get rows from WorkbookRow (v2) or leads DB (v1) ──
    v2_count = db.query(sa_func.count(WorkbookRow.id)).filter(
        WorkbookRow.workbook_id == workbook_id
    ).scalar() or 0

    if v2_count > 0:
        # v2: read from WorkbookRow
        query = db.query(WorkbookRow).filter(WorkbookRow.workbook_id == workbook_id)
        if body.row_ids:
            query = query.filter(WorkbookRow.id.in_(body.row_ids))
        elif body.lead_ids:
            query = query.filter(WorkbookRow.lead_id.in_(body.lead_ids))
        wb_rows = query.all()
        leads = [{"id": r.lead_id or r.id, **r.data} for r in wb_rows]
    else:
        # v1 legacy: read from leads DB
        lead_db = ctx.lead_db()
        try:
            leads, _ = _query_leads(lead_db, wb.filter_criteria or {}, page=1, page_size=10000)
        finally:
            lead_db.close()
        if body.lead_ids:
            leads = [l for l in leads if l["id"] in body.lead_ids]

    if not leads:
        return RunWorkbookResponse(status="skipped", total_jobs=0, message="No rows to process")

    # Update workbook status + reset progress for this run
    wb.status = "running"
    wb.last_run_at = datetime.now(timezone.utc)
    wb.total_rows = len(leads)
    wb.completed_rows = 0
    db.commit()

    total_jobs = len(leads) * len(enrichment_cols)

    # P-1: enqueue ONE durable job on queue_service (DB-polling worker with
    # heartbeat + dead-job reaper + retry). The handler runs cells concurrently
    # off the request thread, so /run returns immediately and the workbook can
    # never get stuck in `running` (the reaper recovers a crashed run).
    # See features/workbook-v2-source-engine-spec.md §1.5.
    from apps.api.services.queue_service import queue_service
    queue_service.add_job(
        db,
        "run_workbook",
        {
            "workbook_id": workbook_id,
            "column_ids": [c["id"] for c in enrichment_cols],
            "row_ids": body.row_ids,
            "lead_ids": body.lead_ids,
        },
    )
    return RunWorkbookResponse(
        status="started",
        total_jobs=total_jobs,
        message=f"Enqueued run: {len(leads)} rows × {len(enrichment_cols)} columns",
    )


@router.post("/{workbook_id}/stop")
async def stop_workbook(
    workbook_id: str,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    """Stop a running workbook."""
    wb = _owned_workbook(db, workbook_id, ctx)
    wb.status = "paused"
    db.commit()
    return {"status": "paused"}


# ── Source Columns (P0) — sourcing as a workbook primitive ───────────────

class SourceColumnRequest(BaseModel):
    name: str = "Source"
    icp: dict = Field(default_factory=dict)        # {description, industry, geo, size, keywords_any, exclude}
    channels: dict = Field(default_factory=dict)   # {categories, regions, explicit_sources}
    target_rows: int = 0                            # 0 = unlimited


@router.post("/{workbook_id}/sources")
async def add_source_column(workbook_id: str, body: SourceColumnRequest, db: Session = Depends(get_db)):
    """Add a `source` column (ICP-driven) to a workbook."""
    import uuid
    wb = db.query(Workbook).filter(Workbook.id == workbook_id).first()
    if not wb:
        raise HTTPException(status_code=404, detail="Workbook not found")

    col = {
        "id": f"src_{uuid.uuid4().hex[:8]}",
        "name": body.name,
        "type": "source",
        "icp": body.icp,
        "channels": body.channels,
        "target_rows": body.target_rows,
    }
    cfg = list(wb.columns_config or [])
    cfg.append(col)
    wb.columns_config = cfg
    db.commit()
    return {"column": col}


@router.post("/{workbook_id}/sources/{col_id}/run")
async def run_source_column(workbook_id: str, col_id: str, db: Session = Depends(get_db)):
    """Materialize rows from a source column — runs on the durable queue worker."""
    wb = db.query(Workbook).filter(Workbook.id == workbook_id).first()
    if not wb:
        raise HTTPException(status_code=404, detail="Workbook not found")
    col = next((c for c in (wb.columns_config or [])
                if c.get("id") == col_id and c.get("type") == "source"), None)
    if not col:
        raise HTTPException(status_code=404, detail="Source column not found")

    from apps.api.services.queue_service import queue_service
    queue_service.add_job(db, "source_workbook", {"workbook_id": workbook_id, "column_id": col_id})
    return {"status": "started", "column_id": col_id, "message": "Sourcing started"}


@router.get("/{workbook_id}/sources/{col_id}/preview")
async def preview_source_column(workbook_id: str, col_id: str, db: Session = Depends(get_db)):
    """Dry-run: the query that would run + which sources it would hit. No write."""
    wb = db.query(Workbook).filter(Workbook.id == workbook_id).first()
    if not wb:
        raise HTTPException(status_code=404, detail="Workbook not found")
    col = next((c for c in (wb.columns_config or [])
                if c.get("id") == col_id and c.get("type") == "source"), None)
    if not col:
        raise HTTPException(status_code=404, detail="Source column not found")

    from apps.api.services.workbook.source_engine import preview_source
    return preview_source(col.get("icp") or {}, col.get("channels") or {})


# ── Cost & provider stats (P2) ───────────────────────────────────────────

class BudgetRequest(BaseModel):
    max_usd: float = 0.0  # 0 = unlimited


@router.put("/{workbook_id}/budget")
async def set_budget(workbook_id: str, body: BudgetRequest, db: Session = Depends(get_db)):
    """Set a workbook's spend ceiling. Paid providers are skipped once exhausted."""
    wb = db.query(Workbook).filter(Workbook.id == workbook_id).first()
    if not wb:
        raise HTTPException(status_code=404, detail="Workbook not found")
    wb.budget_max_usd = max(0.0, body.max_usd)
    db.commit()
    return {"budget_max_usd": wb.budget_max_usd, "budget_spent_usd": wb.budget_spent_usd or 0.0}


@router.get("/{workbook_id}/cost")
async def get_cost(workbook_id: str, db: Session = Depends(get_db)):
    """Spend-to-date + budget headroom for a workbook."""
    wb = db.query(Workbook).filter(Workbook.id == workbook_id).first()
    if not wb:
        raise HTTPException(status_code=404, detail="Workbook not found")
    spent = wb.budget_spent_usd or 0.0
    cap = wb.budget_max_usd or 0.0
    return {
        "budget_max_usd": cap,
        "budget_spent_usd": round(spent, 4),
        "remaining_usd": round(cap - spent, 4) if cap > 0 else None,
        "unlimited": cap <= 0,
    }


@router.get("/meta/provider-stats")
async def provider_stats(db: Session = Depends(get_db)):
    """Learned per-provider/-field yield, latency, cost ledger (feeds the planner)."""
    from apps.api.services.workbook.planner_models import ProviderStat
    rows = db.query(ProviderStat).order_by(ProviderStat.attempts.desc()).all()
    return {"stats": [r.to_api() for r in rows]}


# ── Living workbooks (P3) ────────────────────────────────────────────────

class RefreshPolicyRequest(BaseModel):
    enabled: bool = True
    interval: Optional[str] = None            # "hourly" | "daily" | "weekly" | minutes (int)
    on_signal: list = Field(default_factory=list)        # ["hiring","funding",...]
    staleness_ttl_days: dict = Field(default_factory=dict)  # {field: days}


@router.put("/{workbook_id}/refresh-policy")
async def update_refresh_policy(workbook_id: str, body: RefreshPolicyRequest, db: Session = Depends(get_db)):
    """Make a workbook 'living': schedule recurring refresh and/or signal triggers."""
    from apps.api.services.workbook.refresh import set_refresh_policy
    result = set_refresh_policy(db, workbook_id, body.model_dump())
    if "error" in result:
        raise HTTPException(status_code=404, detail=result["error"])
    return result


@router.post("/{workbook_id}/refresh")
async def refresh_now(workbook_id: str, db: Session = Depends(get_db)):
    """Trigger one refresh cycle immediately (source new rows + re-enrich stale)."""
    wb = db.query(Workbook).filter(Workbook.id == workbook_id).first()
    if not wb:
        raise HTTPException(status_code=404, detail="Workbook not found")
    from apps.api.services.queue_service import queue_service
    queue_service.add_job(db, "refresh_workbook", {"workbook_id": workbook_id, "reason": "manual"})
    return {"status": "refreshing"}


@router.get("/{workbook_id}/rows/{lead_id}/cells/{col_id}/trace")
async def get_cell_trace(workbook_id: str, lead_id: int, col_id: str, db: Session = Depends(get_db)):
    """The agent column's reasoning trace for a cell (which tools, why, cost)."""
    from apps.api.services.workbook.trace_models import CellTrace
    t = db.query(CellTrace).filter(
        CellTrace.workbook_id == workbook_id,
        CellTrace.lead_id == lead_id,
        CellTrace.column_id == col_id,
    ).first()
    if not t:
        raise HTTPException(status_code=404, detail="No trace for this cell")
    return t.to_api()


@router.get("/{workbook_id}/activity")
async def get_activity(workbook_id: str, limit: int = Query(50, le=500), db: Session = Depends(get_db)):
    """Event feed: rows added, refreshes, signals fired, re-enrichments."""
    from apps.api.services.workbook.activity_models import WorkbookActivity
    rows = (
        db.query(WorkbookActivity)
        .filter(WorkbookActivity.workbook_id == workbook_id)
        .order_by(WorkbookActivity.created_at.desc())
        .limit(limit)
        .all()
    )
    return {"activity": [r.to_api() for r in rows]}


# ── Row Management (v2) ──────────────────────────────────────────────────

@router.post("/{workbook_id}/rows")
async def add_rows(
    workbook_id: str,
    body: AddRowsRequest,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    """Add rows to a workbook."""
    wb = _owned_workbook(db, workbook_id, ctx)

    max_pos = db.query(sa_func.max(WorkbookRow.position)).filter(
        WorkbookRow.workbook_id == workbook_id
    ).scalar() or 0

    added = 0
    for i, row_data in enumerate(body.rows):
        db.add(WorkbookRow(
            workbook_id=workbook_id,
            position=max_pos + i + 1,
            data=row_data,
            lead_id=row_data.get("id"),
            enrichments={},
        ))
        added += 1
    db.commit()
    return {"added": added, "total_rows": max_pos + added + 1}


@router.delete("/{workbook_id}/rows")
async def delete_rows(
    workbook_id: str,
    body: DeleteRowsRequest,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    """Delete rows from a workbook."""
    wb = _owned_workbook(db, workbook_id, ctx)

    deleted = db.query(WorkbookRow).filter(
        WorkbookRow.workbook_id == workbook_id,
        WorkbookRow.id.in_(body.row_ids),
    ).delete(synchronize_session=False)
    db.commit()
    return {"deleted": deleted}


@router.post("/{workbook_id}/migrate")
async def migrate_workbook_to_v2(
    workbook_id: str,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    """Migrate a v1 workbook to v2 by snapshotting leads into WorkbookRow."""
    wb = _owned_workbook(db, workbook_id, ctx)

    # Check if already migrated
    existing = db.query(sa_func.count(WorkbookRow.id)).filter(
        WorkbookRow.workbook_id == workbook_id
    ).scalar() or 0
    if existing > 0:
        return {"status": "already_migrated", "rows": existing}

    # Snapshot leads — limited to 500 to avoid OOM
    lead_db = ctx.lead_db()
    try:
        leads, total = _query_leads(lead_db, wb.filter_criteria or {}, page=1, page_size=500)
    finally:
        lead_db.close()

    # Trim lead data to only fields used by workbook columns
    col_fields = set()
    for c in (wb.columns_config or []):
        if c.get("lead_field"):
            col_fields.add(c["lead_field"])
    col_fields.update(["id", "company", "website", "email", "phone", "city"])  # essentials

    def _trim(lead: dict) -> dict:
        if not col_fields:
            return lead
        return {k: v for k, v in lead.items() if k in col_fields}

    # Copy existing enrichment overlay into inline JSON
    enrichments = db.query(WorkbookEnrichment).filter(
        WorkbookEnrichment.workbook_id == workbook_id,
    ).all()
    enrich_map: dict[int, dict] = {}
    for e in enrichments:
        if e.lead_id not in enrich_map:
            enrich_map[e.lead_id] = {}
        enrich_map[e.lead_id][e.column_id] = {
            "value": e.value, "status": e.status or "pending",
            "provider": e.provider, "error": e.error,
        }

    # Insert in batches with commits to reduce memory
    for i, lead in enumerate(leads):
        db.add(WorkbookRow(
            workbook_id=workbook_id,
            position=i,
            data=_trim(lead),
            lead_id=lead.get("id"),
            enrichments=enrich_map.get(lead.get("id"), {}),
        ))
        if (i + 1) % 50 == 0:
            db.commit()

    # Update workbook metadata
    wb.source_type = "leads_filter"
    wb.source_config = wb.filter_criteria or {}
    db.commit()

    return {"status": "migrated", "rows": len(leads), "enrichments_migrated": len(enrichments)}


# ── WebSocket ─────────────────────────────────────────────────────────────

def _ws_authorize(token: Optional[str], workbook_id: str) -> bool:
    """Validate a WS JWT and confirm the user can access this workbook's workspace."""
    if not token:
        return False
    try:
        from jose import jwt, JWTError
        from apps.api.core.config import settings as _settings
        from apps.api.database import SessionLocal
        from apps.api.models import User
        from apps.api.services.workspace import manager as _ws

        try:
            payload = jwt.decode(token, _settings.SECRET_KEY, algorithms=[_settings.ALGORITHM])
        except JWTError:
            return False
        username = payload.get("sub")
        if not username:
            return False

        sess = SessionLocal()
        try:
            user = sess.query(User).filter(User.username == username).first()
            if not user or not user.is_active:
                return False
            wb = sess.query(Workbook).filter(Workbook.id == workbook_id).first()
            if not wb or not wb.workspace_id:
                return False
            return _ws.is_member(wb.workspace_id, user.id)
        finally:
            sess.close()
    except Exception:
        return False


@router.websocket("/{workbook_id}/ws")
async def workbook_websocket(
    websocket: WebSocket, workbook_id: str, token: Optional[str] = Query(default=None)
):
    """WebSocket for live enrichment updates (auth via ?token=<jwt>)."""
    if not _ws_authorize(token, workbook_id):
        await websocket.close(code=4403)
        return
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
async def get_column_types(ctx: WorkspaceCtx = Depends(current_workspace)):
    """Get available column types."""
    return COLUMN_TYPES


@router.get("/meta/lead-fields")
async def get_lead_fields(ctx: WorkspaceCtx = Depends(current_workspace)):
    """Get available Lead fields for column mapping."""
    return {"fields": list(LEAD_FIELD_MAP.keys())}


@router.get("/meta/providers")
async def get_providers(ctx: WorkspaceCtx = Depends(current_workspace)):
    """Get available enrichment providers."""
    from apps.api.services.workbook.providers import list_providers
    providers = list_providers()
    return {"providers": [
        {"name": p["name"], "capabilities": p.get("capabilities", []), "confidence": p.get("confidence", 0.5)}
        for p in providers
    ]}


@router.get("/meta/filter-options")
async def get_filter_options(ctx: WorkspaceCtx = Depends(current_workspace)):
    """Get available filter values from the leads DB."""
    lead_db = ctx.lead_db()
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
