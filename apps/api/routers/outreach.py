"""
Outreach Router — Email sequences, SMTP config, and send management.

Endpoints for creating sequences, enrolling leads, executing sends,
and viewing outreach statistics.
"""

from fastapi import APIRouter, HTTPException
from pydantic import BaseModel
from typing import List, Optional
import time

router = APIRouter(prefix="/api/outreach", tags=["outreach"])


# ── Models ────────────────────────────────────────────────────

class StepInput(BaseModel):
    step_number: int
    subject: str
    body_html: str
    delay_hours: int = 0


class CreateSequenceRequest(BaseModel):
    name: str
    description: str = ""
    steps: List[StepInput] = []
    daily_limit: int = 50


class UpdateSequenceRequest(BaseModel):
    name: Optional[str] = None
    description: Optional[str] = None
    steps: Optional[List[StepInput]] = None
    status: Optional[str] = None
    daily_limit: Optional[int] = None


class EnrollLeadsRequest(BaseModel):
    lead_ids: List[int]


class SendTestRequest(BaseModel):
    to_email: str


class SMTPConfigUpdate(BaseModel):
    smtp_host: Optional[str] = None
    smtp_port: Optional[int] = None
    smtp_email: Optional[str] = None
    smtp_password: Optional[str] = None
    smtp_from_name: Optional[str] = None
    smtp_max_per_hour: Optional[int] = None


# ── Sequence CRUD ─────────────────────────────────────────────

@router.get("/sequences")
def list_sequences():
    """List all outreach sequences."""
    from apps.api.services.outreach.sequence import list_sequences as _list
    seqs = _list()
    return {
        "sequences": [
            {
                "id": s.id,
                "name": s.name,
                "description": s.description,
                "status": s.status,
                "steps_count": len(s.steps),
                "total_leads": s.total_leads,
                "total_sent": s.total_sent,
                "total_opened": s.total_opened,
                "total_replied": s.total_replied,
                "total_bounced": s.total_bounced,
                "daily_limit": s.daily_limit,
                "created_at": s.created_at,
            }
            for s in seqs
        ]
    }


@router.post("/sequences")
def create_sequence(req: CreateSequenceRequest):
    """Create a new email sequence."""
    from apps.api.services.outreach.sequence import create_sequence as _create
    seq = _create(
        name=req.name,
        description=req.description,
        steps=[s.model_dump() for s in req.steps],
    )
    return {"id": seq.id, "name": seq.name, "status": seq.status}


@router.get("/sequences/{seq_id}")
def get_sequence(seq_id: str):
    """Get sequence details with steps and stats."""
    from apps.api.services.outreach.sequence import get_sequence as _get, get_sequence_stats
    seq = _get(seq_id)
    if not seq:
        raise HTTPException(status_code=404, detail="Sequence not found")

    stats = get_sequence_stats(seq_id)
    return {
        "id": seq.id,
        "name": seq.name,
        "description": seq.description,
        "status": seq.status,
        "steps": [
            {
                "step_number": s.step_number,
                "subject": s.subject,
                "body_html": s.body_html,
                "delay_hours": s.delay_hours,
            }
            for s in seq.steps
        ],
        "stats": stats,
        "daily_limit": seq.daily_limit,
        "send_window_start": seq.send_window_start,
        "send_window_end": seq.send_window_end,
        "created_at": seq.created_at,
        "updated_at": seq.updated_at,
    }


@router.put("/sequences/{seq_id}")
def update_sequence(seq_id: str, req: UpdateSequenceRequest):
    """Update a sequence."""
    from apps.api.services.outreach.sequence import update_sequence as _update
    updates = req.model_dump(exclude_none=True)
    if "steps" in updates:
        updates["steps"] = [s.model_dump() if hasattr(s, "model_dump") else s for s in updates["steps"]]
    seq = _update(seq_id, updates)
    if not seq:
        raise HTTPException(status_code=404, detail="Sequence not found")
    return {"status": "ok", "id": seq.id}


@router.delete("/sequences/{seq_id}")
def delete_sequence(seq_id: str):
    """Delete a sequence."""
    from apps.api.services.outreach.sequence import delete_sequence as _delete
    _delete(seq_id)
    return {"status": "ok"}


# ── Sequence Actions ──────────────────────────────────────────

@router.post("/sequences/{seq_id}/start")
def start_sequence(seq_id: str):
    """Activate a sequence (start sending)."""
    from apps.api.services.outreach.sequence import update_sequence as _update
    from apps.api.services.outreach.sender import is_smtp_configured
    if not is_smtp_configured():
        raise HTTPException(status_code=400, detail="SMTP not configured. Go to Settings → Email to set up.")
    seq = _update(seq_id, {"status": "active"})
    if not seq:
        raise HTTPException(status_code=404, detail="Sequence not found")
    return {"status": "active", "message": f"Sequence '{seq.name}' is now active"}


@router.post("/sequences/{seq_id}/pause")
def pause_sequence(seq_id: str):
    """Pause a sequence."""
    from apps.api.services.outreach.sequence import update_sequence as _update
    seq = _update(seq_id, {"status": "paused"})
    if not seq:
        raise HTTPException(status_code=404, detail="Sequence not found")
    return {"status": "paused"}


@router.post("/sequences/{seq_id}/enroll")
def enroll_leads(seq_id: str, req: EnrollLeadsRequest):
    """Enroll leads into a sequence."""
    from apps.api.services.outreach.sequence import enroll_leads as _enroll, get_sequence as _get
    seq = _get(seq_id)
    if not seq:
        raise HTTPException(status_code=404, detail="Sequence not found")
    count = _enroll(seq_id, req.lead_ids)
    return {"enrolled": count, "total_lead_ids": len(req.lead_ids)}


@router.post("/sequences/{seq_id}/execute")
async def execute_sequence(seq_id: str):
    """Execute pending sends for a sequence (trigger manually or via cron)."""
    from apps.api.services.outreach.sequence import execute_pending_sends
    results = await execute_pending_sends(seq_id)
    return {"results": results}


@router.get("/sequences/{seq_id}/stats")
def sequence_stats(seq_id: str):
    """Get detailed stats for a sequence."""
    from apps.api.services.outreach.sequence import get_sequence_stats
    return get_sequence_stats(seq_id)


# ── SMTP Config ───────────────────────────────────────────────

@router.get("/smtp/status")
def smtp_status():
    """Check if SMTP is configured."""
    from apps.api.services.outreach.sender import is_smtp_configured, get_smtp_config
    cfg = get_smtp_config()
    return {
        "configured": is_smtp_configured(),
        "host": cfg.host or None,
        "email": cfg.email or None,
        "from_name": cfg.from_name,
        "max_per_hour": cfg.max_per_hour,
    }


@router.put("/smtp/config")
def update_smtp_config(req: SMTPConfigUpdate):
    """Update SMTP configuration."""
    from apps.api.routers.settings import _db_set as set_setting
    if req.smtp_host is not None:
        set_setting("SMTP_HOST", req.smtp_host)
    if req.smtp_port is not None:
        set_setting("SMTP_PORT", str(req.smtp_port))
    if req.smtp_email is not None:
        set_setting("SMTP_EMAIL", req.smtp_email)
    if req.smtp_password is not None:
        set_setting("SMTP_PASSWORD", req.smtp_password)
    if req.smtp_from_name is not None:
        set_setting("SMTP_FROM_NAME", req.smtp_from_name)
    if req.smtp_max_per_hour is not None:
        set_setting("SMTP_MAX_PER_HOUR", str(req.smtp_max_per_hour))
    return {"status": "ok"}


@router.post("/smtp/test")
async def test_smtp(req: SendTestRequest):
    """Send a test email to verify SMTP configuration."""
    from apps.api.services.outreach.sender import send_test_email
    result = await send_test_email(req.to_email)
    if result.success:
        return {"status": "ok", "message": f"Test email sent to {req.to_email}"}
    else:
        raise HTTPException(status_code=400, detail=result.error)
