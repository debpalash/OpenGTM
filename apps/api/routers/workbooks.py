"""Workbook API router — Clay-style self-contained tables with WorkbookRow."""

from datetime import datetime, timezone
from typing import Optional
import json
import logging
import os

from fastapi import APIRouter, Depends, HTTPException, WebSocket, WebSocketDisconnect, Query, Request
from pydantic import BaseModel, Field
from sqlalchemy.orm import Session
from sqlalchemy import func as sa_func

from apps.api.database import get_db
from apps.api.services.workbook.models import (
    ConnectorRun, Workbook, WorkbookEnrichment, WorkbookRow, WorkbookView,
    COLUMN_TYPES, LEAD_FIELD_MAP,
)
from apps.api.services.workbook.schemas import (
    WorkbookCreate, WorkbookUpdate, WorkbookResponse,
    WorkbookListResponse, WorkbookWithLeadsResponse,
    WorkbookLeadRow, EnrichmentOverlay,
    RunWorkbookRequest, RunWorkbookResponse, RunCellRequest,
    AddColumnRequest, ExportRequest,
    AddRowsRequest, ImportRowsRequest, DeleteRowsRequest,
    GenerateColumnRequest, GenerateColumnResponse,
    WorkbookViewCreate, WorkbookViewUpdate,
    WorkbookViewResponse, WorkbookViewListResponse,
)
from apps.api.services.leadgen.db import LeadDB
from apps.api.core.tenancy import (
    WorkspaceCtx,
    current_workspace,
    require_workspace_role,
)
from apps.api.core.ratelimit import limiter

logger = logging.getLogger("workbook.api")
router = APIRouter(prefix="/api/workbooks", tags=["workbooks"])
# v2 namespace (matches /api/v2/* convention) — NL → column generator lives here.
router_v2 = APIRouter(prefix="/api/v2/workbooks", tags=["workbooks"])
# Saved views live under the v2 prefix (new surface, no legacy consumers).
views_router = APIRouter(prefix="/api/v2/workbooks", tags=["workbook-views"])
require_editor = require_workspace_role("editor", "admin")


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
    """Query through the backend-neutral tenant lead-store contract."""
    return db.query_leads_page(filter_criteria, page=page, page_size=page_size)


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
    ctx: WorkspaceCtx = Depends(require_editor),
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
                    workspace_id=ctx.workspace_id,
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
                workspace_id=ctx.workspace_id,
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
                        workspace_id=ctx.workspace_id,
                        position=i,
                        data=lead,
                        lead_id=lead.get("id"),
                        enrichments={},
                    ))
                db.commit()
            finally:
                lead_db.close()

    # source="empty" → no rows created

    # Automations: on_row_added event (sites 1/2/4 — leads_filter / csv import /
    # job_results snapshot at workbook creation). Emit the freshly-inserted ids.
    try:
        from apps.api.services.automations import events as _auto_events
        _new_ids = [
            rid for (rid,) in db.query(WorkbookRow.id).filter(WorkbookRow.workbook_id == wb.id).all()
        ]
        if _new_ids:
            _auto_events.emit_row_added(ctx.workspace_id, wb.id, _new_ids)
    except Exception as _e:
        logger.warning("on_row_added emit (create_workbook) failed: %s", _e)

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
    ctx: WorkspaceCtx = Depends(require_editor),
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
            from apps.api.services.workspace.manager import workspace_leads_db_path

            job_db = LeadDB(workspace_leads_db_path(ctx.slug))
            try:
                job_queries = []
                for jid in body.job_ids[:3]:
                    job = job_db.conn.execute(
                        "SELECT query FROM jobs WHERE id = ?", (jid,)
                    ).fetchone()
                    if job:
                        job_queries.append(job[0])
                if job_queries:
                    auto_name = " + ".join(job_queries)[:120]
                else:
                    auto_name = f"Task Workbook ({len(body.job_ids)} jobs)"
            finally:
                job_db.close()

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
                    workspace_id=ctx.workspace_id,
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
                lead_id=r.lead_id,
                row_id=r.id,
                position=r.position,
                source_provider=r.source_provider,
                source_record_id=r.source_record_id,
                source_rank=r.source_rank,
                source_fetched_at=r.source_fetched_at,
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
        vstatus = None
        prov = None
        if isinstance(e.cell_metadata, dict):
            vstatus = (e.cell_metadata.get("verify") or {}).get("status")
            prov = e.cell_metadata.get("provenance")  # per-fact provenance (flag-gated)
        enrich_map[e.lead_id][e.column_id] = EnrichmentOverlay(
            value=e.value, status=e.status or "pending",
            provider=e.provider, error=e.error, verify_status=vstatus,
            provenance=prov,
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


@router.get("/{workbook_id}/connector-runs")
async def list_connector_runs(
    workbook_id: str,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    """Operational history for durable connector executions."""
    _owned_workbook(db, workbook_id, ctx)
    runs = db.query(ConnectorRun).filter(
        ConnectorRun.workbook_id == workbook_id
    ).order_by(ConnectorRun.created_at.desc()).limit(100).all()
    return {"runs": [run.to_api() for run in runs]}


@router.put("/{workbook_id}", response_model=WorkbookResponse)
async def update_workbook(
    workbook_id: str,
    body: WorkbookUpdate,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(require_editor),
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
    ctx: WorkspaceCtx = Depends(require_editor),
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
    ctx: WorkspaceCtx = Depends(require_editor),
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

        # Per-fact provenance (flag-gated): a manual edit is source=user-provided
        # (no license claim). Read-modify-write merge into field_provenance so we
        # don't clobber provider provenance on other fields.
        write_updates = dict(updates)
        try:
            from apps.api.core.config import settings as _prov_settings
            if getattr(_prov_settings, "PROVENANCE_TRACKING_ENABLED", False):
                from apps.api.services.leadgen.enrichment.licenses import (
                    provenance_for, merge_field_provenance, SOURCE_USER_PROVIDED,
                )
                current = lead_db.get_lead(lead_id)
                cur_fp = getattr(current, "field_provenance", "") if current else ""
                provs = {
                    f: provenance_for(SOURCE_USER_PROVIDED, license="user-provided")
                    for f in updates
                }
                write_updates["field_provenance"] = merge_field_provenance(cur_fp or "", provs)
        except Exception as e:
            logger.debug(f"manual-edit provenance skipped: {e}")
            write_updates = dict(updates)

        if not lead_db.get_lead(lead_id):
            raise HTTPException(status_code=404, detail="Lead not found")
        lead_db.update_lead_fields(lead_id, write_updates)
        return {"status": "updated", "lead_id": lead_id, "fields": list(updates.keys())}
    finally:
        lead_db.close()


# ── CSV Import ────────────────────────────────────────────────────────────

@router.post("/{workbook_id}/import")
async def import_csv_leads(
    workbook_id: str,
    body: ImportRowsRequest,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(require_editor),
):
    """Import CSV rows into the workbook and preserve its complete schema."""
    wb = _owned_workbook(db, workbook_id, ctx)
    from apps.api.services.workbook.csv_import import prepare_csv_import
    from apps.api.services.leadgen.dedup import normalize_domain, normalize_company

    rows, columns, added_columns, resolved_mapping = prepare_csv_import(
        body.rows,
        wb.columns_config or [],
        body.mapping,
        create_columns=body.create_columns,
    )
    if not rows:
        raise HTTPException(status_code=400, detail="CSV has no importable rows")
    if len(resolved_mapping) > 500:
        raise HTTPException(status_code=400, detail="CSV has more than 500 columns")

    def _identity(data: dict) -> str:
        domain = normalize_domain(data.get("website") or data.get("domain") or "")
        if domain:
            return f"d:{domain}"
        company = normalize_company(data.get("company") or "")
        return f"n:{company}" if company else ""

    seen: set[str] = set()
    if body.dedupe:
        for (data,) in db.query(WorkbookRow.data).filter(WorkbookRow.workbook_id == workbook_id):
            identity = _identity(data or {})
            if identity:
                seen.add(identity)

    max_pos = db.query(sa_func.max(WorkbookRow.position)).filter(
        WorkbookRow.workbook_id == workbook_id
    ).scalar()
    next_pos = 0 if max_pos is None else max_pos + 1
    new_rows: list[WorkbookRow] = []
    skipped = 0
    for row in rows:
        identity = _identity(row)
        if body.dedupe and identity and identity in seen:
            skipped += 1
            continue
        if identity:
            seen.add(identity)
        workbook_row = WorkbookRow(
            workbook_id=workbook_id,
            workspace_id=ctx.workspace_id,
            position=next_pos + len(new_rows),
            data=row,
            enrichments={},
        )
        db.add(workbook_row)
        new_rows.append(workbook_row)

    wb.columns_config = columns
    if wb.source_type == "empty":
        wb.source_type = "csv"
    source_config = dict(wb.source_config or {})
    source_config["last_csv_import"] = {
        "file_name": body.file_name,
        "rows": len(new_rows),
        "columns_added": len(added_columns),
        "imported_at": datetime.now(timezone.utc).isoformat(),
    }
    wb.source_config = source_config
    db.commit()

    try:
        from apps.api.services.automations import events as _auto_events
        _auto_events.emit_row_added(
            ctx.workspace_id, workbook_id, [row.id for row in new_rows],
        )
    except Exception as exc:
        logger.warning("on_row_added emit (csv import) failed: %s", exc)

    total_rows = db.query(sa_func.count(WorkbookRow.id)).filter(
        WorkbookRow.workbook_id == workbook_id
    ).scalar() or 0
    return {
        "created": len(new_rows),
        "added": len(new_rows),
        "skipped_duplicates": skipped,
        "total_rows": total_rows,
        "columns_added": [column["name"] for column in added_columns],
        "mapping": resolved_mapping,
    }


# ── Column Management ────────────────────────────────────────────────────

@router.post("/{workbook_id}/columns", response_model=WorkbookResponse)
async def add_column(
    workbook_id: str,
    body: AddColumnRequest,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(require_editor),
):
    """Add a new column to the workbook."""
    wb = _owned_workbook(db, workbook_id, ctx)

    cols = list(wb.columns_config or [])
    cols.append(body.column.model_dump(exclude_none=True))
    wb.columns_config = cols
    db.commit()
    db.refresh(wb)
    return _workbook_response(wb)


@router_v2.post("/{workbook_id}/generate-column", response_model=GenerateColumnResponse)
async def generate_column(
    workbook_id: str,
    body: GenerateColumnRequest,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(require_editor),
):
    """NL → column generator: turn an instruction into a validated, ready-to-add
    column config (formula / ai_formula / http). Does NOT add the column."""
    from apps.api.services.workbook.nl_column import NLColumnError, generate_column as _generate

    wb = _owned_workbook(db, workbook_id, ctx)
    try:
        return await _generate(body.instruction, wb.columns_config or [])
    except NLColumnError as e:
        raise HTTPException(status_code=502, detail=f"Column generation failed: {e}")


@router.delete("/{workbook_id}/columns/{column_id}", response_model=WorkbookResponse)
async def remove_column(
    workbook_id: str,
    column_id: str,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(require_editor),
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

@router.get("/{workbook_id}/run/estimate")
def estimate_run(
    workbook_id: str,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    """Estimate the spend of running this workbook BEFORE running it.

    Returns worst/best USD + a per-column breakdown of paid providers, so the UI
    can show a spend-confirmation gate ("N rows × providers = $X, proceed?").
    """
    from apps.api.services.workbook import vendor_catalog
    from apps.api.services.workbook.enrichment import DEFAULT_WATERFALLS, ENRICHMENT_COL_TYPES

    wb = _owned_workbook(db, workbook_id, ctx)
    num_rows = db.query(sa_func.count(WorkbookRow.id)).filter(
        WorkbookRow.workbook_id == wb.id
    ).scalar() or 0

    providers_by_col = {}
    for c in (wb.columns_config or []):
        if c.get("type") not in ENRICHMENT_COL_TYPES:
            continue
        target = c.get("target_field") or c.get("lead_field") or c.get("id")
        chain = c.get("waterfall") or ([c["provider"]] if c.get("provider") else [])
        if not chain:
            chain = DEFAULT_WATERFALLS.get(target, [])
        providers_by_col[c.get("id") or target] = chain

    est = vendor_catalog.estimate_run_cost(num_rows, providers_by_col)
    est["workbook_id"] = workbook_id
    return est


@router.post("/{workbook_id}/run", response_model=RunWorkbookResponse)
@limiter.limit("20/minute")
async def run_workbook(
    request: Request,
    workbook_id: str,
    body: RunWorkbookRequest = None,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(require_editor),
):
    """Run enrichment on workbook rows (v2: WorkbookRow, v1 fallback: leads DB)."""
    if body is None:
        body = RunWorkbookRequest()

    wb = _owned_workbook(db, workbook_id, ctx)

    columns = wb.columns_config or []
    enrichment_cols = [
        c for c in columns
        if c.get("type") in ("enrichment", "waterfall", "ai_formula", "output", "research", "agent", "http", "formula")
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

    # Resolve the EXACT set of rows this run covers here, once, and pass their
    # ids to the worker. The worker must not re-derive the set from a different
    # code path: its v1 loader only honors a subset of filter fields, so a
    # filter like {"specialization": "SaaS"} would silently collapse to "all
    # leads" and enrich the whole table instead of the rows shown here.
    resolved_row_ids = None
    resolved_lead_ids = None
    if v2_count > 0:
        # v2: read from WorkbookRow
        query = db.query(WorkbookRow).filter(WorkbookRow.workbook_id == workbook_id)
        if body.row_ids:
            query = query.filter(WorkbookRow.id.in_(body.row_ids))
        elif body.lead_ids:
            query = query.filter(WorkbookRow.lead_id.in_(body.lead_ids))
        wb_rows = query.all()
        leads = [
            {"id": r.lead_id or r.id, "__row_id": r.id, "__lead_id": r.lead_id, **r.data}
            for r in wb_rows
        ]
        resolved_row_ids = [r.id for r in wb_rows]
    else:
        # v1 legacy: read from leads DB
        lead_db = ctx.lead_db()
        try:
            leads, _ = _query_leads(lead_db, wb.filter_criteria or {}, page=1, page_size=10000)
        finally:
            lead_db.close()
        if body.lead_ids:
            leads = [l for l in leads if l["id"] in body.lead_ids]
        resolved_lead_ids = [l["id"] for l in leads]

    if not leads:
        return RunWorkbookResponse(status="skipped", total_jobs=0, message="No rows to process")

    # ── Billing enforcement (WI-9) ──────────────────────────────────────
    # Same chokepoint that computes "N rows × providers = $X": debit the
    # platform-billed (non-BYOK) portion from the workspace's credit balance.
    # A no-op when BILLING_ENABLED is off (self-host), so runs are never blocked.
    # The debit is idempotent per run_id; we generate the run_id here and pass it
    # to the worker so a retried/superseded enqueue can't double-charge.
    import uuid as _uuid
    from apps.api.services.billing import service as _billing
    run_id = _uuid.uuid4().hex
    if _billing.billing_enabled():
        providers_by_col = {}
        for c in enrichment_cols:
            target = c.get("target_field") or c.get("lead_field") or c.get("id")
            chain = c.get("waterfall") or ([c["provider"]] if c.get("provider") else [])
            if not chain:
                from apps.api.services.workbook.enrichment import DEFAULT_WATERFALLS
                chain = DEFAULT_WATERFALLS.get(target, [])
            providers_by_col[c.get("id") or target] = chain
        projected = _billing.projected_platform_cost(len(leads), providers_by_col)
        try:
            _billing.check_and_debit(db, ctx.workspace_id, projected, run_id=run_id)
        except _billing.InsufficientCreditsError as e:
            raise HTTPException(
                status_code=402,
                detail={
                    "message": str(e),
                    "balance_usd": e.balance_usd,
                    "required_usd": e.required_usd,
                },
            )

    # Update workbook status + reset progress for this run
    wb.status = "running"
    wb.last_run_at = datetime.now(timezone.utc)
    wb.total_rows = len(leads)
    wb.completed_rows = 0
    db.commit()

    total_jobs = len(leads) * len(enrichment_cols)

    # Supersede any in-flight run for THIS workbook so runs don't stack (the
    # worker is sequential — a stale run would block this one and re-enrich).
    # The handler polls its own job status and stops promptly when cancelled.
    from apps.api.models import Job as _Job
    active_runs = (
        db.query(_Job)
        .filter(_Job.type == "run_workbook", _Job.status.in_(["pending", "processing"]))
        .all()
    )
    for j in active_runs:
        if (j.payload or {}).get("workbook_id") == workbook_id:
            j.status = "cancelled"
            j.error = "superseded by a newer run"
    if active_runs:
        db.commit()

    # P-1: enqueue ONE durable job on queue_service (DB-polling worker with
    # heartbeat + dead-job reaper + retry). The handler runs cells concurrently
    # off the request thread, so /run returns immediately and the workbook can
    # never get stuck in `running` (the reaper recovers a crashed run).
    # See docs/specs/workbook-v2-source-engine-spec.md §1.5.
    # Performance knobs come from Settings (user-configurable), with env/default
    # fallback. Concurrency = how many rows run at once; retry_passes re-runs the
    # cells still failing (lifts fill rate); max_providers caps waterfall depth
    # (0 = full chain). See settings.get_enrichment_settings.
    from apps.api.routers.settings import get_enrichment_settings
    _es = get_enrichment_settings()
    from apps.api.services.queue_service import queue_service
    queue_service.add_job(
        db,
        "run_workbook",
        {
            "workbook_id": workbook_id,
            # OD-4: stamp the tenant into the payload so the worker enters the
            # right workspace_scope (it must never read the row to learn its ws).
            "workspace_id": ctx.workspace_id,
            "run_id": run_id,
            "column_ids": [c["id"] for c in enrichment_cols],
            # Pass the resolved ids (not the raw request) so the worker enriches
            # exactly the rows resolved above — the single source of truth for
            # this run's scope.
            "row_ids": resolved_row_ids if resolved_row_ids is not None else body.row_ids,
            "lead_ids": resolved_lead_ids if resolved_lead_ids is not None else body.lead_ids,
            "concurrency": _es["row_concurrency"],
            "max_providers": _es["max_providers"],
            "retry_passes": _es["retry_passes"],
            "provider_workers": _es["provider_workers"],
            "provider_timeout": _es["provider_timeout"],
            "fill_missing": bool(body.fill_missing),
            # Force re-run: bypasses success-skip gates; for output columns this
            # overrides run-once (re-pushes). The UI confirms before sending it.
            "force": bool(body.force),
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
    ctx: WorkspaceCtx = Depends(require_editor),
):
    """Stop a running workbook.

    Sets the workbook to `paused` (the run handler polls this and halts within a
    couple seconds, before its next cell) and cancels the in-flight job so the
    sequential worker is freed immediately instead of waiting out the run.
    """
    wb = _owned_workbook(db, workbook_id, ctx)
    wb.status = "paused"
    from apps.api.models import Job as _Job
    active_runs = (
        db.query(_Job)
        .filter(_Job.type == "run_workbook", _Job.status.in_(["pending", "processing"]))
        .all()
    )
    for j in active_runs:
        if (j.payload or {}).get("workbook_id") == workbook_id:
            j.status = "cancelled"
            j.error = "stopped by user"
    db.commit()
    return {"status": "paused"}


# ── Single-cell re-run ────────────────────────────────────────────────────

@router.post("/{workbook_id}/rows/{row_id}/cells/{col_id}/run")
@limiter.limit("120/minute")
async def run_cell(
    request: Request,
    workbook_id: str,
    row_id: int,
    col_id: str,
    body: RunCellRequest = None,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(require_editor),
):
    """(Re-)run ONE cell synchronously and return its result.

    Unlike /run this doesn't enqueue a durable job — a single cell is small
    enough to run in-request, and the caller wants the fresh value back.

    ``force=true`` bypasses success-skip gates: for side-effecting ``output``
    columns it overrides the run-once guard and RE-PUSHES the row to the
    destination, so the UI confirms before sending it. Without force, a
    complete output cell returns ``{"skipped": true}`` untouched.

    ``row_id`` is the WorkbookRow id (v2); falls back to matching lead_id so
    v1/legacy rows (leads-DB backed) can be re-run too.
    """
    from apps.api.services.workbook.enrichment import enrich_cell, ENRICHMENT_COL_TYPES
    from apps.api.core.tenancy import workspace_scope

    if body is None:
        body = RunCellRequest()

    wb = _owned_workbook(db, workbook_id, ctx)
    columns_config = wb.columns_config or []
    runnable_types = ENRICHMENT_COL_TYPES
    col = next(
        (c for c in columns_config
         if c.get("id") == col_id and c.get("type") in runnable_types),
        None,
    )
    if not col:
        raise HTTPException(status_code=404, detail="Enrichment column not found")

    # Resolve the row — v2 WorkbookRow by id, then by lead_id; v1 leads-DB last.
    wr = db.query(WorkbookRow).filter(
        WorkbookRow.workbook_id == workbook_id, WorkbookRow.id == row_id
    ).first()
    if wr is None:
        wr = db.query(WorkbookRow).filter(
            WorkbookRow.workbook_id == workbook_id, WorkbookRow.lead_id == row_id
        ).first()

    if wr is not None:
        lead_id = wr.lead_id or wr.id
        lead_data = {
            "id": lead_id,
            "__row_id": wr.id,
            "__lead_id": wr.lead_id,
            **(wr.data or {}),
        }
    else:
        # v1 legacy — row lives in the tenant lead store.
        import dataclasses
        lead_db = ctx.lead_db()
        try:
            lead = lead_db.get_lead(row_id)
        finally:
            lead_db.close()
        if lead is None:
            raise HTTPException(status_code=404, detail="Row not found")
        lead_data = dataclasses.asdict(lead) if dataclasses.is_dataclass(lead) else dict(lead)
        lead_id = lead_data.get("id") or row_id
        lead_data["id"] = lead_id

    # Run under the caller's tenant scope (RLS GUC + workspace_id stamping on
    # the enrichment upsert), exactly like the queue worker does.
    with workspace_scope(ctx.workspace_id):
        result = await enrich_cell(
            db=db,
            workbook_id=workbook_id,
            lead_id=lead_id,
            col_id=col_id,
            col_config=col,
            lead_data=lead_data,
            columns_config=columns_config,
            redis_client=None,
            force=bool(body.force),
        )

    if result.get("skipped"):
        status = "skipped"
    elif result.get("success"):
        status = "complete"
    else:
        status = "error"
    return {
        "status": status,
        "value": result.get("value"),
        "provider": result.get("provider"),
        "error": result.get("error"),
        "skipped": bool(result.get("skipped", False)),
        "forced": bool(body.force),
    }


# ── Source Columns (P0) — sourcing as a workbook primitive ───────────────

class SourceColumnRequest(BaseModel):
    name: str = "Source"
    kind: str = "icp"                               # "icp" (default) | "people_search"
    icp: dict = Field(default_factory=dict)        # {description, industry, geo, size, keywords_any, exclude}
    channels: dict = Field(default_factory=dict)   # {categories, regions, explicit_sources}
    target_rows: int = 0                            # 0 = unlimited
    # ── people_search config (kind == "people_search") ──
    companies: list[str] = Field(default_factory=list)  # names or domains
    from_column: str = ""                           # OR: read companies off rows (column id/name/lead field)
    titles: list[str] = Field(default_factory=list)      # ["CTO", "Founder", ...]
    seniority: str = ""
    geo: str = ""
    max_per_company: int = 10                       # capped at 25
    max_searches: int = 100                         # total DDG queries per run
    # Non-ICP source kinds, e.g. CRM import (flag-gated CRM_IMPORT_ENABLED):
    # {kind: "crm_import", crm: "hubspot"|"salesforce", object: "contact",
    #  filter?: <hubspot list id | SOQL WHERE fragment>, limit?: 500,
    #  field_map?: {crm_prop: lead_field}}
    source: dict = Field(default_factory=dict)


@router.post("/{workbook_id}/sources")
async def add_source_column(
    workbook_id: str,
    body: SourceColumnRequest,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(require_editor),
):
    """Add a `source` column (ICP-driven, or people_search) to a workbook."""
    import uuid
    wb = _owned_workbook(db, workbook_id, ctx)

    if body.kind == "people_search":
        from apps.api.core.config import settings
        from apps.api.services.workbook.people_search import validate_people_search_config
        if not settings.PEOPLE_SEARCH_SOURCE_ENABLED:
            raise HTTPException(
                status_code=403,
                detail="people_search source is disabled (PEOPLE_SEARCH_SOURCE_ENABLED)",
            )
        try:
            ps = validate_people_search_config({
                "companies": body.companies,
                "from_column": body.from_column,
                "titles": body.titles,
                "seniority": body.seniority,
                "geo": body.geo,
                "max_per_company": body.max_per_company,
                "max_searches": body.max_searches,
            })
        except ValueError as e:
            raise HTTPException(status_code=422, detail=str(e))
        col = {
            "id": f"src_{uuid.uuid4().hex[:8]}",
            "name": body.name,
            "type": "source",
            "kind": "people_search",
            **ps,
        }
    else:
        col = {
            "id": f"src_{uuid.uuid4().hex[:8]}",
            "name": body.name,
            "type": "source",
            "icp": body.icp,
            "channels": body.channels,
            "target_rows": body.target_rows,
        }
        if body.source:
            col["source"] = body.source
    cfg = list(wb.columns_config or [])
    cfg.append(col)
    wb.columns_config = cfg
    db.commit()
    return {"column": col}


@router.post("/{workbook_id}/sources/{col_id}/run")
@limiter.limit("20/minute")
async def run_source_column(
    request: Request,
    workbook_id: str,
    col_id: str,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(require_editor),
):
    """Materialize rows from a source column — runs on the durable queue worker."""
    wb = _owned_workbook(db, workbook_id, ctx)
    col = next((c for c in (wb.columns_config or [])
                if c.get("id") == col_id and c.get("type") == "source"), None)
    if not col:
        raise HTTPException(status_code=404, detail="Source column not found")

    from apps.api.services.queue_service import queue_service
    # OD-4: the workbook row carries its tenant; stamp it into the payload so the
    # worker can enter workspace_scope (never reads the row to discover its ws).
    queue_service.add_job(
        db, "source_workbook",
        {"workbook_id": workbook_id, "column_id": col_id, "workspace_id": wb.workspace_id},
    )
    return {"status": "started", "column_id": col_id, "message": "Sourcing started"}


@router.get("/{workbook_id}/sources/{col_id}/preview")
async def preview_source_column(
    workbook_id: str,
    col_id: str,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    """Dry-run: the query that would run + which sources it would hit. No write."""
    wb = _owned_workbook(db, workbook_id, ctx)
    col = next((c for c in (wb.columns_config or [])
                if c.get("id") == col_id and c.get("type") == "source"), None)
    if not col:
        raise HTTPException(status_code=404, detail="Source column not found")

    if (col.get("kind") or "") == "people_search":
        from apps.api.services.workbook.people_search import preview_people_search
        return preview_people_search(col)

    from apps.api.services.workbook.source_engine import preview_source
    return preview_source(col.get("icp") or {}, col.get("channels") or {})


# ── Cost & provider stats (P2) ───────────────────────────────────────────

class BudgetRequest(BaseModel):
    max_usd: float = 0.0  # 0 = unlimited


@router.put("/{workbook_id}/budget")
async def set_budget(
    workbook_id: str,
    body: BudgetRequest,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(require_editor),
):
    """Set a workbook's spend ceiling. Paid providers are skipped once exhausted."""
    wb = _owned_workbook(db, workbook_id, ctx)
    wb.budget_max_usd = max(0.0, body.max_usd)
    db.commit()
    return {"budget_max_usd": wb.budget_max_usd, "budget_spent_usd": wb.budget_spent_usd or 0.0}


@router.get("/{workbook_id}/cost")
async def get_cost(
    workbook_id: str,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    """Spend-to-date + budget headroom for a workbook."""
    wb = _owned_workbook(db, workbook_id, ctx)
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


@router.get("/meta/provider-accuracy")
async def provider_accuracy(persist: bool = False, db: Session = Depends(get_db)):
    """Public per-provider CORRECTNESS ranking from the accuracy/freshness eval
    harness (scored offline against a golden dataset). This is the signal the
    hit-rate ledger is missing: a provider that returns confident WRONG data
    ranks low here. Pass ?persist=true to also write the correctness prior onto
    the ProviderStat ledger so it feeds the waterfall planner's ordering."""
    if persist:
        from apps.api.services.leadgen.enrichment.eval.persist import run_and_persist
        ranking = run_and_persist(db)
        db.commit()
    else:
        from apps.api.services.leadgen.enrichment.eval.scorer import run_eval
        ranking = run_eval()
    return {
        "ranking": [s.to_api() for s in ranking],
        "persisted": persist,
    }


# ── Living workbooks (P3) ────────────────────────────────────────────────

class RefreshPolicyRequest(BaseModel):
    enabled: bool = True
    interval: Optional[str] = None            # "hourly" | "daily" | "weekly" | minutes (int)
    on_signal: list = Field(default_factory=list)        # ["hiring","funding",...]
    staleness_ttl_days: dict = Field(default_factory=dict)  # {field: days}


@router.put("/{workbook_id}/refresh-policy")
async def update_refresh_policy(
    workbook_id: str,
    body: RefreshPolicyRequest,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(require_editor),
):
    """Make a workbook 'living': schedule recurring refresh and/or signal triggers."""
    from apps.api.services.workbook.refresh import set_refresh_policy
    _owned_workbook(db, workbook_id, ctx)
    result = set_refresh_policy(db, workbook_id, body.model_dump())
    if "error" in result:
        raise HTTPException(status_code=404, detail=result["error"])
    return result


@router.post("/{workbook_id}/refresh")
async def refresh_now(
    workbook_id: str,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(require_editor),
):
    """Trigger one refresh cycle immediately (source new rows + re-enrich stale)."""
    wb = _owned_workbook(db, workbook_id, ctx)
    from apps.api.services.queue_service import queue_service
    # OD-4: stamp the workbook's tenant so the refresh worker scopes correctly.
    queue_service.add_job(
        db, "refresh_workbook",
        {"workbook_id": workbook_id, "reason": "manual", "workspace_id": wb.workspace_id},
    )
    return {"status": "refreshing"}


@router.get("/{workbook_id}/rows/{lead_id}/cells/{col_id}/trace")
async def get_cell_trace(
    workbook_id: str,
    lead_id: int,
    col_id: str,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    """The agent column's reasoning trace for a cell (which tools, why, cost)."""
    from apps.api.services.workbook.trace_models import CellTrace
    _owned_workbook(db, workbook_id, ctx)
    t = db.query(CellTrace).filter(
        CellTrace.workbook_id == workbook_id,
        CellTrace.lead_id == lead_id,
        CellTrace.column_id == col_id,
    ).first()
    if not t:
        raise HTTPException(status_code=404, detail="No trace for this cell")
    return t.to_api()


@router.get("/{workbook_id}/activity")
async def get_activity(
    workbook_id: str,
    limit: int = Query(50, le=500),
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    """Event feed: rows added, refreshes, signals fired, re-enrichments."""
    from apps.api.services.workbook.activity_models import WorkbookActivity
    _owned_workbook(db, workbook_id, ctx)
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
    ctx: WorkspaceCtx = Depends(require_editor),
):
    """Add rows to a workbook."""
    wb = _owned_workbook(db, workbook_id, ctx)

    max_pos = db.query(sa_func.max(WorkbookRow.position)).filter(
        WorkbookRow.workbook_id == workbook_id
    ).scalar() or 0

    incoming = body.rows
    skipped = 0
    if body.dedupe:
        from apps.api.services.leadgen.dedup import normalize_domain, normalize_company

        def _identity(d: dict) -> str:
            dom = normalize_domain(d.get("website") or d.get("domain") or "")
            if dom:
                return f"d:{dom}"
            comp = normalize_company(d.get("company") or "")
            return f"n:{comp}" if comp else ""

        # identities already present in the workbook
        seen = set()
        for (data,) in db.query(WorkbookRow.data).filter(WorkbookRow.workbook_id == workbook_id):
            ident = _identity(data or {})
            if ident:
                seen.add(ident)

        deduped = []
        for row in incoming:
            ident = _identity(row)
            if ident and ident in seen:
                skipped += 1
                continue
            if ident:
                seen.add(ident)
            deduped.append(row)
        incoming = deduped

    added = 0
    new_rows = []
    for i, row_data in enumerate(incoming):
        r = WorkbookRow(
            workbook_id=workbook_id,
            workspace_id=ctx.workspace_id,
            position=max_pos + i + 1,
            data=row_data,
            lead_id=row_data.get("id"),
            enrichments={},
        )
        db.add(r)
        new_rows.append(r)
        added += 1
    db.commit()
    # Automations: on_row_added event (site 3/4 — manual add-row endpoint).
    try:
        from apps.api.services.automations import events as _auto_events
        _auto_events.emit_row_added(
            ctx.workspace_id, workbook_id, [r.id for r in new_rows],
        )
    except Exception as _e:
        logger.warning("on_row_added emit (add_rows) failed: %s", _e)
    return {"added": added, "skipped_duplicates": skipped, "total_rows": max_pos + added + 1}


@router.delete("/{workbook_id}/rows")
async def delete_rows(
    workbook_id: str,
    body: DeleteRowsRequest,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(require_editor),
):
    """Delete rows from a workbook."""
    wb = _owned_workbook(db, workbook_id, ctx)

    deleted = db.query(WorkbookRow).filter(
        WorkbookRow.workbook_id == workbook_id,
        WorkbookRow.id.in_(body.row_ids),
    ).delete(synchronize_session=False)
    db.commit()
    return {"deleted": deleted}


@router.patch("/{workbook_id}/rows/{row_id}")
async def update_row_data(
    workbook_id: str,
    row_id: int,
    body: dict,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(require_editor),
):
    """Update a workbook snapshot without impersonating a Lead record."""
    _owned_workbook(db, workbook_id, ctx)
    row = db.query(WorkbookRow).filter(
        WorkbookRow.workbook_id == workbook_id,
        WorkbookRow.id == row_id,
    ).first()
    if row is None:
        raise HTTPException(status_code=404, detail="Workbook row not found")

    allowed = set(LEAD_FIELD_MAP)
    allowed.update(
        c.get("lead_field") or c.get("id")
        for c in (row.workbook.columns_config or [])
        if c.get("type") in ("lead_field", "input")
    )
    updates = {
        key: value
        for key, value in body.items()
        if key in allowed and key not in {"id", "row_id", "lead_id"}
    }
    if not updates:
        raise HTTPException(status_code=400, detail="No editable row fields supplied")

    from sqlalchemy.orm.attributes import flag_modified

    row.data = {**(row.data or {}), **updates}
    flag_modified(row, "data")
    db.commit()
    return {"status": "updated", "row_id": row_id, "fields": list(updates)}


@router.post("/{workbook_id}/migrate")
async def migrate_workbook_to_v2(
    workbook_id: str,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(require_editor),
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
            workspace_id=ctx.workspace_id,
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

def _ws_authorize(token: Optional[str], workbook_id: str, workspace_id: Optional[str]) -> bool:
    """Validate a WS JWT and confirm the user can access this workbook's workspace.

    OD-5: the tenant arrives OUT-OF-BAND as a ws query param (the frontend always
    knows the active workspace) — we never read the workbook row to discover its
    workspace, because under FORCE RLS that read happens before any scope is set
    and would return nothing. We enter ``workspace_scope(workspace_id)`` first,
    then look up the workbook (now RLS-scoped) and confirm both that it exists in
    that tenant (belt) and that the user is a member of it.
    """
    if not token or not workspace_id:
        return False
    try:
        from apps.api.core.security import authenticate_query_token
        from apps.api.core.tenancy import workspace_scope
        from apps.api.database import SessionLocal
        from apps.api.services.workspace import manager as _ws

        user = authenticate_query_token(token)

        with workspace_scope(workspace_id):
            sess = SessionLocal()
            try:
                # Membership in the CLAIMED workspace — a forged/wrong ws fails here.
                if not _ws.is_member(workspace_id, user.id):
                    return False
                # The workbook must belong to that workspace (belt; RLS suspenders).
                wb = sess.query(Workbook).filter(Workbook.id == workbook_id).first()
                if not wb or wb.workspace_id != workspace_id:
                    return False
                return True
            finally:
                sess.close()
    except Exception:
        return False


@router.websocket("/{workbook_id}/ws")
async def workbook_websocket(
    websocket: WebSocket,
    workbook_id: str,
    token: Optional[str] = Query(default=None),
    workspace_id: Optional[str] = Query(default=None),
):
    """WebSocket for live enrichment updates (auth via ?token=<jwt>&workspace_id=<ws>).

    OD-5: the workspace is passed out-of-band as a query param so authorization
    can scope to the tenant before touching the (RLS-protected) workbook row.
    """
    if not _ws_authorize(token, workbook_id, workspace_id):
        await websocket.close(code=4403)
        return
    await websocket.accept()
    try:
        import redis.asyncio as aioredis
        # Honour REDIS_URL so live cell updates work in the shipped Docker stack
        # (compose points services at redis://redis:6379, not localhost). Matches
        # the publisher in services/workbook/enrichment.py. No decode_responses —
        # this consumer decodes message payloads manually below.
        redis_url = os.getenv("REDIS_URL", "redis://localhost:6379")
        redis_client = await aioredis.from_url(redis_url)
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


# ── Saved Views (v2) ──────────────────────────────────────────────────────
# Named filter/sort/hidden-column presets per workbook. Presentation-layer
# only — a view never mutates rows; the editor applies its config client-side.
# Workspace-scoped exactly like the parent workbook: other-tenant ids → 404.

def _owned_view(db: Session, workbook_id: str, view_id: str, ctx: WorkspaceCtx) -> WorkbookView:
    """Fetch a view scoped to (workbook, workspace); 404 on any mismatch."""
    v = db.query(WorkbookView).filter(WorkbookView.id == view_id).first()
    if (not v or v.workbook_id != workbook_id
            or v.workspace_id != ctx.workspace_id):
        raise HTTPException(status_code=404, detail="View not found")
    return v


@views_router.get("/{workbook_id}/views", response_model=WorkbookViewListResponse)
async def list_views(
    workbook_id: str,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    """List saved views for a workbook (oldest first, stable switcher order)."""
    _owned_workbook(db, workbook_id, ctx)
    views = (
        db.query(WorkbookView)
        .filter(
            WorkbookView.workbook_id == workbook_id,
            WorkbookView.workspace_id == ctx.workspace_id,
        )
        .order_by(WorkbookView.created_at, WorkbookView.id)
        .all()
    )
    return WorkbookViewListResponse(views=views, total=len(views))


@views_router.post("/{workbook_id}/views", response_model=WorkbookViewResponse, status_code=201)
async def create_view(
    workbook_id: str,
    body: WorkbookViewCreate,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(require_editor),
):
    """Create a saved view on a workbook."""
    _owned_workbook(db, workbook_id, ctx)
    v = WorkbookView(
        workbook_id=workbook_id,
        workspace_id=ctx.workspace_id,
        name=body.name,
        config=body.config.model_dump(),
    )
    db.add(v)
    db.commit()
    db.refresh(v)
    return v


@views_router.put("/{workbook_id}/views/{view_id}", response_model=WorkbookViewResponse)
async def update_view(
    workbook_id: str,
    view_id: str,
    body: WorkbookViewUpdate,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(require_editor),
):
    """Rename a view and/or replace its filter/sort/hidden-column config."""
    _owned_workbook(db, workbook_id, ctx)
    v = _owned_view(db, workbook_id, view_id, ctx)
    if body.name is not None:
        v.name = body.name
    if body.config is not None:
        v.config = body.config.model_dump()
    v.updated_at = datetime.now(timezone.utc)
    db.commit()
    db.refresh(v)
    return v


@views_router.delete("/{workbook_id}/views/{view_id}")
async def delete_view(
    workbook_id: str,
    view_id: str,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(require_editor),
):
    """Delete a saved view (never touches the workbook's rows)."""
    _owned_workbook(db, workbook_id, ctx)
    v = _owned_view(db, workbook_id, view_id, ctx)
    db.delete(v)
    db.commit()
    return {"status": "deleted"}


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


@router.get("/meta/ai-column-presets")
async def get_ai_column_presets(category: Optional[str] = None, ctx: WorkspaceCtx = Depends(current_workspace)):
    """Ready-to-ship AI-column prompt presets (qualification / personalization /
    research) the UI can offer when adding an AI column."""
    from apps.api.services.workbook.ai_column_presets import list_presets, categories
    return {"presets": list_presets(category), "categories": categories()}


@router.get("/meta/filter-options")
async def get_filter_options(ctx: WorkspaceCtx = Depends(current_workspace)):
    """Get available filter values from the leads DB."""
    lead_db = ctx.lead_db()
    try:
        return lead_db.get_filter_options()
    finally:
        lead_db.close()
