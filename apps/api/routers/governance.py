import csv
import io
from datetime import datetime
from typing import Optional

from fastapi import APIRouter, Depends, HTTPException, Query
from fastapi.responses import StreamingResponse
from sqlalchemy.orm import Session
from pydantic import BaseModel, Field

from apps.api.core.tenancy import WorkspaceCtx, require_workspace_role
from apps.api.database import get_db
from apps.api.services.governance.models import GovernanceAuditEvent, RetentionPolicy, RetentionRun
from apps.api.services.governance.retention import DEFAULT_DAYS, normalized_days, preview_retention, schedule_policy

router = APIRouter(prefix="/api/governance", tags=["governance"])
require_admin = require_workspace_role("admin")


class RetentionUpdate(BaseModel):
    enabled: bool
    legal_hold: bool = False
    retention_days: dict[str, int] = Field(default_factory=dict)


class RetentionEnforce(BaseModel):
    confirmation: str


def _audit_query(db, ctx, actor_user_id=None, method=None, outcome=None, since=None, before=None):
    query = db.query(GovernanceAuditEvent).filter(GovernanceAuditEvent.workspace_id == ctx.workspace_id)
    if actor_user_id is not None: query = query.filter(GovernanceAuditEvent.actor_user_id == actor_user_id)
    if method: query = query.filter(GovernanceAuditEvent.method == method.upper())
    if outcome: query = query.filter(GovernanceAuditEvent.outcome == outcome)
    if since: query = query.filter(GovernanceAuditEvent.created_at >= since)
    if before: query = query.filter(GovernanceAuditEvent.created_at < before)
    return query


@router.get("/audit-events")
def list_audit_events(
    actor_user_id: Optional[int] = None,
    method: Optional[str] = None,
    outcome: Optional[str] = None,
    since: Optional[datetime] = None,
    before: Optional[datetime] = None,
    limit: int = Query(default=100, ge=1, le=500),
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(require_admin),
):
    rows = _audit_query(db, ctx, actor_user_id, method, outcome, since, before).order_by(GovernanceAuditEvent.created_at.desc(), GovernanceAuditEvent.id.desc()).limit(limit + 1).all()
    has_more = len(rows) > limit
    page = rows[:limit]
    return {"events": [row.to_api() for row in page], "next_before": page[-1].created_at if has_more else None}


@router.get("/audit-events/export.csv")
def export_audit_events(
    since: Optional[datetime] = None,
    db: Session = Depends(get_db),
    ctx: WorkspaceCtx = Depends(require_admin),
):
    rows = _audit_query(db, ctx, since=since).order_by(GovernanceAuditEvent.created_at.desc()).limit(10_000).all()
    stream = io.StringIO()
    writer = csv.writer(stream)
    writer.writerow(["id", "created_at", "actor_user_id", "actor_role", "method", "route", "resource_path", "response_status", "outcome", "request_id"])
    for row in rows:
        writer.writerow([row.id, row.created_at.isoformat(), row.actor_user_id, row.actor_role, row.method, row.route, row.resource_path, row.response_status, row.outcome, row.request_id])
    return StreamingResponse(iter([stream.getvalue()]), media_type="text/csv", headers={"Content-Disposition": "attachment; filename=opengtm-audit-events.csv"})


@router.get("/retention")
def get_retention(db: Session = Depends(get_db), ctx: WorkspaceCtx = Depends(require_admin)):
    policy = db.query(RetentionPolicy).filter(RetentionPolicy.workspace_id == ctx.workspace_id).first()
    return policy.to_api() if policy else {"workspace_id": ctx.workspace_id, "enabled": False, "legal_hold": False, "retention_days": DEFAULT_DAYS, "next_run_at": None, "updated_by": None}


@router.put("/retention")
def update_retention(body: RetentionUpdate, db: Session = Depends(get_db), ctx: WorkspaceCtx = Depends(require_admin)):
    try: days = normalized_days(body.retention_days)
    except ValueError as exc: raise HTTPException(status_code=422, detail=str(exc))
    policy = db.query(RetentionPolicy).filter(RetentionPolicy.workspace_id == ctx.workspace_id).first()
    if policy is None:
        policy = RetentionPolicy(workspace_id=ctx.workspace_id); db.add(policy)
    policy.enabled, policy.legal_hold, policy.retention_days, policy.updated_by = body.enabled, body.legal_hold, days, ctx.user.id
    schedule_policy(db, policy); db.refresh(policy)
    return policy.to_api()


@router.post("/retention/preview")
def preview_policy(db: Session = Depends(get_db), ctx: WorkspaceCtx = Depends(require_admin)):
    policy = db.query(RetentionPolicy).filter(RetentionPolicy.workspace_id == ctx.workspace_id).first()
    days = normalized_days(policy.retention_days if policy else DEFAULT_DAYS)
    return {"retention_days": days, "legal_hold": bool(policy and policy.legal_hold), "expired_counts": preview_retention(db, ctx.workspace_id, days)}


@router.post("/retention/enforce", status_code=202)
def enforce_policy(body: RetentionEnforce, db: Session = Depends(get_db), ctx: WorkspaceCtx = Depends(require_admin)):
    if body.confirmation != "PURGE EXPIRED DATA":
        raise HTTPException(status_code=422, detail="confirmation must equal PURGE EXPIRED DATA")
    policy = db.query(RetentionPolicy).filter(RetentionPolicy.workspace_id == ctx.workspace_id).first()
    if policy is None: raise HTTPException(status_code=409, detail="Configure a retention policy first")
    if policy.legal_hold: raise HTTPException(status_code=409, detail="Legal hold blocks retention enforcement")
    run = RetentionRun(workspace_id=ctx.workspace_id, requested_by=str(ctx.user.id), policy_snapshot=normalized_days(policy.retention_days or {}))
    db.add(run); db.commit(); db.refresh(run)
    from apps.api.services.queue_service import queue_service
    queue_service.add_job(db, "retention_enforce", {"workspace_id": ctx.workspace_id, "run_id": run.id}, fire_key=f"retention-run:{run.id}")
    return run.to_api()


@router.get("/retention/runs")
def retention_runs(db: Session = Depends(get_db), ctx: WorkspaceCtx = Depends(require_admin)):
    return [row.to_api() for row in db.query(RetentionRun).filter(RetentionRun.workspace_id == ctx.workspace_id).order_by(RetentionRun.created_at.desc()).limit(100).all()]
