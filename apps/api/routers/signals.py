"""
Signals Router — Buying signal feed and management.

Reads/writes go through the unified, workspace-scoped ORM signal store
(:func:`apps.api.services.signals.store.get_signal_store`) on BOTH backends —
Postgres (RLS-protected ``signals`` table) and SQLite/self-host (same ORM table,
``workspace_id`` belt filter is the isolation). The legacy ``data/signals.db``
file path is no longer read or written by the feed.
"""

from fastapi import APIRouter, Depends, Query
from pydantic import BaseModel
from typing import List, Optional
from datetime import datetime, timezone

from apps.api.core.tenancy import WorkspaceCtx, current_workspace, require_workspace_role
from apps.api.core.security import get_current_admin_user
from apps.api.database import get_db
from sqlalchemy import Integer, cast, func
from sqlalchemy.orm import Session

router = APIRouter(prefix="/api/signals", tags=["signals"])
require_editor = require_workspace_role("editor", "admin", permission="signals.write")


@router.get("/analytics")
def signal_analytics(
    days: int = Query(default=30, ge=7, le=90),
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    """Workspace-scoped signal volume, weighted momentum and account rankings."""
    import time
    from apps.api.services.leadgen.orm_models import SignalRow
    cutoff = time.time() - days * 86400
    base = db.query(SignalRow).filter(SignalRow.workspace_id == ctx.workspace_id, SignalRow.created_at >= cutoff)
    total, weighted, accounts = base.with_entities(func.count(SignalRow.id), func.coalesce(func.sum(SignalRow.weight), 0), func.count(func.distinct(SignalRow.lead_id))).one()
    by_type = [{"signal_type": kind or "unknown", "count": count, "weight": int(weight or 0)} for kind, count, weight in base.with_entities(SignalRow.signal_type, func.count(SignalRow.id), func.sum(SignalRow.weight)).group_by(SignalRow.signal_type).order_by(func.count(SignalRow.id).desc()).all()]
    by_source = [{"source": source or "unknown", "count": count} for source, count in base.with_entities(SignalRow.source, func.count(SignalRow.id)).group_by(SignalRow.source).order_by(func.count(SignalRow.id).desc()).limit(10).all()]
    top_accounts = [{"lead_id": lead_id, "company": company or f"Lead {lead_id}", "count": count, "weight": int(weight or 0)} for lead_id, company, count, weight in base.with_entities(SignalRow.lead_id, SignalRow.company, func.count(SignalRow.id), func.sum(SignalRow.weight)).group_by(SignalRow.lead_id, SignalRow.company).order_by(func.sum(SignalRow.weight).desc(), func.count(SignalRow.id).desc()).limit(10).all()]
    day_bucket = cast(SignalRow.created_at / 86400, Integer)
    buckets = {int(day): {"count": int(count), "weight": int(weight or 0)} for day, count, weight in base.with_entities(day_bucket, func.count(SignalRow.id), func.sum(SignalRow.weight)).group_by(day_bucket).all()}
    today = int(time.time() // 86400)
    trend = [{"date": datetime.fromtimestamp(day * 86400, timezone.utc).date().isoformat(), **buckets.get(day, {"count": 0, "weight": 0})} for day in range(today - days + 1, today + 1)]
    previous_cutoff = cutoff - days * 86400
    previous = db.query(func.count(SignalRow.id)).filter(SignalRow.workspace_id == ctx.workspace_id, SignalRow.created_at >= previous_cutoff, SignalRow.created_at < cutoff).scalar() or 0
    momentum = None if previous == 0 else round((int(total) - int(previous)) / int(previous) * 100, 1)
    return {"period_days": days, "summary": {"total": int(total), "weighted_score": int(weighted), "active_accounts": int(accounts), "momentum_pct": momentum}, "trend": trend, "by_type": by_type, "by_source": by_source, "top_accounts": top_accounts}


@router.get("")
def list_signals(
    signal_type: Optional[str] = None,
    lead_id: Optional[int] = None,
    limit: int = 50,
    offset: int = 0,
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    """Get recent signals for THIS workspace, optionally filtered by type or lead."""
    from apps.api.services.signals.monitor import SIGNAL_TYPES
    from apps.api.services.signals.store import get_signal_store

    store = get_signal_store(ctx.workspace_id)
    signals = store.get_signals(
        signal_type=signal_type, lead_id=lead_id, limit=limit, offset=offset
    )
    counts = store.get_signal_counts()
    return {
        "signals": signals,
        "counts": counts,
        "signal_types": {k: v["label"] for k, v in SIGNAL_TYPES.items()},
    }


@router.post("/scan")
def trigger_scan(_admin=Depends(get_current_admin_user)):
    """Trigger a signal scan on the durable queue worker.

    The scan fans out to job-board providers (JobSpy etc.) per lead, which can
    take a long time and do blocking I/O — running it inline (or as a main-loop
    BackgroundTask) froze the API. We enqueue it on the queue worker (which runs
    handlers off the event loop in their own thread) and return immediately; the
    client polls GET /api/signals for results.
    """
    from apps.api.database import SessionLocal
    from apps.api.services.queue_service import queue_service
    with SessionLocal() as db:
        queue_service.add_job(db, "signal_scan", {})
    return {"status": "started"}


class MarkReadRequest(BaseModel):
    signal_ids: List[str]


@router.post("/mark-read")
def mark_signals_read(req: MarkReadRequest, ctx: WorkspaceCtx = Depends(require_editor)):
    """Mark signals as read (within this workspace)."""
    from apps.api.services.signals.store import get_signal_store

    get_signal_store(ctx.workspace_id).mark_signals_read(req.signal_ids)
    return {"status": "ok", "count": len(req.signal_ids)}


@router.get("/counts")
def signal_counts(ctx: WorkspaceCtx = Depends(current_workspace)):
    """Get signal counts by type (within this workspace)."""
    from apps.api.services.signals.store import get_signal_store

    return get_signal_store(ctx.workspace_id).get_signal_counts()
