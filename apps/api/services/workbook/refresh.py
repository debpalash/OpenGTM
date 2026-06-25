"""
Living workbooks (Pillar 3).

A workbook with a `refresh_policy` becomes a standing subscription to a slice of
the market. Recurrence is built on the existing DB-polling queue_service: the
`refresh_workbook` handler does its work then re-enqueues itself at
`next_run_at = now + interval`. No new scheduler infra (APScheduler/Celery).

Refresh does two things:
  1. Re-run every `source` column → append only genuinely-new entities (the
     entity graph dedups, so refresh never duplicates).
  2. Re-enrich only STALE fields (per `staleness_ttl_days`), row-scoped.

Signals become source triggers: `signal_scan` runs the existing monitor, applies
score boosts, and refreshes workbooks whose policy lists the fired signal type.
"""

import logging
from datetime import datetime, timezone, timedelta
from typing import List, Optional

from apps.api.database import SessionLocal
from apps.api.services.workbook.models import Workbook, WorkbookRow, WorkbookEnrichment
from apps.api.services.workbook.activity_models import WorkbookActivity

logger = logging.getLogger("workbook.refresh")

DEFAULT_STALENESS_DAYS = 30
INTERVAL_MINUTES = {"hourly": 60, "daily": 1440, "weekly": 10080}


def _now():
    return datetime.now(timezone.utc)


def log_activity(db, workbook_id: str, kind: str, message: str):
    # Denormalized tenant from the active workspace scope (callers run inside
    # workspace_scope) so the row matches the RLS GUC / WITH CHECK.
    from apps.api.core.tenancy import current_workspace_var
    db.add(WorkbookActivity(
        workbook_id=workbook_id, workspace_id=current_workspace_var.get(),
        kind=kind, message=message,
    ))


def _interval_minutes(policy: dict) -> Optional[int]:
    interval = (policy or {}).get("interval")
    if not interval:
        return None
    if isinstance(interval, (int, float)):
        return int(interval)
    return INTERVAL_MINUTES.get(str(interval).lower())


def _stale_lead_ids(db, workbook_id: str, enrichment_cols: List[dict], ttl_map: dict) -> List[int]:
    """Lead ids with at least one enrichment cell missing or older than its TTL."""
    rows = db.query(WorkbookRow).filter(WorkbookRow.workbook_id == workbook_id).all()
    if not rows:
        return []
    # Index existing enrichments by (lead_id, col)
    overlays = db.query(WorkbookEnrichment).filter(
        WorkbookEnrichment.workbook_id == workbook_id
    ).all()
    seen = {(o.lead_id, o.column_id): o for o in overlays}

    stale = set()
    now = _now()
    for row in rows:
        lead_id = row.lead_id or row.id
        for col in enrichment_cols:
            cid = col.get("id")
            ttl_days = ttl_map.get(col.get("target_field") or cid, DEFAULT_STALENESS_DAYS)
            o = seen.get((lead_id, cid))
            if o is None or o.status != "complete":
                stale.add(lead_id)
                break
            upd = o.updated_at
            if upd is None:
                stale.add(lead_id); break
            if upd.tzinfo is None:
                upd = upd.replace(tzinfo=timezone.utc)
            if upd < now - timedelta(days=ttl_days):
                stale.add(lead_id); break
    return list(stale)


async def refresh_workbook(
    workbook_id: str, reason: str = "scheduled", workspace_id: str = None
) -> dict:
    """One refresh cycle: source new rows + re-enrich stale fields.

    ``workspace_id`` is the tenant this refresh belongs to; it arrives out-of-band
    (job payload / caller), never read off the row. Enter ``workspace_scope``
    FIRST (fail-loud on empty) so the wb/row reads and the chained source/enrich
    work are tenant-scoped for RLS.
    """
    from apps.api.core.tenancy import workspace_scope

    with workspace_scope(workspace_id):
        return await _refresh_workbook_impl(workbook_id, reason, workspace_id)


async def _refresh_workbook_impl(workbook_id: str, reason: str, workspace_id: str) -> dict:
    from apps.api.services.workbook.source_engine import materialize_source
    from apps.api.services.workbook.enrichment import run_workbook_enrichment

    with SessionLocal() as db:
        wb = db.query(Workbook).filter(Workbook.id == workbook_id).first()
        if not wb:
            return {"error": "workbook_not_found"}
        cols = wb.columns_config or []
        policy = wb.refresh_policy or {}
        source_cols = [c for c in cols if c.get("type") == "source"]
        enrichment_cols = [c for c in cols if c.get("type") in ("enrichment", "waterfall", "ai_formula")]
        ttl_map = policy.get("staleness_ttl_days") or {}

    # 1) Re-source (append-only via entity dedup)
    sourced = 0
    for col in source_cols:
        res = await materialize_source(workbook_id, col["id"], workspace_id)
        sourced += res.get("added", 0)

    # 2) Re-enrich stale rows only
    reenriched = 0
    with SessionLocal() as db:
        stale = _stale_lead_ids(db, workbook_id, enrichment_cols, ttl_map) if enrichment_cols else []
    if stale:
        result = await run_workbook_enrichment(workbook_id, lead_ids=stale, workspace_id=workspace_id)
        reenriched = result.get("completed", 0)

    with SessionLocal() as db:
        log_activity(db, workbook_id, "refresh",
                     f"{reason}: +{sourced} new rows, re-enriched {reenriched} stale cells")
        db.commit()

    logger.info(f"Refresh {workbook_id} ({reason}): +{sourced} rows, {reenriched} re-enriched")
    return {"sourced": sourced, "reenriched": reenriched, "stale_rows": len(stale)}


def _enqueue_next(db, workbook_id: str, minutes: int, workspace_id: str):
    """Self-re-enqueue the next refresh via queue_service.next_run_at.

    Stamp ``workspace_id`` into the payload (OD-4) so the handler that picks this
    job up can enter the correct tenant scope.
    """
    from apps.api.models import Job
    job = Job(
        type="refresh_workbook",
        payload={"workbook_id": workbook_id, "workspace_id": workspace_id},
        status="pending",
        priority=1,
        next_run_at=_now() + timedelta(minutes=minutes),
        max_retries=3,
    )
    db.add(job)
    db.commit()


async def handle_refresh_workbook(job_id: int, payload: dict):
    """queue_service handler — runs a refresh, then schedules the next if enabled.

    Worker tenant signal comes from the payload (OD-4): enter ``workspace_scope``
    first and fail loud if ``workspace_id`` is absent.
    """
    from apps.api.core.tenancy import workspace_scope

    workbook_id = payload["workbook_id"]
    workspace_id = payload.get("workspace_id")
    with workspace_scope(workspace_id):
        await refresh_workbook(
            workbook_id, reason=payload.get("reason", "scheduled"), workspace_id=workspace_id
        )
        with SessionLocal() as db:
            wb = db.query(Workbook).filter(Workbook.id == workbook_id).first()
            if not wb:
                return
            policy = wb.refresh_policy or {}
            minutes = _interval_minutes(policy)
            if policy.get("enabled") and minutes:
                _enqueue_next(db, workbook_id, minutes, workspace_id)
                logger.info(f"[job {job_id}] next refresh for {workbook_id} in {minutes}m")


def set_refresh_policy(db, workbook_id: str, policy: dict) -> dict:
    """Persist a refresh policy and (if enabled+interval) kick off the recurring chain."""
    wb = db.query(Workbook).filter(Workbook.id == workbook_id).first()
    if not wb:
        return {"error": "workbook_not_found"}
    wb.refresh_policy = policy
    db.commit()
    minutes = _interval_minutes(policy)
    if policy.get("enabled") and minutes:
        # Called from request/copilotkit paths with the workbook loaded; the
        # workbook's own workspace is the tenant for the recurring chain.
        _enqueue_next(db, workbook_id, minutes, wb.workspace_id)
    return {"refresh_policy": policy, "next_in_minutes": minutes if policy.get("enabled") else None}


# ── Signals → score → action ─────────────────────────────────────────────

async def handle_signal_scan(job_id: int, payload: dict):
    """Run the signal monitor, then trigger workbooks subscribed to fired signals.

    The signal_scan job itself is GLOBAL (it touches only the non-RLS ``jobs``
    table and runs the per-workspace ``run_signal_scan`` from #89). The workbook
    trigger pass must NOT enumerate workbooks globally (``Workbook.all()`` is
    structurally incompatible with per-tenant RLS — it returns zero rows once
    FORCE RLS lands). Instead enumerate workspaces and, per workspace, scope to
    that tenant and trigger only its workbooks (mirrors run_signal_scan's
    per-workspace loop). One workspace failing must not abort the rest.
    """
    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.workspace import manager as ws_manager

    try:
        from apps.api.services.signals.monitor import run_signal_scan
        scan = await run_signal_scan()
    except Exception as e:
        logger.warning(f"signal scan failed: {e}")
        scan = {}

    fired_types = payload.get("signal_types") or ["hiring", "funding", "tech_change", "news"]
    triggered = 0
    try:
        workspaces = ws_manager.list_workspaces()
    except Exception as e:
        logger.warning(f"signal_scan: could not list workspaces: {e}")
        workspaces = []

    for ws in workspaces:
        try:
            with workspace_scope(ws.id):
                with SessionLocal() as db:
                    # Belt (explicit workspace filter) + suspenders (RLS once on).
                    # With RLS off this filter is what keeps the per-workspace
                    # loop from re-triggering every tenant's workbooks N times.
                    wbs = (
                        db.query(Workbook)
                        .filter(Workbook.workspace_id == ws.id)
                        .all()
                    )
                    for wb in wbs:
                        policy = wb.refresh_policy or {}
                        on_signal = set(policy.get("on_signal") or [])
                        if policy.get("enabled") and on_signal & set(fired_types):
                            _enqueue_next_now(db, wb.id, reason="signal", workspace_id=ws.id)
                            log_activity(
                                db, wb.id, "signal",
                                f"signal trigger → refresh ({', '.join(on_signal & set(fired_types))})",
                            )
                            triggered += 1
                    db.commit()
        except Exception as e:
            logger.warning(f"signal_scan: workspace {ws.id} trigger pass failed: {e}")

    # Self-re-enqueue the next periodic scan (default daily).
    interval_min = int(payload.get("interval_minutes", INTERVAL_MINUTES["daily"]))
    with SessionLocal() as db:
        from apps.api.models import Job
        db.add(Job(
            type="signal_scan", payload={"interval_minutes": interval_min},
            status="pending", priority=1,
            next_run_at=_now() + timedelta(minutes=interval_min), max_retries=3,
        ))
        db.commit()
    logger.info(f"[job {job_id}] signal_scan: {scan}, triggered {triggered} workbooks, next in {interval_min}m")


def bootstrap_signal_scan(interval_minutes: int = None):
    """Enqueue the recurring signal scan once, if no scan job is already pending."""
    interval_minutes = interval_minutes or INTERVAL_MINUTES["daily"]
    with SessionLocal() as db:
        from apps.api.models import Job
        pending = db.query(Job).filter(
            Job.type == "signal_scan", Job.status.in_(["pending", "processing"])
        ).count()
        if pending:
            return False
        db.add(Job(
            type="signal_scan", payload={"interval_minutes": interval_minutes},
            status="pending", priority=1,
            next_run_at=_now() + timedelta(minutes=interval_minutes), max_retries=3,
        ))
        db.commit()
    logger.info(f"bootstrapped recurring signal_scan (every {interval_minutes}m)")
    return True


def _enqueue_next_now(db, workbook_id: str, reason: str, workspace_id: str):
    from apps.api.models import Job
    db.add(Job(
        type="refresh_workbook",
        payload={"workbook_id": workbook_id, "reason": reason, "workspace_id": workspace_id},
        status="pending", priority=2, next_run_at=_now(), max_retries=3,
    ))
