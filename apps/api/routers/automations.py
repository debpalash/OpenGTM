"""Automations / Trigger Engine API router (§4 of the spec).

prefix ``/api/automations``. Every endpoint depends on ``current_workspace``
(reads) or ``require_workspace_role("admin")`` (mutations) so the RLS GUC is set
and membership/role enforced. When ``AUTOMATIONS_ENABLED`` is False the router
returns 404 for every path.

v1 LOCKED SCOPE validation:
  * trigger_type / action_type enums,
  * on_signal rejected (409) when PG_LEAD_STORE is false,
  * sequencer / send_email rejected (409) — deferred WI (legacy_outreach_disabled),
  * re_enrich column_ids validated against the scoped workbook columns,
  * webhook URL scheme/creds checked at create (full DNS-pin SSRF at execution),
  * condition parse-checked by the hardened evaluator against an adversarial row.
"""

import logging
from datetime import datetime, timezone
from typing import Any, Optional

from fastapi import APIRouter, Depends, HTTPException, Query
from pydantic import BaseModel, Field
from sqlalchemy.orm import Session

from apps.api.core.config import settings
from apps.api.core.tenancy import WorkspaceCtx, current_workspace, require_workspace_role
from apps.api.database import get_db
from apps.api.services.automations.models import (
    Trigger, TriggerRun, TriggerActionResult,
    TRIGGER_TYPES, ACTION_TYPES_V1, ACTION_TYPES_LEGACY, SCHEDULE_INTERVALS,
)
from apps.api.services.automations.safe_conditions import validate_condition, ConditionError

logger = logging.getLogger("automations.api")
router = APIRouter(prefix="/api/automations", tags=["automations"])

# Single shared admin dependency so mutations are uniformly role-gated AND so
# tests can override it via app.dependency_overrides[require_admin].
require_admin = require_workspace_role("admin")


def _require_enabled():
    if not getattr(settings, "AUTOMATIONS_ENABLED", False):
        raise HTTPException(status_code=404, detail="automations disabled")


# ── request models ──────────────────────────────────────────────────────────

class TriggerCreate(BaseModel):
    name: str = Field(..., min_length=1, max_length=200)
    trigger_type: str
    trigger_config: dict = Field(default_factory=dict)
    condition: str = ""
    actions: list[dict] = Field(default_factory=list)
    scope_workbook_ids: list[str] = Field(default_factory=list)
    stop_on_error: bool = False
    max_spend_usd_per_day: Optional[float] = None
    max_actions_per_day: Optional[int] = None
    enabled: bool = True


class TriggerPatch(BaseModel):
    name: Optional[str] = None
    trigger_config: Optional[dict] = None
    condition: Optional[str] = None
    actions: Optional[list[dict]] = None
    scope_workbook_ids: Optional[list[str]] = None
    stop_on_error: Optional[bool] = None
    max_spend_usd_per_day: Optional[float] = None
    max_actions_per_day: Optional[int] = None
    enabled: Optional[bool] = None


class PreviewRequest(BaseModel):
    row_ids: Optional[list[str]] = None
    limit: int = 25


class RunRequest(BaseModel):
    row_ids: Optional[list[str]] = None
    dry_run: bool = False


# ── validation helpers ──────────────────────────────────────────────────────

def _scoped_columns(db: Session, ws_id: str, scope_workbook_ids: list) -> set:
    """All column ids across the rule's scoped workbooks (or all ws workbooks)."""
    from apps.api.services.workbook.models import Workbook

    q = db.query(Workbook).filter(Workbook.workspace_id == ws_id)
    if scope_workbook_ids:
        q = q.filter(Workbook.id.in_(scope_workbook_ids))
    col_ids: set = set()
    for wb in q.all():
        for c in (wb.columns_config or []):
            if c.get("id"):
                col_ids.add(c["id"])
    return col_ids


def _validate_rule(db: Session, ws_id: str, *, trigger_type: str, trigger_config: dict,
                   actions: list, condition: str, scope_workbook_ids: list):
    if trigger_type not in TRIGGER_TYPES:
        raise HTTPException(status_code=422, detail=f"invalid trigger_type '{trigger_type}'")

    # on_signal works on BOTH backends now: scanner + poller write through the
    # shared ORM signal store, so emit_signal_matches fires on SQLite too. The
    # signals table is guaranteed present on both backends. Gated only by
    # AUTOMATIONS_ENABLED (emit is a no-op when off). No PG_LEAD_STORE gate.

    # on_schedule interval validation
    if trigger_type == "on_schedule":
        interval = (trigger_config or {}).get("interval")
        if interval not in SCHEDULE_INTERVALS:
            raise HTTPException(status_code=422, detail=f"invalid schedule interval '{interval}'")

    max_actions = int(getattr(settings, "AUTOMATIONS_MAX_ACTIONS_PER_RULE", 10))
    if len(actions or []) > max_actions:
        raise HTTPException(status_code=422, detail=f"too many actions (max {max_actions})")

    scoped_cols = None
    for i, action in enumerate(actions or []):
        atype = action.get("type")
        cfg = action.get("config") or {}
        if atype in ACTION_TYPES_LEGACY:
            # Outreach actions are ACCEPTED types now, but the flag check lives
            # HERE in their validation branch (spec §11) — they are NOT moved into
            # ACTION_TYPES_V1 (which would skip this gate). The flag is consulted
            # on EVERY create, so AC10 holds.
            if not getattr(settings, "AUTOMATIONS_ALLOW_LEGACY_OUTREACH", False):
                raise HTTPException(status_code=409, detail="legacy_outreach_disabled")
            if atype == "sequencer":
                seq_id = (cfg.get("sequence_id") or "").strip()
                if not seq_id:
                    raise HTTPException(status_code=422, detail=f"sequencer action {i}: sequence_id required")
                from apps.api.services.outreach.store import get_outreach_store
                if not get_outreach_store(ws_id).sequence_exists(seq_id):
                    raise HTTPException(status_code=404, detail=f"sequencer action {i}: sequence not found")
            elif atype == "send_email":
                from apps.api.services.outreach.sender import is_smtp_configured
                from apps.api.services.workspace.secrets import get_secret
                if not is_smtp_configured(ws_id):
                    raise HTTPException(status_code=400, detail=f"send_email action {i}: SMTP not configured")
                if not (get_secret(ws_id, "OUTREACH_FOOTER", "") or "").strip():
                    raise HTTPException(status_code=400, detail=f"send_email action {i}: OUTREACH_FOOTER required")
                seq_id = (cfg.get("sequence_id") or "").strip()
                if seq_id:
                    from apps.api.services.outreach.store import get_outreach_store
                    if not get_outreach_store(ws_id).sequence_exists(seq_id):
                        raise HTTPException(status_code=404, detail=f"send_email action {i}: sequence not found")
            continue  # validated; skip the V1-type checks below
        if atype not in ACTION_TYPES_V1:
            raise HTTPException(status_code=422, detail=f"invalid action type '{atype}' at index {i}")
        if atype == "re_enrich":
            if scoped_cols is None:
                scoped_cols = _scoped_columns(db, ws_id, scope_workbook_ids)
            col_ids = cfg.get("column_ids") or []
            if not col_ids:
                raise HTTPException(status_code=422, detail=f"re_enrich action {i}: column_ids required")
            missing = [c for c in col_ids if c not in scoped_cols]
            if missing:
                raise HTTPException(
                    status_code=422,
                    detail=f"re_enrich action {i}: column_ids not in scoped workbook(s): {missing}",
                )
        if atype == "webhook":
            url = (cfg.get("url") or "").strip()
            if not url:
                raise HTTPException(status_code=422, detail=f"webhook action {i}: url required")
            from urllib.parse import urlparse
            p = urlparse(url)
            # Allow {field} templated URLs; only hard-reject obvious bad static URLs.
            if "{" not in url:
                if p.scheme not in ("http", "https"):
                    raise HTTPException(status_code=422, detail=f"webhook action {i}: only http/https allowed")
                if p.username or p.password:
                    raise HTTPException(status_code=422, detail=f"webhook action {i}: credentials in url not allowed")
            ref = cfg.get("header_secret_ref")
            if ref:
                from apps.api.services.workspace.secrets import get_secret
                if not get_secret(ws_id, ref, ""):
                    raise HTTPException(status_code=422, detail=f"webhook action {i}: header_secret_ref '{ref}' not found")

    # condition: hardened parse against an adversarial populated row
    try:
        validate_condition(condition or "")
    except ConditionError as e:
        raise HTTPException(status_code=422, detail=f"invalid condition: {e}")


# ── CRUD ────────────────────────────────────────────────────────────────────

@router.post("/triggers")
def create_trigger(
    body: TriggerCreate,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(require_admin),
):
    _require_enabled()
    ws_id = ctx.workspace_id

    max_rules = int(getattr(settings, "AUTOMATIONS_MAX_RULES_PER_WS", 50))
    count = db.query(Trigger).filter(Trigger.workspace_id == ws_id).count()
    if count >= max_rules:
        raise HTTPException(status_code=422, detail=f"max rules per workspace reached ({max_rules})")

    _validate_rule(
        db, ws_id, trigger_type=body.trigger_type, trigger_config=body.trigger_config,
        actions=body.actions, condition=body.condition, scope_workbook_ids=body.scope_workbook_ids,
    )

    trig = Trigger(
        workspace_id=ws_id,
        name=body.name,
        enabled=body.enabled,
        trigger_type=body.trigger_type,
        trigger_config=body.trigger_config or {},
        condition=body.condition or "",
        actions=body.actions or [],
        scope_workbook_ids=body.scope_workbook_ids or [],
        stop_on_error=body.stop_on_error,
        max_spend_usd_per_day=body.max_spend_usd_per_day,
        max_actions_per_day=body.max_actions_per_day,
        created_by=getattr(ctx.user, "id", None),
    )
    if body.trigger_type == "on_schedule":
        trig.schedule_anchor = datetime.now(timezone.utc)
    db.add(trig)
    db.commit()
    db.refresh(trig)

    # bootstrap schedule + mirror
    if body.trigger_type == "on_schedule" and body.enabled:
        from apps.api.services.automations.engine import schedule_bootstrap_for_trigger
        try:
            schedule_bootstrap_for_trigger(db, trig)
        except Exception as e:
            logger.warning("schedule bootstrap on create failed: %s", e)

    return trig.to_api()


@router.get("/triggers")
def list_triggers(
    enabled: Optional[bool] = Query(default=None),
    trigger_type: Optional[str] = Query(default=None),
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    _require_enabled()
    q = db.query(Trigger).filter(Trigger.workspace_id == ctx.workspace_id)
    if enabled is not None:
        q = q.filter(Trigger.enabled.is_(enabled))
    if trigger_type:
        q = q.filter(Trigger.trigger_type == trigger_type)
    return [t.to_api() for t in q.order_by(Trigger.created_at.desc()).all()]


def _get_trigger_or_404(db: Session, ws_id: str, trigger_id: str) -> Trigger:
    trig = (
        db.query(Trigger)
        .filter(Trigger.id == trigger_id, Trigger.workspace_id == ws_id)
        .first()
    )
    if trig is None:
        raise HTTPException(status_code=404, detail="trigger not found")
    return trig


@router.get("/triggers/{trigger_id}")
def get_trigger(
    trigger_id: str,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    _require_enabled()
    trig = _get_trigger_or_404(db, ctx.workspace_id, trigger_id)
    runs = (
        db.query(TriggerRun)
        .filter(TriggerRun.workspace_id == ctx.workspace_id, TriggerRun.trigger_id == trigger_id)
        .order_by(TriggerRun.started_at.desc())
        .limit(10)
        .all()
    )
    out = trig.to_api()
    out["recent_runs"] = [r.to_api() for r in runs]
    return out


@router.patch("/triggers/{trigger_id}")
def update_trigger(
    trigger_id: str,
    body: TriggerPatch,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(require_admin),
):
    _require_enabled()
    ws_id = ctx.workspace_id
    trig = _get_trigger_or_404(db, ws_id, trigger_id)

    new_actions = body.actions if body.actions is not None else (trig.actions or [])
    new_condition = body.condition if body.condition is not None else (trig.condition or "")
    new_scope = body.scope_workbook_ids if body.scope_workbook_ids is not None else (trig.scope_workbook_ids or [])
    new_config = body.trigger_config if body.trigger_config is not None else (trig.trigger_config or {})
    _validate_rule(
        db, ws_id, trigger_type=trig.trigger_type, trigger_config=new_config,
        actions=new_actions, condition=new_condition, scope_workbook_ids=new_scope,
    )

    for fld in ("name", "stop_on_error", "max_spend_usd_per_day", "max_actions_per_day", "enabled"):
        val = getattr(body, fld)
        if val is not None:
            setattr(trig, fld, val)
    if body.trigger_config is not None:
        trig.trigger_config = body.trigger_config
    if body.condition is not None:
        trig.condition = body.condition
    if body.actions is not None:
        trig.actions = body.actions
    if body.scope_workbook_ids is not None:
        trig.scope_workbook_ids = body.scope_workbook_ids
    db.commit()
    db.refresh(trig)

    # keep schedule mirror in sync
    if trig.trigger_type == "on_schedule":
        from apps.api.services.automations.engine import mirror_set, schedule_bootstrap_for_trigger
        if trig.enabled:
            schedule_bootstrap_for_trigger(db, trig)
        else:
            mirror_set(db, trig.id, ws_id, trig.next_run_at, False)
            db.commit()
    return trig.to_api()


@router.delete("/triggers/{trigger_id}")
def delete_trigger(
    trigger_id: str,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(require_admin),
):
    _require_enabled()
    trig = _get_trigger_or_404(db, ctx.workspace_id, trigger_id)
    from apps.api.services.automations.engine import mirror_delete
    mirror_delete(db, trig.id)
    db.delete(trig)
    db.commit()
    return {"deleted": True}


@router.post("/triggers/{trigger_id}/pause")
def pause_trigger(
    trigger_id: str,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(require_admin),
):
    _require_enabled()
    trig = _get_trigger_or_404(db, ctx.workspace_id, trigger_id)
    trig.enabled = False
    from apps.api.services.automations.engine import mirror_set
    mirror_set(db, trig.id, ctx.workspace_id, trig.next_run_at, False)
    db.commit()
    return {"enabled": False}


@router.post("/triggers/{trigger_id}/resume")
def resume_trigger(
    trigger_id: str,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(require_admin),
):
    _require_enabled()
    trig = _get_trigger_or_404(db, ctx.workspace_id, trigger_id)
    trig.enabled = True
    db.commit()
    next_run = None
    if trig.trigger_type == "on_schedule":
        from apps.api.services.automations.engine import schedule_bootstrap_for_trigger
        next_run = schedule_bootstrap_for_trigger(db, trig)
    return {"enabled": True, "next_run_at": next_run.isoformat() if next_run else None}


# ── preview (dry-run, NO writes/sends/debits) ───────────────────────────────

@router.post("/triggers/{trigger_id}/preview")
async def preview_trigger(
    trigger_id: str,
    body: PreviewRequest,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    _require_enabled()
    trig = _get_trigger_or_404(db, ctx.workspace_id, trigger_id)

    from apps.api.services.automations.engine import resolve_candidate_rows, _row_cells, _lead_data_for_action
    from apps.api.services.automations.safe_conditions import safe_evaluate_condition
    from apps.api.services.automations import actions as actmod

    payload = {"targets": [{"workbook_id": None, "row_id": r} for r in body.row_ids] if body.row_ids else None}
    # for preview without explicit targets, sweep the scoped rows
    if body.row_ids:
        # resolve workbook for each row id
        from apps.api.services.workbook.models import WorkbookRow
        targets = []
        for rid in body.row_ids:
            try:
                row = db.query(WorkbookRow).filter(WorkbookRow.id == int(rid)).first()
            except (ValueError, TypeError):
                row = db.query(WorkbookRow).filter(WorkbookRow.id == rid).first()
            if row:
                targets.append({"workbook_id": row.workbook_id, "row_id": str(row.id)})
        payload = {"targets": targets}
    else:
        payload = {}

    candidates = resolve_candidate_rows(db, trig, payload)
    matched = 0
    sample = []
    projected_total = 0.0
    limit = max(1, min(body.limit or 25, 100))
    for wb_id, row_id, row, cols in candidates:
        cells = _row_cells(row)
        verdict = safe_evaluate_condition(trig.condition or "", cells, cols)
        if not verdict.passed:
            continue
        matched += 1
        lead_data = _lead_data_for_action(row, cols)
        action_previews = []
        for action in (trig.actions or []):
            cost = actmod.project_action_cost(action, cols)
            projected_total += cost
            preview_payload = _preview_payload(action, lead_data, cols)
            action_previews.append({
                "type": action.get("type"),
                "would_charge_usd": round(cost, 4),
                "resolved_payload_preview": preview_payload,
            })
        if len(sample) < limit:
            sample.append({
                "row_id": row_id,
                "workbook_id": wb_id,
                "condition_pass": True,
                "actions": action_previews,
            })
    return {
        "matched": matched,
        "sample": sample,
        "projected_total_usd": round(projected_total, 4),
    }


def _preview_payload(action: dict, lead_data: dict, cols: list) -> Any:
    atype = action.get("type")
    cfg = action.get("config") or {}
    try:
        from apps.api.services.workbook.output import _resolve, _resolve_deep
        if atype == "webhook":
            url = _resolve((cfg.get("url") or ""), lead_data, cols)
            body = cfg.get("body")
            body_prev = _resolve_deep(body, lead_data, cols) if isinstance(body, (dict, list)) else (
                _resolve(str(body), lead_data, cols) if body else None
            )
            return {"url": url, "method": (cfg.get("method") or "POST").upper(), "body": body_prev}
        if atype == "push_crm":
            return {"crm": cfg.get("type", "hubspot"), "field_map": cfg.get("field_map")}
        if atype == "re_enrich":
            return {"column_ids": cfg.get("column_ids", [])}
    except Exception as e:
        return {"error": f"preview failed: {e}"}
    return {}


# ── manual run (enqueues a trigger_eval) ────────────────────────────────────

@router.post("/triggers/{trigger_id}/run")
def run_trigger(
    trigger_id: str,
    body: RunRequest,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(require_admin),
):
    _require_enabled()
    trig = _get_trigger_or_404(db, ctx.workspace_id, trigger_id)

    targets = None
    if body.row_ids:
        from apps.api.services.workbook.models import WorkbookRow
        targets = []
        for rid in body.row_ids:
            try:
                row = db.query(WorkbookRow).filter(WorkbookRow.id == int(rid)).first()
            except (ValueError, TypeError):
                row = db.query(WorkbookRow).filter(WorkbookRow.id == rid).first()
            if row:
                targets.append({"workbook_id": row.workbook_id, "row_id": str(row.id)})

    from apps.api.services.queue_service import queue_service
    fire_key = f"manual:{trigger_id}:{datetime.now(timezone.utc).isoformat()}"
    payload = {
        "trigger_id": trigger_id,
        "workspace_id": ctx.workspace_id,
        "fire_source": "manual",
        "fire_key": fire_key,
        "dry_run": bool(body.dry_run),
    }
    if targets is not None:
        payload["targets"] = targets
    job = queue_service.add_job(db, "trigger_eval", payload)
    return {"job_id": job.id, "fire_key": fire_key, "dry_run": bool(body.dry_run)}


# ── run history ─────────────────────────────────────────────────────────────

@router.get("/triggers/{trigger_id}/runs")
def list_runs(
    trigger_id: str,
    limit: int = Query(default=50, le=200),
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    _require_enabled()
    _get_trigger_or_404(db, ctx.workspace_id, trigger_id)
    runs = (
        db.query(TriggerRun)
        .filter(TriggerRun.workspace_id == ctx.workspace_id, TriggerRun.trigger_id == trigger_id)
        .order_by(TriggerRun.started_at.desc())
        .limit(limit)
        .all()
    )
    return [r.to_api() for r in runs]


@router.get("/runs/{run_id}")
def get_run(
    run_id: str,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(current_workspace),
):
    _require_enabled()
    run = (
        db.query(TriggerRun)
        .filter(TriggerRun.id == run_id, TriggerRun.workspace_id == ctx.workspace_id)
        .first()
    )
    if run is None:
        raise HTTPException(status_code=404, detail="run not found")
    results = (
        db.query(TriggerActionResult)
        .filter(TriggerActionResult.workspace_id == ctx.workspace_id, TriggerActionResult.run_id == run_id)
        .order_by(TriggerActionResult.id.asc())
        .all()
    )
    out = run.to_api()
    out["action_results"] = [r.to_api() for r in results]
    return out
