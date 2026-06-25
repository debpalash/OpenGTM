"""Intent-Signal Poller API router (spec §4).

prefix ``/api/watches``. Every endpoint depends on ``current_workspace`` (reads)
or ``require_workspace_role("editor","admin")`` (mutations) so the RLS GUC is set
and membership/role enforced. Belt-and-suspenders ``workspace_id == ctx.workspace_id``
on top of RLS. When ``INTENT_POLLER_ENABLED`` is False the router 404s every path.

Delivery is rules-only: the create endpoint can OPTIONALLY auto-create an
``on_signal``→``webhook`` rule (validated by the automations service), but the
watch itself never stores a webhook and the poller never sends one.
"""

from __future__ import annotations

import logging
import uuid
from datetime import datetime, timezone
from typing import Optional

from fastapi import APIRouter, Depends, HTTPException, Query
from pydantic import BaseModel, Field
from sqlalchemy.orm import Session

from apps.api.core.config import settings
from apps.api.core.tenancy import WorkspaceCtx, current_workspace, require_workspace_role
from apps.api.database import get_db
from apps.api.services.poller import engine as poller_engine
from apps.api.services.poller.models import (
    WATCH_INTERVALS,
    WATCH_KINDS,
    PollBudgetLedger,
    WatchSubscription,
)

logger = logging.getLogger("poller.api")
router = APIRouter(prefix="/api/watches", tags=["watches"])

require_editor = require_workspace_role("editor", "admin")

# signal_types each kind may emit (for validation against on_signal config).
_KIND_SIGNAL_TYPES = {
    "funding": {"company_funded", "executive_hired"},
    "hiring": {"hiring_surge", "new_tech_adopted"},
    "feed": {"news"},
    "company": {"company_funded", "executive_hired", "hiring_surge", "new_tech_adopted"},
}


def _require_enabled():
    if not getattr(settings, "INTENT_POLLER_ENABLED", False):
        raise HTTPException(status_code=404, detail="intent poller disabled")
    if not getattr(settings, "PG_LEAD_STORE", False):
        raise HTTPException(status_code=409, detail="intent_poller_requires_pg_lead_store")


# ── request models ──────────────────────────────────────────────────────────

class WatchCreate(BaseModel):
    kind: str
    target: str = Field(..., min_length=1)
    lead_id: Optional[int] = None
    signal_types: Optional[list[str]] = None
    interval: Optional[str] = None
    # Optional convenience: auto-create an on_signal→webhook rule (rules-only
    # delivery; the watch stores no webhook).
    create_webhook_rule: bool = False
    webhook_url: Optional[str] = None
    webhook_secret_ref: Optional[str] = None


class WatchPatch(BaseModel):
    enabled: Optional[bool] = None
    interval: Optional[str] = None
    signal_types: Optional[list[str]] = None
    lead_id: Optional[int] = None


# ── serialization ─────────────────────────────────────────────────────────────

def _to_api(w: WatchSubscription) -> dict:
    return {
        "id": w.id,
        "workspace_id": w.workspace_id,
        "kind": w.kind,
        "target": w.target,
        "resolved_cik": w.resolved_cik,
        "lead_id": w.lead_id,
        "signal_types": w.signal_types or [],
        "interval": w.interval,
        "enabled": w.enabled,
        "next_poll_at": w.next_poll_at.isoformat() if w.next_poll_at else None,
        "last_polled_at": w.last_polled_at.isoformat() if w.last_polled_at else None,
        "last_error": w.last_error,
        "consecutive_failures": w.consecutive_failures or 0,
        "cursor": w.cursor or {},
        "created_at": w.created_at.isoformat() if w.created_at else None,
    }


def _load(db: Session, ws_id: str, watch_id: str) -> WatchSubscription:
    w = (
        db.query(WatchSubscription)
        .filter(WatchSubscription.id == watch_id, WatchSubscription.workspace_id == ws_id)
        .first()
    )
    if w is None:
        raise HTTPException(status_code=404, detail="watch not found")
    return w


def _maybe_create_webhook_rule(ws_id: str, body: WatchCreate, signal_types: list[str]):
    """Auto-create an on_signal→webhook rule via the automations service (which
    validates the URL via _validate_and_pin at create). The watch stores nothing."""
    if not body.create_webhook_rule:
        return None
    if not getattr(settings, "AUTOMATIONS_ENABLED", False):
        raise HTTPException(status_code=409, detail="automations_disabled_cannot_create_webhook_rule")
    url = (body.webhook_url or "").strip()
    if not url:
        raise HTTPException(status_code=422, detail="webhook_url required when create_webhook_rule is set")
    from urllib.parse import urlparse

    from apps.api.database import SessionLocal
    from apps.api.services.automations.models import Trigger

    p = urlparse(url)
    if p.scheme not in ("http", "https"):
        raise HTTPException(status_code=422, detail="webhook_url: only http/https allowed")
    if p.username or p.password:
        raise HTTPException(status_code=422, detail="webhook_url: credentials not allowed")
    cfg = {"url": url, "method": "POST"}
    if body.webhook_secret_ref:
        from apps.api.services.workspace.secrets import get_secret
        if not get_secret(ws_id, body.webhook_secret_ref, ""):
            raise HTTPException(status_code=422, detail="webhook_secret_ref not found")
        cfg["header_secret_ref"] = body.webhook_secret_ref
    trig = Trigger(
        workspace_id=ws_id,
        name=f"watch webhook ({body.kind}:{body.target})"[:200],
        enabled=True,
        trigger_type="on_signal",
        trigger_config={"signal_types": signal_types},
        condition="",
        actions=[{"type": "webhook", "config": cfg}],
        scope_workbook_ids=[],
    )
    with SessionLocal() as db:
        db.add(trig)
        db.commit()
        db.refresh(trig)
        return trig.id


# ── CRUD ──────────────────────────────────────────────────────────────────────

@router.post("")
def create_watch(
    body: WatchCreate,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(require_editor),
):
    _require_enabled()
    ws_id = ctx.workspace_id

    if body.kind not in WATCH_KINDS:
        raise HTTPException(status_code=422, detail=f"invalid kind '{body.kind}'")
    interval = body.interval or settings.INTENT_POLLER_DEFAULT_INTERVAL
    if interval not in WATCH_INTERVALS:
        raise HTTPException(status_code=422, detail=f"invalid interval '{interval}'")

    allowed = _KIND_SIGNAL_TYPES[body.kind]
    signal_types = body.signal_types or sorted(allowed)
    bad = [s for s in signal_types if s not in allowed]
    if bad:
        raise HTTPException(status_code=422, detail=f"signal_types {bad} not valid for kind '{body.kind}'")

    max_watches = int(settings.INTENT_POLLER_MAX_WATCHES_PER_WS)
    count = db.query(WatchSubscription).filter(WatchSubscription.workspace_id == ws_id).count()
    if count >= max_watches:
        raise HTTPException(status_code=422, detail=f"max watches per workspace reached ({max_watches})")

    # Optional webhook rule (separate session/txn; validates URL at create).
    self_rule_id = _maybe_create_webhook_rule(ws_id, body, signal_types)

    w = WatchSubscription(
        id=str(uuid.uuid4()),
        workspace_id=ws_id,
        kind=body.kind,
        target=body.target.strip(),
        lead_id=body.lead_id,
        signal_types=signal_types,
        interval=interval,
        schedule_anchor=datetime.now(timezone.utc),
        enabled=True,
        cursor={"bootstrapped": False},
        consecutive_failures=0,
    )
    db.add(w)
    db.flush()
    poller_engine.schedule_bootstrap_for_watch(db, w)
    db.commit()
    db.refresh(w)
    out = _to_api(w)
    out["webhook_rule_id"] = self_rule_id
    return out


@router.get("")
def list_watches(
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
    limit: int = Query(100, ge=1, le=500),
    offset: int = Query(0, ge=0),
):
    _require_enabled()
    rows = (
        db.query(WatchSubscription)
        .filter(WatchSubscription.workspace_id == ctx.workspace_id)
        .order_by(WatchSubscription.created_at.desc())
        .limit(limit)
        .offset(offset)
        .all()
    )
    return {"watches": [_to_api(w) for w in rows], "limit": limit, "offset": offset}


@router.get("/{watch_id}")
def get_watch(
    watch_id: str,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    _require_enabled()
    return _to_api(_load(db, ctx.workspace_id, watch_id))


@router.patch("/{watch_id}")
def patch_watch(
    watch_id: str,
    body: WatchPatch,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(require_editor),
):
    _require_enabled()
    ws_id = ctx.workspace_id
    w = _load(db, ws_id, watch_id)
    if body.interval is not None:
        if body.interval not in WATCH_INTERVALS:
            raise HTTPException(status_code=422, detail=f"invalid interval '{body.interval}'")
        w.interval = body.interval
    if body.signal_types is not None:
        allowed = _KIND_SIGNAL_TYPES[w.kind]
        bad = [s for s in body.signal_types if s not in allowed]
        if bad:
            raise HTTPException(status_code=422, detail=f"signal_types {bad} not valid for kind '{w.kind}'")
        w.signal_types = body.signal_types
    if body.lead_id is not None:
        w.lead_id = body.lead_id
    if body.enabled is not None:
        was_enabled = w.enabled
        w.enabled = body.enabled
        if body.enabled and not was_enabled:
            w.consecutive_failures = 0
            w.last_error = None
            poller_engine.schedule_bootstrap_for_watch(db, w)
        elif not body.enabled:
            w.next_poll_at = None
            poller_engine.mirror_set(db, w.id, ws_id, None, False)
    db.commit()
    db.refresh(w)
    return _to_api(w)


@router.delete("/{watch_id}")
def delete_watch(
    watch_id: str,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(require_editor),
):
    _require_enabled()
    w = _load(db, ctx.workspace_id, watch_id)
    poller_engine.mirror_delete(db, w.id)
    db.delete(w)
    db.commit()
    return {"deleted": True, "id": watch_id}


# ── poll-now (own quota + per-watch rate limit; reuses scheduled fire_key) ────

@router.post("/{watch_id}/poll")
def poll_now(
    watch_id: str,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(require_editor),
):
    _require_enabled()
    ws_id = ctx.workspace_id
    w = _load(db, ws_id, watch_id)
    if not w.enabled:
        raise HTTPException(status_code=409, detail="watch disabled")

    # per-watch rate limit
    min_iv = int(settings.INTENT_POLLER_POLL_NOW_MIN_INTERVAL_SEC)
    if min_iv > 0 and w.last_polled_at is not None:
        last = w.last_polled_at
        if last.tzinfo is None:
            last = last.replace(tzinfo=timezone.utc)
        if (datetime.now(timezone.utc) - last).total_seconds() < min_iv:
            raise HTTPException(status_code=429, detail="poll-now rate limited")

    # own per-(ws, day) quota
    quota = int(settings.INTENT_POLLER_POLL_NOW_DAILY_QUOTA)
    if quota > 0 and poller_engine.poll_now_quota_used(db, ws_id) >= quota:
        raise HTTPException(status_code=429, detail="poll-now daily quota exhausted")

    # Single-flight: if a scheduled poll is already due/in-flight, reuse it
    # (return 202 "already queued"). Match any active watch_poll job for THIS
    # watch whose fire_key is the scheduled key (``watch:<id>:`` but not a
    # ``:pollnow:`` manual key) — robust against datetime round-trip lossiness.
    from apps.api.models import Job

    active = (
        db.query(Job)
        .filter(
            Job.type == "watch_poll",
            Job.fire_key.like(f"watch:{w.id}:%"),
            Job.status.in_(("pending", "processing")),
        )
        .all()
    )
    scheduled = [j for j in active if ":pollnow:" not in (j.fire_key or "")]
    if scheduled:
        return {"status": "already_queued", "job_id": scheduled[0].id,
                "fire_key": scheduled[0].fire_key}

    # Forced poll-now: manual fire_key, still single-flighted via advisory lock +
    # unique index, counts poll-now quota.
    manual_key = f"watch:{w.id}:pollnow:{datetime.now(timezone.utc).date().isoformat()}"
    enqueued = poller_engine._enqueue_poll_if_absent(
        db, fire_key=manual_key, workspace_id=ws_id, watch_id=w.id,
        next_run_at=datetime.now(timezone.utc),
    )
    # tag the job as a manual poll-now so the handler counts the right quota
    if enqueued:
        job = (
            db.query(Job)
            .filter(Job.type == "watch_poll", Job.fire_key == manual_key,
                    Job.status == "pending")
            .first()
        )
        if job is not None:
            payload = dict(job.payload or {})
            payload["poll_now"] = True
            job.payload = payload
    db.commit()
    if not enqueued:
        return {"status": "already_queued", "fire_key": manual_key}
    return {"status": "queued", "fire_key": manual_key}


# ── signal feed (proxy to PgLeadStore.get_signals, watch-filtered) ────────────

@router.get("/{watch_id}/signals")
def watch_signals(
    watch_id: str,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
    signal_type: Optional[str] = None,
    limit: int = Query(50, ge=1, le=200),
    offset: int = Query(0, ge=0),
):
    _require_enabled()
    w = _load(db, ctx.workspace_id, watch_id)
    store = ctx.lead_db()
    out = []
    types = [signal_type] if signal_type else (w.signal_types or [None])
    for st in types:
        out.extend(store.get_signals(
            signal_type=st, lead_id=w.lead_id, limit=limit, offset=offset,
        ))
    # de-dup by id, sort by created_at desc
    seen = set()
    uniq = []
    for s in sorted(out, key=lambda r: r.get("created_at", 0), reverse=True):
        if s["id"] in seen:
            continue
        seen.add(s["id"])
        uniq.append(s)
    return {"signals": uniq[:limit]}
