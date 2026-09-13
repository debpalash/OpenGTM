import csv
import io
from datetime import datetime
from typing import Optional

from fastapi import APIRouter, Depends, Query
from fastapi.responses import StreamingResponse
from sqlalchemy.orm import Session

from apps.api.core.tenancy import WorkspaceCtx, require_workspace_role
from apps.api.database import get_db
from apps.api.services.governance.models import GovernanceAuditEvent

router = APIRouter(prefix="/api/governance", tags=["governance"])
require_admin = require_workspace_role("admin")


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
