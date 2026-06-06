"""
Signals Router — Buying signal feed and management.
"""

from fastapi import APIRouter, Depends, BackgroundTasks
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
async def trigger_scan(
    background_tasks: BackgroundTasks,
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    """Trigger a signal scan in the background.

    The scan fans out to job-board providers (JobSpy etc.) per lead, which can
    take a long time — running it inline blocked the request (and the single dev
    worker) until it finished. We enqueue it and return immediately; the client
    polls GET /api/signals for the results.
    """
    from apps.api.services.signals.monitor import run_signal_scan
    background_tasks.add_task(run_signal_scan)
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
