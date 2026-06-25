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

from contextvars import ContextVar
from dataclasses import dataclass
from typing import Optional

from fastapi import Depends, Header, HTTPException

from apps.api.models import User
from apps.api.core.security import get_current_active_user
from apps.api.services.workspace import manager as ws_manager


# Active workspace id for the CURRENT execution context (request or worker unit
# of work). This is the single source of truth the SQLAlchemy `after_begin` hook
# reads to emit `SET LOCAL app.workspace_id` on Postgres, so RLS scopes the txn.
# It is NOT request-global mutable state shared across requests: contextvars are
# per-asyncio-task / per-thread, so concurrent requests never see each other's
# value. Workers set it explicitly per job via `workspace_scope(...)`.
current_workspace_var: ContextVar[Optional[str]] = ContextVar(
    "current_workspace", default=None
)


@dataclass
class WorkspaceCtx:
    """Resolved tenant context for a request."""

    user: User
    workspace_id: str
    slug: str

    def lead_db(self):
        """Return the tenant-scoped lead store for this workspace.

        Backend-detected (mirrors database.py:IS_SQLITE):
          * Postgres + PG_LEAD_STORE → :class:`PgLeadStore` over the shared,
            RLS-protected `leads`/`signals` tables, scoped to this workspace_id.
          * otherwise → legacy per-workspace SQLite :class:`LeadDB`.

        Also publishes this workspace into the `current_workspace` contextvar so
        the SQLAlchemy session hook sets `app.workspace_id` for RLS.
        """
        from apps.api.services.leadgen.store import get_lead_store

        # Publish for the RLS session hook (also done in get_lead_store, but
        # setting here keeps the GUC correct for any direct ORM session opened
        # within this request after lead_db()).
        current_workspace_var.set(self.workspace_id)
        return get_lead_store(self.workspace_id, self.slug)


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

    # Publish the resolved tenant for the duration of the request so the RLS
    # session hook (after_begin) scopes every PG transaction to this workspace.
    current_workspace_var.set(ws_id)
    return WorkspaceCtx(user=user, workspace_id=ws_id, slug=slug)


from contextlib import contextmanager


@contextmanager
def workspace_scope(workspace_id: str):
    """Bind ``workspace_id`` as the active tenant for a unit of work.

    For stateless workers / background jobs / CLI that have no FastAPI request:
    wrap each per-tenant unit of work so the RLS GUC is set on Postgres and any
    LeadStore opened inside is scoped correctly. Restores the previous value on
    exit (so a pooled worker thread never leaks tenant context across jobs).

        with workspace_scope(job.workspace_id):
            store = get_lead_store(job.workspace_id, slug)
            ...
    """
    if not workspace_id:
        raise ValueError(
            "workspace_scope requires a non-empty workspace_id "
            "(empty would yield zero rows under RLS and silently mask the bug)."
        )
    token = current_workspace_var.set(workspace_id)
    try:
        yield
    finally:
        current_workspace_var.reset(token)


def require_workspace_role(*roles: str):
    """Dependency factory enforcing the caller has one of ``roles`` (or owns it)."""

    def _dep(ctx: WorkspaceCtx = Depends(current_workspace)) -> WorkspaceCtx:
        role = ws_manager.member_role(ctx.workspace_id, ctx.user.id)
        # Workspace owner implicitly satisfies any role requirement.
        if role in roles or role == "owner":
            return ctx
        raise HTTPException(status_code=403, detail="Insufficient workspace role")

    return _dep
