import csv
import io
from datetime import datetime
from typing import Optional

from fastapi import APIRouter, Depends, HTTPException, Query
from fastapi.responses import StreamingResponse
from sqlalchemy.orm import Session
from pydantic import BaseModel, Field
from typing import Literal

from apps.api.core.tenancy import WorkspaceCtx, require_workspace_role
from apps.api.database import get_db
from apps.api.services.governance.models import GovernanceAuditEvent, RetentionPolicy, RetentionRun
from apps.api.services.governance.retention import DEFAULT_DAYS, normalized_days, preview_retention, schedule_policy
from apps.api.models import User
from apps.api.services.workspace import manager as workspace_manager

router = APIRouter(prefix="/api/governance", tags=["governance"])
require_admin = require_workspace_role("admin", permission="governance.manage")


class RetentionUpdate(BaseModel):
    enabled: bool
    legal_hold: bool = False
    retention_days: dict[str, int] = Field(default_factory=dict)


class RetentionEnforce(BaseModel):
    confirmation: str


PERMISSIONS = {
    "tables.write": ("Tables", "Create, edit and run workbooks", ["editor", "admin"]),
    "integrations.manage": ("Integrations", "Configure enrichment and integration credentials", ["editor", "admin"]),
    "campaigns.write": ("Campaigns", "Create and operate campaigns", ["editor", "admin"]),
    "audiences.write": ("Audiences", "Create, refresh and schedule audiences", ["editor", "admin"]),
    "activation.write": ("Activation", "Configure and run destinations", ["editor", "admin"]),
    "agents.write": ("Agents", "Create and run research playbooks", ["editor", "admin"]),
    "signals.write": ("Signals", "Scan and manage buying signals", ["editor", "admin"]),
    "outreach.write": ("Outreach", "Configure and execute outreach", ["admin"]),
    "automations.manage": ("Automations", "Configure trigger automation", ["admin"]),
    "secrets.manage": ("Secrets", "Rotate machine credentials", ["admin"]),
    "governance.manage": ("Governance", "Audit, retention and access policy", ["admin"]),
}


class PermissionUpdate(BaseModel):
    effect: Literal["allow", "deny"] | None = None


class MemberCreate(BaseModel):
    username: str = Field(min_length=1, max_length=150)
    role: Literal["viewer", "member", "editor", "admin"] = "viewer"


class MemberRoleUpdate(BaseModel):
    role: Literal["viewer", "member", "editor", "admin"]


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


@router.get("/rbac")
def get_rbac(db: Session = Depends(get_db), ctx: WorkspaceCtx = Depends(require_admin)):
    members = workspace_manager.list_members(ctx.workspace_id)
    users = {user.id: user for user in db.query(User).filter(User.id.in_([member["user_id"] for member in members])).all()} if members else {}
    return {
        "permissions": [{"key": key, "label": value[0], "description": value[1], "default_roles": value[2]} for key, value in PERMISSIONS.items()],
        "members": [{**member, "username": users[member["user_id"]].username if member["user_id"] in users else f"User {member['user_id']}", "overrides": workspace_manager.member_permissions(ctx.workspace_id, member["user_id"])} for member in members],
    }


@router.put("/rbac/{user_id}/{permission}")
def update_rbac(user_id: int, permission: str, body: PermissionUpdate, ctx: WorkspaceCtx = Depends(require_admin)):
    if permission not in PERMISSIONS:
        raise HTTPException(status_code=404, detail="Unknown workspace permission")
    role = workspace_manager.member_role(ctx.workspace_id, user_id)
    if role is None:
        raise HTTPException(status_code=404, detail="Workspace member not found")
    if role == "owner":
        raise HTTPException(status_code=409, detail="Owner permissions cannot be overridden")
    workspace_manager.set_member_permission(ctx.workspace_id, user_id, permission, body.effect)
    return {"user_id": user_id, "permission": permission, "effect": body.effect, "effective": body.effect == "allow" or (body.effect is None and role in PERMISSIONS[permission][2])}


@router.post("/members", status_code=201)
def add_workspace_member(body: MemberCreate, db: Session = Depends(get_db), ctx: WorkspaceCtx = Depends(require_admin)):
    user = db.query(User).filter(User.username == body.username.strip()).first()
    if user is None:
        raise HTTPException(status_code=404, detail="User account not found")
    if workspace_manager.is_member(ctx.workspace_id, user.id):
        raise HTTPException(status_code=409, detail="User is already a workspace member")
    workspace_manager.add_member(ctx.workspace_id, user.id, body.role)
    return {"user_id": user.id, "username": user.username, "role": body.role, "overrides": {}}


@router.patch("/members/{user_id}")
def update_workspace_member(user_id: int, body: MemberRoleUpdate, ctx: WorkspaceCtx = Depends(require_admin)):
    role = workspace_manager.member_role(ctx.workspace_id, user_id)
    if role is None:
        raise HTTPException(status_code=404, detail="Workspace member not found")
    if role == "owner":
        raise HTTPException(status_code=409, detail="Owner role cannot be changed")
    workspace_manager.set_member_role(ctx.workspace_id, user_id, body.role)
    return {"user_id": user_id, "role": body.role, "overrides": workspace_manager.member_permissions(ctx.workspace_id, user_id)}


@router.delete("/members/{user_id}", status_code=204)
def remove_workspace_member(user_id: int, ctx: WorkspaceCtx = Depends(require_admin)):
    role = workspace_manager.member_role(ctx.workspace_id, user_id)
    if role is None:
        raise HTTPException(status_code=404, detail="Workspace member not found")
    if role == "owner":
        raise HTTPException(status_code=409, detail="Workspace owner cannot be removed")
    workspace_manager.remove_member(ctx.workspace_id, user_id)
