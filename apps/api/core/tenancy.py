"""
Tenancy — per-workspace data isolation.

Provides FastAPI dependencies that resolve the caller's *active workspace* and
verify membership, so every data endpoint can scope its queries to a tenant the
user is actually allowed to access.

Tenant model (see services/workspace/manager.py):
  - A workspace is owned by a user and has a set of members.
  - Each user has an active workspace (per-user, not global).
  - Leads live in a per-workspace SQLite file; workbooks carry a workspace_id.

Usage in a router::

    from apps.api.core.tenancy import WorkspaceCtx, current_workspace

    @router.get("/leads")
    def list_leads(ctx: WorkspaceCtx = Depends(current_workspace)):
        db = ctx.lead_db()
        ...
"""

from dataclasses import dataclass
from typing import Optional

from fastapi import Depends, Header, HTTPException

from apps.api.models import User
from apps.api.core.security import get_current_active_user
from apps.api.services.workspace import manager as ws_manager


@dataclass
class WorkspaceCtx:
    """Resolved tenant context for a request."""

    user: User
    workspace_id: str
    slug: str

    def lead_db(self):
        """Open a LeadDB bound to this workspace's data file."""
        from apps.api.services.leadgen.db import LeadDB

        return LeadDB(ws_manager.workspace_leads_db_path(self.slug))


def current_workspace(
    user: User = Depends(get_current_active_user),
    x_workspace_id: Optional[str] = Header(default=None),
) -> WorkspaceCtx:
    """Resolve and authorize the caller's workspace.

    The workspace is taken from the ``X-Workspace-Id`` header when present
    (so the frontend can act in a specific workspace), otherwise from the
    user's stored active workspace. Either way, membership is enforced.
    """
    ws_id = x_workspace_id or ws_manager.get_user_active_workspace(user.id)

    if not ws_id:
        raise HTTPException(
            status_code=403,
            detail="No accessible workspace. Ask an admin to add you to one.",
        )

    if not ws_manager.is_member(ws_id, user.id):
        # Don't leak existence — same response whether the workspace is
        # missing or simply not the caller's.
        raise HTTPException(status_code=403, detail="Workspace access denied")

    slug = ws_manager.workspace_slug(ws_id)
    if not slug:
        raise HTTPException(status_code=404, detail="Workspace not found")

    return WorkspaceCtx(user=user, workspace_id=ws_id, slug=slug)


def require_workspace_role(*roles: str):
    """Dependency factory enforcing the caller has one of ``roles`` (or owns it)."""

    def _dep(ctx: WorkspaceCtx = Depends(current_workspace)) -> WorkspaceCtx:
        role = ws_manager.member_role(ctx.workspace_id, ctx.user.id)
        # Workspace owner implicitly satisfies any role requirement.
        if role in roles or role == "owner":
            return ctx
        raise HTTPException(status_code=403, detail="Insufficient workspace role")

    return _dep
