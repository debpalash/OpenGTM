"""Outreach Router — RLS-hardened email sequences, SMTP, suppression, unsubscribe.

Every endpoint is workspace-scoped via ``Depends(current_workspace)`` (reads) or
``Depends(require_workspace_role("admin"))`` (mutations), EXCEPT the public
unsubscribe + provider bounce webhook endpoints which authenticate via signed
token / platform secret and derive the workspace from the verified payload only.
"""

import logging
import time
from typing import List, Optional

from fastapi import APIRouter, Depends, HTTPException, Query, Request
from fastapi.responses import HTMLResponse
from pydantic import BaseModel
from sqlalchemy.orm import Session

from apps.api.core.config import settings
from apps.api.core.tenancy import (
    WorkspaceCtx,
    current_workspace,
    require_workspace_role,
    workspace_scope,
)
from apps.api.database import get_db
from apps.api.services.outreach.orm_models import OutreachDraft
from apps.api.services.outreach.normalize import normalize_email
from apps.api.services.outreach.store import get_outreach_store

logger = logging.getLogger("outreach.api")
router = APIRouter(prefix="/api/outreach", tags=["outreach"])

require_admin = require_workspace_role("admin")


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
    send_window_start: int = 9
    send_window_end: int = 18
    send_window_tz: str = "UTC"
    consent_basis: str = ""


class UpdateSequenceRequest(BaseModel):
    name: Optional[str] = None
    description: Optional[str] = None
    steps: Optional[List[StepInput]] = None
    status: Optional[str] = None
    daily_limit: Optional[int] = None
    send_window_start: Optional[int] = None
    send_window_end: Optional[int] = None
    send_window_tz: Optional[str] = None
    consent_basis: Optional[str] = None


class EnrollLeadsRequest(BaseModel):
    lead_ids: List[int]
    consent_source: str = ""


class SendTestRequest(BaseModel):
    to_email: str


class SMTPConfigUpdate(BaseModel):
    smtp_host: Optional[str] = None
    smtp_port: Optional[int] = None
    smtp_email: Optional[str] = None
    smtp_password: Optional[str] = None
    smtp_from_name: Optional[str] = None
    smtp_max_per_hour: Optional[int] = None
    smtp_use_tls: Optional[bool] = None


class SuppressionAdd(BaseModel):
    email: str


def _draft_to_api(draft: OutreachDraft, *, include_evidence: bool) -> dict:
    result = {
        "id": draft.id,
        "person_id": draft.person_id,
        "person_name": draft.person_name,
        "company": draft.company,
        "title": draft.title or "",
        "to_email": draft.to_email,
        "contact_status": draft.contact_status,
        "risky_approved": bool(draft.risky_approved),
        "generic_inbox": bool(draft.generic_inbox),
        "is_role_address": bool(draft.is_role_address),
        "subject": draft.subject,
        "body_text": draft.body_text,
        "state": draft.state,
        "sent_at": draft.sent_at.isoformat() if draft.sent_at else None,
        "send_performed": draft.sent_at is not None or draft.state == "sent",
        "created_at": draft.created_at.isoformat() if draft.created_at else None,
        "updated_at": draft.updated_at.isoformat() if draft.updated_at else None,
    }
    if include_evidence:
        result["sentence_evidence"] = list(draft.sentence_evidence or [])
        result["source_snapshot"] = dict(draft.source_snapshot or {})
    return result


# ── Grounded draft inspection (draft-only; no send endpoint) ──────────────

@router.get("/drafts")
def list_drafts(
    limit: int = Query(50, ge=1, le=200),
    offset: int = Query(0, ge=0),
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    rows = db.query(OutreachDraft).filter(
        OutreachDraft.workspace_id == ctx.workspace_id,
    ).order_by(OutreachDraft.created_at.desc()).limit(limit).offset(offset).all()
    return {
        "drafts": [_draft_to_api(row, include_evidence=False) for row in rows],
        "limit": limit,
        "offset": offset,
    }


@router.get("/drafts/{draft_id}")
def get_draft(
    draft_id: str,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    draft = db.query(OutreachDraft).filter(
        OutreachDraft.id == draft_id,
        OutreachDraft.workspace_id == ctx.workspace_id,
    ).one_or_none()
    if draft is None:
        raise HTTPException(status_code=404, detail="Draft not found")
    return _draft_to_api(draft, include_evidence=True)


# ── Sequence CRUD ─────────────────────────────────────────────

@router.get("/sequences")
def list_sequences(ctx: WorkspaceCtx = Depends(current_workspace)):
    store = get_outreach_store(ctx.workspace_id)
    return {"sequences": store.list_sequences()}


@router.post("/sequences")
def create_sequence(req: CreateSequenceRequest, ctx: WorkspaceCtx = Depends(require_admin)):
    store = get_outreach_store(ctx.workspace_id)
    seq = store.create_sequence(
        name=req.name,
        description=req.description,
        steps=[s.model_dump() for s in req.steps],
        daily_limit=req.daily_limit,
        send_window_start=req.send_window_start,
        send_window_end=req.send_window_end,
        send_window_tz=req.send_window_tz,
        consent_basis=req.consent_basis,
    )
    return {"id": seq["id"], "name": seq["name"], "status": seq["status"]}


@router.get("/sequences/{seq_id}")
def get_sequence(seq_id: str, ctx: WorkspaceCtx = Depends(current_workspace)):
    store = get_outreach_store(ctx.workspace_id)
    seq = store.get_sequence(seq_id)
    if not seq:
        raise HTTPException(status_code=404, detail="Sequence not found")
    seq["stats"] = store.get_sequence_stats(seq_id)
    return seq


@router.put("/sequences/{seq_id}")
def update_sequence(seq_id: str, req: UpdateSequenceRequest, ctx: WorkspaceCtx = Depends(require_admin)):
    store = get_outreach_store(ctx.workspace_id)
    updates = req.model_dump(exclude_none=True)
    if "steps" in updates:
        updates["steps"] = [
            s.model_dump() if hasattr(s, "model_dump") else s for s in updates["steps"]
        ]
    seq = store.update_sequence(seq_id, updates)
    if not seq:
        raise HTTPException(status_code=404, detail="Sequence not found")
    return {"status": "ok", "id": seq["id"]}


@router.delete("/sequences/{seq_id}")
def delete_sequence(seq_id: str, ctx: WorkspaceCtx = Depends(require_admin)):
    store = get_outreach_store(ctx.workspace_id)
    if not store.delete_sequence(seq_id):
        raise HTTPException(status_code=404, detail="Sequence not found")
    return {"status": "ok"}


# ── Sequence Actions ──────────────────────────────────────────

@router.post("/sequences/{seq_id}/start")
def start_sequence(seq_id: str, ctx: WorkspaceCtx = Depends(require_admin)):
    from apps.api.services.outreach.sender import is_smtp_configured
    from apps.api.services.workspace.secrets import get_secret
    from apps.api.services.outreach.sending import tick_sequence

    store = get_outreach_store(ctx.workspace_id)
    seq = store.get_sequence(seq_id)
    if not seq:
        raise HTTPException(status_code=404, detail="Sequence not found")
    if not is_smtp_configured(ctx.workspace_id):
        raise HTTPException(status_code=400, detail="SMTP not configured")
    if not (seq.get("consent_basis") or "").strip():
        raise HTTPException(status_code=400, detail="consent_basis required before activating")
    if not (get_secret(ctx.workspace_id, "OUTREACH_FOOTER", "") or "").strip():
        raise HTTPException(status_code=400, detail="OUTREACH_FOOTER (physical address) required")
    store.set_sequence_status(seq_id, "active")
    # Enable the ticker mirror + kick the first tick.
    try:
        tick_sequence(ctx.workspace_id, seq_id)
    except Exception as e:
        logger.warning("initial tick failed for %s: %s", seq_id, e)
    return {"status": "active"}


@router.post("/sequences/{seq_id}/pause")
def pause_sequence(seq_id: str, ctx: WorkspaceCtx = Depends(require_admin)):
    store = get_outreach_store(ctx.workspace_id)
    seq = store.set_sequence_status(seq_id, "paused")
    if not seq:
        raise HTTPException(status_code=404, detail="Sequence not found")
    store.disable_schedule(seq_id)
    return {"status": "paused"}


@router.post("/sequences/{seq_id}/enroll")
def enroll_leads(seq_id: str, req: EnrollLeadsRequest, ctx: WorkspaceCtx = Depends(require_admin)):
    from datetime import datetime, timezone

    store = get_outreach_store(ctx.workspace_id)
    if not store.sequence_exists(seq_id):
        raise HTTPException(status_code=404, detail="Sequence not found")
    lead_store = ctx.lead_db()
    enrolled, skipped = 0, []
    now = datetime.now(timezone.utc)
    for lead_id in req.lead_ids:
        lead = lead_store.get_lead(lead_id)
        email = normalize_email(getattr(lead, "email", "") if lead else "")
        if not email:
            skipped.append({"lead_id": lead_id, "reason": "no_email"})
            continue
        if store.is_suppressed(email):
            skipped.append({"lead_id": lead_id, "reason": "suppressed"})
            continue
        eid = store.enroll(
            seq_id, lead_id, email,
            consent_source=req.consent_source or "manual_enroll",
            consent_at=now,
        )
        if eid is not None:
            enrolled += 1
        else:
            skipped.append({"lead_id": lead_id, "reason": "already_enrolled"})
    return {"enrolled": enrolled, "skipped": skipped, "total_lead_ids": len(req.lead_ids)}


@router.post("/sequences/{seq_id}/execute")
def execute_sequence(seq_id: str, ctx: WorkspaceCtx = Depends(require_admin)):
    """Enqueue ``send`` jobs for due steps (no inline send)."""
    from apps.api.services.outreach.sending import enqueue_due_sends

    store = get_outreach_store(ctx.workspace_id)
    if not store.sequence_exists(seq_id):
        raise HTTPException(status_code=404, detail="Sequence not found")
    n = enqueue_due_sends(store, ctx.workspace_id, seq_id, settings.OUTREACH_TICK_MAX_ENQUEUE)
    return {"enqueued": n}


@router.get("/sequences/{seq_id}/stats")
def sequence_stats(seq_id: str, ctx: WorkspaceCtx = Depends(current_workspace)):
    store = get_outreach_store(ctx.workspace_id)
    if not store.sequence_exists(seq_id):
        raise HTTPException(status_code=404, detail="Sequence not found")
    return store.get_sequence_stats(seq_id)


@router.get("/sequences/{seq_id}/sends")
def sequence_sends(seq_id: str, ctx: WorkspaceCtx = Depends(current_workspace)):
    store = get_outreach_store(ctx.workspace_id)
    return {"sends": store.list_sends(seq_id=seq_id)}


# ── SMTP Config ───────────────────────────────────────────────

@router.get("/smtp/status")
def smtp_status(ctx: WorkspaceCtx = Depends(current_workspace)):
    from apps.api.services.outreach.sender import get_smtp_config, is_smtp_configured

    cfg = get_smtp_config(ctx.workspace_id)
    return {
        "configured": is_smtp_configured(ctx.workspace_id),
        "host": cfg.host or None,
        "email": cfg.email or None,
        "from_name": cfg.from_name,
        "max_per_hour": cfg.max_per_hour,
        # NEVER return the password.
    }


@router.put("/smtp/config")
def update_smtp_config(req: SMTPConfigUpdate, ctx: WorkspaceCtx = Depends(require_admin)):
    from apps.api.services.workspace.secrets import set_secret

    ws = ctx.workspace_id
    if req.smtp_host is not None:
        set_secret(ws, "SMTP_HOST", req.smtp_host)
    if req.smtp_port is not None:
        set_secret(ws, "SMTP_PORT", str(req.smtp_port))
    if req.smtp_email is not None:
        set_secret(ws, "SMTP_EMAIL", req.smtp_email)
    if req.smtp_password is not None:
        set_secret(ws, "SMTP_PASSWORD", req.smtp_password)
    if req.smtp_from_name is not None:
        set_secret(ws, "SMTP_FROM_NAME", req.smtp_from_name)
    if req.smtp_max_per_hour is not None:
        set_secret(ws, "SMTP_MAX_PER_HOUR", str(req.smtp_max_per_hour))
    if req.smtp_use_tls is not None:
        set_secret(ws, "SMTP_USE_TLS", "1" if req.smtp_use_tls else "0")
    return {"status": "ok"}


@router.post("/smtp/test")
async def test_smtp(req: SendTestRequest, ctx: WorkspaceCtx = Depends(require_admin)):
    from apps.api.services.outreach.sender import get_smtp_config, send_email

    cfg = get_smtp_config(ctx.workspace_id)
    result = await send_email(
        to_email=req.to_email,
        subject="OpenGTM — SMTP Test",
        body_html="<p>Your SMTP configuration is working.</p>",
        config=cfg,
    )
    if result.success:
        return {"status": "ok", "message": f"Test email sent to {req.to_email}"}
    raise HTTPException(status_code=400, detail=result.error)


# ── Suppressions ──────────────────────────────────────────────

@router.get("/suppressions")
def list_suppressions(ctx: WorkspaceCtx = Depends(current_workspace)):
    store = get_outreach_store(ctx.workspace_id)
    return {"suppressions": store.list_suppressions()}


@router.post("/suppressions")
def add_suppression(req: SuppressionAdd, ctx: WorkspaceCtx = Depends(require_admin)):
    store = get_outreach_store(ctx.workspace_id)
    added = store.add_suppression(req.email, reason="manual", source="manual", locked=False)
    return {"status": "ok", "added": added}


@router.delete("/suppressions/{email}")
def remove_suppression(email: str, ctx: WorkspaceCtx = Depends(require_admin)):
    store = get_outreach_store(ctx.workspace_id)
    outcome = store.remove_suppression(email)
    if outcome == "not_found":
        raise HTTPException(status_code=404, detail="Suppression not found")
    if outcome == "locked":
        raise HTTPException(status_code=403, detail="Cannot remove an unsubscribe/complaint suppression")
    return {"status": "ok"}


# ── Public unsubscribe (token-auth, RFC 8058 one-click) ───────

# Tiny in-process rate limiter for the public endpoint (per IP + per token).
_unsub_hits: dict = {}
_UNSUB_WINDOW = 60
_UNSUB_MAX = 20


def _rate_limited(key: str) -> bool:
    now = time.time()
    bucket = [t for t in _unsub_hits.get(key, []) if t > now - _UNSUB_WINDOW]
    bucket.append(now)
    _unsub_hits[key] = bucket
    return len(bucket) > _UNSUB_MAX


@router.get("/unsubscribe", response_class=HTMLResponse)
def unsubscribe_landing(token: str = Query("")):
    from apps.api.services.outreach.tokens import verify_unsubscribe_token

    payload = verify_unsubscribe_token(token, allow_expired=True)
    if not payload:
        return HTMLResponse("<h2>Invalid unsubscribe link</h2>", status_code=400)
    email = payload.get("email", "")
    return HTMLResponse(
        f"<h2>Unsubscribe</h2><p>Click below to stop receiving emails at "
        f"{email}.</p>"
        f'<form method="post" action="/api/outreach/unsubscribe?token={token}">'
        f'<button type="submit">Unsubscribe</button></form>'
    )


@router.post("/unsubscribe")
def unsubscribe_confirm(request: Request, token: str = Query("")):
    """RFC 8058 one-click: no login, NO CSRF, side-effecting, idempotent."""
    from apps.api.services.outreach.tokens import verify_unsubscribe_token

    client_ip = request.client.host if request.client else "?"
    if _rate_limited(f"ip:{client_ip}") or _rate_limited(f"tok:{token[:32]}"):
        raise HTTPException(status_code=429, detail="rate limited")
    payload = verify_unsubscribe_token(token)  # rejects expired/forged
    if not payload:
        raise HTTPException(status_code=400, detail="invalid or expired token")
    ws_id = payload["ws"]
    email = payload["email"]
    seq_id = payload.get("seq", "")
    with workspace_scope(ws_id):
        store = get_outreach_store(ws_id)
        store.add_suppression(email, reason="unsubscribe", source=seq_id or "unsubscribe", locked=True)
    return {"status": "unsubscribed"}


# ── Bounce / complaint webhook (provider-auth, platform-global) ─

class BounceEvent(BaseModel):
    message_id: str
    event: str = "bounce"  # bounce | complaint
    hard: bool = True


@router.post("/webhooks/bounce")
def bounce_webhook(event: BounceEvent, request: Request):
    """Platform-authenticated bounce/complaint hook (spec §6.6).

    Verified by a platform-global shared secret BEFORE any workspace is trusted.
    The message_id→workspace lookup spans tenants and runs on a dedicated owner
    connection (no GUC); the write then enters workspace_scope.
    """
    secret = settings.OUTREACH_BOUNCE_WEBHOOK_SECRET
    if not secret:
        raise HTTPException(status_code=503, detail="bounce webhook not configured")
    provided = request.headers.get("X-Webhook-Secret", "")
    import hmac as _hmac

    if not _hmac.compare_digest(provided, secret):
        raise HTTPException(status_code=403, detail="invalid signature")

    # Cross-tenant lookup on a dedicated owner connection (no GUC, system read).
    from apps.api.database import SessionLocal
    from apps.api.services.outreach.orm_models import OutreachSend

    with SessionLocal() as db:
        send = (
            db.query(OutreachSend)
            .filter(OutreachSend.message_id == event.message_id)
            .order_by(OutreachSend.id.desc())
            .first()
        )
        if send is None:
            raise HTTPException(status_code=404, detail="message not found")
        ws_id = send.workspace_id
        seq_id = send.sequence_id
        enrollment_id = send.enrollment_id
        to_email = send.to_email
        send_id = send.id

    # Map the ESP event to the shared bounce kind.
    if event.event == "complaint":
        kind = "complaint"
    elif event.hard:
        kind = "hard"
    else:
        kind = "soft"

    with workspace_scope(ws_id):
        store = get_outreach_store(ws_id)
        # Idempotency gate (fixes the historical double-count on replay): the
        # ledger insert keyed on the event message_id runs apply_bounce once. A
        # replay conflicts on (workspace_id, source_message_id) → no-op.
        created = store.record_inbound_processed(
            imap_uid=event.message_id,
            uidvalidity="webhook",
            source_message_id=event.message_id,
            matched_send_id=send_id,
            kind=kind,
            recipient=to_email,
            diagnostic="webhook",
        )
        if not created:
            return {"status": "duplicate"}
        # Now also marks the send row bounced (the other historical defect) via
        # the shared applier used by the IMAP path.
        store.apply_bounce(
            send_id=send_id,
            sequence_id=seq_id,
            enrollment_id=enrollment_id,
            to_email=to_email,
            kind=kind,
            diagnostic="webhook",
            source="webhook",
        )
    return {"status": "ok"}
