"""
Signals Router — Buying signal feed and management.
"""

from fastapi import APIRouter, Depends
from pydantic import BaseModel
from typing import List, Optional

from apps.api.core.tenancy import WorkspaceCtx, current_workspace

router = APIRouter(prefix="/api/signals", tags=["signals"])


@router.get("")
def list_signals(
    signal_type: Optional[str] = None,
    lead_id: Optional[int] = None,
    limit: int = 50,
    offset: int = 0,
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    """Get recent signals, optionally filtered by type or lead."""
    from apps.api.services.signals.monitor import get_signals, get_signal_counts, SIGNAL_TYPES
    signals = get_signals(signal_type=signal_type, lead_id=lead_id, limit=limit, offset=offset)
    counts = get_signal_counts()
    return {
        "signals": signals,
        "counts": counts,
        "signal_types": {k: v["label"] for k, v in SIGNAL_TYPES.items()},
    }


@router.post("/scan")
def trigger_scan(ctx: WorkspaceCtx = Depends(current_workspace)):
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
def mark_signals_read(req: MarkReadRequest, ctx: WorkspaceCtx = Depends(current_workspace)):
    """Mark signals as read."""
    from apps.api.services.signals.monitor import mark_read
    mark_read(req.signal_ids)
    return {"status": "ok", "count": len(req.signal_ids)}


@router.get("/counts")
def signal_counts(ctx: WorkspaceCtx = Depends(current_workspace)):
    """Get signal counts by type."""
    from apps.api.services.signals.monitor import get_signal_counts
    return get_signal_counts()
