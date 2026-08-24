"""Snapshot AmbitionBox company-search results into a workbook.

AmbitionBox returns company intelligence, not corporate contact records.  Keep
the complete source payload in self-contained ``WorkbookRow`` data instead of
forcing it through ``Lead`` (which would discard ratings/review counts and can
overwrite an existing, enriched lead with empty contact fields).
"""

from __future__ import annotations

import logging
import math
from datetime import datetime, timezone
from typing import Any, Dict, Iterable, List, Optional

from sqlalchemy.exc import IntegrityError

from apps.api.core.tenancy import workspace_scope
from apps.api.database import SessionLocal
from apps.api.services.connectors.contracts import ConnectorRecord
from apps.api.services.workbook.models import ConnectorRun, Workbook, WorkbookRow
from apps.api.services.workbook.source_ingest import upsert_source_records

logger = logging.getLogger("workbook.ambitionbox_import")


class AmbitionBoxImportAlreadyRunning(RuntimeError):
    """A workbook already has an active AmbitionBox mutation."""

    def __init__(self, run_id: str):
        self.run_id = run_id
        super().__init__(f"AmbitionBox import {run_id} is already active")


AMBITIONBOX_COLUMNS = [
    {"id": "company", "name": "Company", "type": "lead_field", "lead_field": "company", "width": 220},
    {"id": "industry", "name": "Industry", "type": "lead_field", "lead_field": "industry", "width": 180},
    {"id": "ambitionbox_rating", "name": "Rating", "type": "lead_field", "lead_field": "ambitionbox_rating", "width": 90},
    {"id": "review_count", "name": "Reviews", "type": "lead_field", "lead_field": "review_count", "width": 100},
    {"id": "company_size", "name": "Employees", "type": "lead_field", "lead_field": "company_size", "width": 130},
    {"id": "city", "name": "Top location", "type": "lead_field", "lead_field": "city", "width": 150},
    {"id": "jobs_count", "name": "Jobs", "type": "lead_field", "lead_field": "jobs_count", "width": 80},
    {"id": "company_type", "name": "Company type", "type": "lead_field", "lead_field": "company_type", "width": 150},
    {"id": "is_verified", "name": "Verified", "type": "lead_field", "lead_field": "is_verified", "width": 90},
    {"id": "ambitionbox_url", "name": "AmbitionBox", "type": "lead_field", "lead_field": "ambitionbox_url", "width": 260},
]


def _dedupe_companies(companies: Iterable[dict]) -> List[dict]:
    """Drop empty/duplicate cards while preserving AmbitionBox ranking order."""
    unique: List[dict] = []
    seen = set()
    for company in companies:
        if not isinstance(company, dict):
            continue
        name = str(company.get("name") or "").strip()
        if not name:
            continue
        company_id = company.get("company_id")
        key = ("id", str(company_id)) if company_id not in (None, "", 0) else (
            "name", name.casefold()
        )
        if key in seen:
            continue
        seen.add(key)
        unique.append(company)
    return unique


def company_to_row(company: dict) -> Dict[str, Any]:
    """Map an AmbitionBox card to workbook fields without inventing a website."""
    return {
        "company": str(company.get("name") or "").strip(),
        "industry": company.get("industry") or "",
        # Also populate the standard lead fields so later enrichment columns
        # can consume this row without source-specific adapters.
        "specialization": company.get("industry") or "",
        "industry_tags": company.get("industry") or "",
        "company_size": company.get("employee_count") or "",
        "city": company.get("top_location") or "",
        "source": "ambitionbox",
        # Source-native intelligence remains first-class workbook data.
        "ambitionbox_company_id": company.get("company_id"),
        "ambitionbox_rating": company.get("rating"),
        "review_count": company.get("review_count"),
        "jobs_count": company.get("jobs_count") or 0,
        "salaries_count": company.get("salaries_count") or 0,
        "interviews_count": company.get("interviews_count") or 0,
        "total_locations": company.get("total_locations") or 0,
        "company_type": company.get("company_type") or "",
        "is_verified": bool(company.get("is_verified")),
        "ambitionbox_url": company.get("profile_url") or "",
        "logo_url": company.get("logo_url") or "",
        "highly_rated_for": company.get("highly_rated_for") or [],
        "critically_rated_for": company.get("critically_rated_for") or [],
    }


def _company_record(company: dict, rank: int) -> ConnectorRecord:
    company_id = company.get("company_id")
    record_id = (
        str(company_id)
        if company_id not in (None, "", 0)
        else "name:" + str(company.get("name") or "").strip().casefold()
    )
    return ConnectorRecord(
        provider="ambitionbox",
        record_id=record_id,
        data=company_to_row(company),
        rank=rank,
        source_url=company.get("profile_url") or None,
    )


def create_ambitionbox_workbook(
    companies: Iterable[dict],
    *,
    workspace_id: str,
    name: str,
    industry: Optional[str] = None,
    rating: Optional[str] = None,
    sort_by: str = "popular",
    requested_limit: Optional[int] = None,
) -> Dict[str, Any]:
    """Create a tenant-scoped workbook containing the supplied ranked cards."""
    ranked = _dedupe_companies(companies)
    if not ranked:
        raise ValueError("AmbitionBox returned no companies; workbook was not created")

    with workspace_scope(workspace_id):
        with SessionLocal() as db:
            source_config = {
                "provider": "ambitionbox",
                "industry": industry,
                "rating": rating,
                "sort_by": sort_by,
                "requested_limit": requested_limit,
                "returned": len(ranked),
            }
            wb = Workbook(
                name=name.strip() or "AmbitionBox Companies",
                description=(
                    f"AmbitionBox company search"
                    + (f" for {industry}" if industry else "")
                ),
                workspace_id=workspace_id,
                source_type="ambitionbox",
                source_config=source_config,
                filter_criteria={},
                columns_config=AMBITIONBOX_COLUMNS,
                total_rows=len(ranked),
                sync_to_leads=False,
                status="draft",
            )
            db.add(wb)
            db.flush()

            ingest = upsert_source_records(
                db,
                workbook_id=wb.id,
                workspace_id=workspace_id,
                provider="ambitionbox",
                records=[
                    _company_record(company, position)
                    for position, company in enumerate(ranked)
                ],
            )
            row_ids = list(ingest.added_row_ids)
            db.commit()
            db.refresh(wb)
            workbook_id = wb.id
            workbook_name = wb.name

    # Match every other workbook materialization path: row-added automations are
    # best effort and must never roll back a successfully-created workbook.
    try:
        from apps.api.services.automations import events as automation_events

        with workspace_scope(workspace_id):
            automation_events.emit_row_added(workspace_id, workbook_id, row_ids)
    except Exception as exc:
        logger.warning("on_row_added emit (AmbitionBox import) failed: %s", exc)

    return {
        "workbook_id": workbook_id,
        "name": workbook_name,
        "rows_added": len(ranked),
        "requested_limit": requested_limit,
        "source": "ambitionbox",
        "url": f"/workbooks/{workbook_id}",
    }


def start_ambitionbox_import(
    *,
    workspace_id: str,
    name: str,
    industry: Optional[str] = None,
    rating: Optional[str] = None,
    sort_by: str = "popular",
    requested_limit: int = 100,
    workbook_id: Optional[str] = None,
    replace_existing: bool = False,
) -> Dict[str, Any]:
    """Create a durable connector run and enqueue it atomically."""
    requested_limit = max(1, min(int(requested_limit), 500))
    max_pages = min(25, max(1, math.ceil(requested_limit / 20) + 5))
    with workspace_scope(workspace_id):
        try:
            with SessionLocal() as db:
                wb = None
                if workbook_id:
                    wb_query = db.query(Workbook).filter(
                        Workbook.id == workbook_id
                    )
                    if db.get_bind().dialect.name == "postgresql":
                        # Serialize starts for this workbook across API replicas.
                        wb_query = wb_query.with_for_update()
                    wb = wb_query.first()
                    if not wb or wb.workspace_id != workspace_id:
                        raise ValueError(
                            "workbook not found in the active workspace"
                        )
                    active = db.query(ConnectorRun).filter(
                        ConnectorRun.workbook_id == workbook_id,
                        ConnectorRun.connector == "ambitionbox",
                        ConnectorRun.status.in_(
                            ("pending", "running", "retrying")
                        ),
                    ).order_by(ConnectorRun.created_at.desc()).first()
                    if active:
                        raise AmbitionBoxImportAlreadyRunning(active.id)
                    if (
                        not replace_existing
                        and wb.source_type not in {"ambitionbox"}
                    ):
                        raise ValueError(
                            "existing non-AmbitionBox workbooks require "
                            "replace_existing=true"
                        )
                    wb.name = name.strip() or wb.name
                    wb.columns_config = AMBITIONBOX_COLUMNS
                    wb.source_type = "ambitionbox"
                    wb.status = "running"
                else:
                    wb = Workbook(
                        name=name.strip() or "AmbitionBox Companies",
                        description=(
                            "Durable AmbitionBox company import"
                            + (f" for {industry}" if industry else "")
                        ),
                        workspace_id=workspace_id,
                        source_type="ambitionbox",
                        source_config={},
                        filter_criteria={},
                        columns_config=AMBITIONBOX_COLUMNS,
                        total_rows=0,
                        sync_to_leads=False,
                        status="running",
                    )
                    db.add(wb)
                    db.flush()

                query = {
                    "industry": industry,
                    "rating": rating,
                    "sort_by": sort_by,
                    "max_pages": max_pages,
                    "replace_existing": bool(replace_existing),
                }
                run = ConnectorRun(
                    workspace_id=workspace_id,
                    workbook_id=wb.id,
                    connector="ambitionbox",
                    status="pending",
                    query=query,
                    cursor={
                        "next_page": 1,
                        "reset_done": False,
                        "seen_record_ids": [],
                    },
                    requested_count=requested_limit,
                )
                db.add(run)
                db.flush()
                wb.source_config = {
                    "provider": "ambitionbox",
                    **query,
                    "requested_limit": requested_limit,
                    "active_run_id": run.id,
                }

                from apps.api.services.queue_service import queue_service

                job = queue_service.add_job(
                    db,
                    "ambitionbox_import",
                    {
                        "workspace_id": workspace_id,
                        "workbook_id": wb.id,
                        "run_id": run.id,
                    },
                    priority=2,
                )
                return {
                    "workbook_id": wb.id,
                    "name": wb.name,
                    "run_id": run.id,
                    "job_id": job.id,
                    "status": "pending",
                    "requested_limit": requested_limit,
                    "source": "ambitionbox",
                    "url": f"/workbooks/{wb.id}",
                }
        except IntegrityError as exc:
            # SQLite cannot row-lock the parent workbook; the partial unique
            # index is the final arbiter there (and defense in depth on PG).
            with SessionLocal() as db:
                active = db.query(ConnectorRun).filter(
                    ConnectorRun.workbook_id == workbook_id,
                    ConnectorRun.connector == "ambitionbox",
                    ConnectorRun.status.in_(("pending", "running", "retrying")),
                ).order_by(ConnectorRun.created_at.desc()).first()
                if active:
                    raise AmbitionBoxImportAlreadyRunning(active.id) from exc
            raise


def reconcile_ambitionbox_job_failure(
    job_id: int,
    payload: dict,
    error: str,
    will_retry: bool,
) -> None:
    """Make connector/workbook state agree with the durable queue outcome."""
    workspace_id = str(payload.get("workspace_id") or "").strip()
    workbook_id = str(payload.get("workbook_id") or "").strip()
    run_id = str(payload.get("run_id") or "").strip()
    if not workspace_id or not workbook_id or not run_id:
        logger.error("job %s has invalid AmbitionBox failure payload", job_id)
        return
    with workspace_scope(workspace_id):
        with SessionLocal() as db:
            run = db.query(ConnectorRun).filter(
                ConnectorRun.id == run_id,
                ConnectorRun.workbook_id == workbook_id,
            ).first()
            wb = db.query(Workbook).filter(Workbook.id == workbook_id).first()
            if not run or not wb or run.status in {"complete", "partial"}:
                return
            run.status = "retrying" if will_retry else "failed"
            run.error = str(error)[:1000]
            run.completed_at = None if will_retry else datetime.now(timezone.utc)
            wb.status = "running" if will_retry else "draft"
            wb.source_config = {
                **(wb.source_config or {}),
                "run_status": run.status,
                "last_error": run.error,
                "target_met": bool(run.target_met),
            }
            db.commit()


async def handle_ambitionbox_import(job_id: int, payload: dict) -> Dict[str, Any]:
    """Resume an AmbitionBox import from its last committed page cursor."""
    workspace_id = str(payload.get("workspace_id") or "").strip()
    workbook_id = str(payload.get("workbook_id") or "").strip()
    run_id = str(payload.get("run_id") or "").strip()
    if not workspace_id or not workbook_id or not run_id:
        raise ValueError("ambitionbox_import requires workspace_id, workbook_id, run_id")

    from apps.api.services.leadgen.ambitionbox import ambitionbox
    from apps.api.services.workbook.enrichment import _broadcast, _make_redis

    with workspace_scope(workspace_id):
        try:
            while True:
                with SessionLocal() as db:
                    run = db.query(ConnectorRun).filter(
                        ConnectorRun.id == run_id,
                        ConnectorRun.workbook_id == workbook_id,
                    ).first()
                    wb = db.query(Workbook).filter(Workbook.id == workbook_id).first()
                    if not run or not wb:
                        raise ValueError("connector run or workbook not found")
                    if run.status in {"complete", "partial"}:
                        return run.to_api()
                    run.status = "running"
                    run.error = None
                    run.completed_at = None
                    run.started_at = run.started_at or datetime.now(timezone.utc)
                    wb.status = "running"
                    wb.source_config = {
                        **(wb.source_config or {}),
                        "run_status": "running",
                        "last_error": None,
                    }
                    cursor = dict(run.cursor or {})
                    page_number = max(1, int(cursor.get("next_page") or 1))
                    query = dict(run.query or {})
                    db.commit()

                page = await ambitionbox.fetch_company_page(
                    page=page_number,
                    industry=[query["industry"]] if query.get("industry") else None,
                    sort_by=query.get("sort_by") or "popular",
                    rating=query.get("rating"),
                )

                added_ids: tuple[int, ...] = ()
                done = False
                with SessionLocal() as db:
                    run = db.query(ConnectorRun).filter(
                        ConnectorRun.id == run_id,
                        ConnectorRun.workbook_id == workbook_id,
                    ).first()
                    wb = db.query(Workbook).filter(Workbook.id == workbook_id).first()
                    if not run or not wb:
                        raise ValueError("connector run or workbook disappeared")
                    cursor = dict(run.cursor or {})
                    # Another retry/process may have committed this page first.
                    if int(cursor.get("next_page") or 1) != page_number:
                        continue
                    reset_rows = bool(
                        query.get("replace_existing")
                        and page_number == 1
                        and not cursor.get("reset_done")
                    )
                    seen_ids = {
                        str(value) for value in (cursor.get("seen_record_ids") or [])
                    }
                    remaining = max(0, int(run.requested_count) - len(seen_ids))
                    selected = []
                    if remaining:
                        for record in page.records:
                            if record.record_id in seen_ids:
                                continue
                            selected.append(record)
                            seen_ids.add(record.record_id)
                            if len(selected) >= remaining:
                                break
                    records = [
                        ConnectorRecord(
                            provider="ambitionbox",
                            record_id=record.record_id,
                            data=company_to_row(record.as_dict()),
                            rank=record.rank,
                            source_url=record.source_url,
                            fetched_at=record.fetched_at,
                        )
                        for record in selected
                    ]
                    ingest = upsert_source_records(
                        db,
                        workbook_id=workbook_id,
                        workspace_id=workspace_id,
                        provider="ambitionbox",
                        records=records,
                        reset_rows=reset_rows,
                    )
                    added_ids = ingest.added_row_ids
                    fetched_count = db.query(WorkbookRow).filter(
                        WorkbookRow.workbook_id == workbook_id,
                        WorkbookRow.source_provider == "ambitionbox",
                    ).count()
                    cursor = {
                        "next_page": page_number + 1,
                        "reset_done": bool(cursor.get("reset_done") or reset_rows),
                        "seen_record_ids": sorted(seen_ids),
                    }
                    run.cursor = cursor
                    run.pages_fetched = int(run.pages_fetched or 0) + 1
                    run.fetched_count = len(seen_ids)
                    run.added_count = int(run.added_count or 0) + ingest.added
                    run.updated_count = int(run.updated_count or 0) + ingest.updated
                    run.skipped_count = int(run.skipped_count or 0) + ingest.skipped
                    run.source_total = page.source_total
                    run.target_met = run.fetched_count >= run.requested_count
                    run.exhausted = not page.has_more
                    max_pages = int(query.get("max_pages") or 25)
                    max_reached = run.pages_fetched >= max_pages
                    done = bool(run.target_met or run.exhausted or max_reached)
                    if done:
                        run.status = "complete" if run.target_met else "partial"
                        run.completed_at = datetime.now(timezone.utc)
                        if max_reached and not run.target_met and not run.exhausted:
                            run.error = "maximum page budget reached before requested target"
                        wb.status = "draft"
                    else:
                        run.status = "running"
                    wb.total_rows = db.query(WorkbookRow).filter(
                        WorkbookRow.workbook_id == workbook_id
                    ).count()
                    wb.source_config = {
                        **(wb.source_config or {}),
                        "returned": fetched_count,
                        "pages_fetched": run.pages_fetched,
                        "source_total": run.source_total,
                        "target_met": bool(run.target_met),
                        "run_status": run.status,
                    }
                    db.commit()
                    result = run.to_api()

                if added_ids:
                    try:
                        from apps.api.services.automations import events as automation_events
                        automation_events.emit_row_added(
                            workspace_id, workbook_id, list(added_ids)
                        )
                    except Exception as exc:
                        logger.warning("on_row_added emit (AmbitionBox page) failed: %s", exc)
                redis_client = _make_redis()
                if redis_client is not None:
                    try:
                        await _broadcast(redis_client, workbook_id, {
                            "type": "source_page",
                            "connector": "ambitionbox",
                            "runId": run_id,
                            "page": page_number,
                            "added": len(added_ids),
                            "total": result["fetched_count"],
                            "done": done,
                        })
                    finally:
                        await redis_client.aclose()
                if done:
                    logger.info(
                        "[job %s] AmbitionBox import %s finished status=%s rows=%s",
                        job_id, run_id, result["status"], result["fetched_count"],
                    )
                    return result
        except Exception as exc:
            with SessionLocal() as db:
                run = db.query(ConnectorRun).filter(
                    ConnectorRun.id == run_id,
                    ConnectorRun.workbook_id == workbook_id,
                ).first()
                if run and run.status not in {"complete", "partial"}:
                    # Keep the run active until the parent worker atomically
                    # decides retry-vs-terminal. This closes the small window
                    # in which a second import could start between child exit
                    # and queue reconciliation.
                    run.status = "retrying"
                    run.error = str(exc)[:1000]
                    run.completed_at = None
                    wb = db.query(Workbook).filter(
                        Workbook.id == workbook_id
                    ).first()
                    if wb:
                        wb.status = "running"
                        wb.source_config = {
                            **(wb.source_config or {}),
                            "run_status": "retrying",
                            "last_error": run.error,
                            "target_met": bool(run.target_met),
                        }
                    db.commit()
            raise
