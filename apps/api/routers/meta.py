"""
Meta / foundation read-only endpoints for the GTM-automation UI.

Two tiny, auth'd, workspace-scoped endpoints the SPA uses for *proactive* gating
(disable/hide write controls + flagged features up front), with the reactive
403/404/409 handling kept as a backstop:

  * ``GET /api/me/context`` → the caller's per-workspace role for the ACTIVE
    workspace (admin | editor | member; ``owner`` is reported verbatim and the UI
    treats it as having every permission).
  * ``GET /api/flags``      → the runtime feature flags the UI branches on.

Both depend on ``current_workspace`` so they are authenticated and resolve the
active workspace (via ``X-Workspace-Id`` header or the user's stored active ws),
exactly like every other data endpoint.
"""

from __future__ import annotations

from fastapi import APIRouter, Depends
from pydantic import BaseModel

from apps.api.core.config import settings
from apps.api.core.tenancy import WorkspaceCtx, current_workspace
from apps.api.services.workspace import manager as ws_manager

router = APIRouter(prefix="/api", tags=["meta"])


class MeContext(BaseModel):
    user_id: str
    workspace_id: str
    # The caller's role IN THE ACTIVE WORKSPACE. One of owner|admin|editor|member
    # (owner implicitly satisfies any role requirement; see require_workspace_role).
    role: str
    is_owner: bool


class Flags(BaseModel):
    automations_enabled: bool
    intent_poller_enabled: bool
    pg_lead_store: bool
    allow_legacy_outreach: bool


@router.get("/me/context", response_model=MeContext)
def me_context(ctx: WorkspaceCtx = Depends(current_workspace)) -> MeContext:
    """Return the caller's role for the resolved active workspace.

    Membership is already enforced by ``current_workspace`` (403 otherwise), so a
    successful response always carries a real role. ``member_role`` returns the
    raw role stored in ``workspace_members`` (owner rows carry role ``owner``);
    if for any reason the row is missing we fall back to ``member`` (least
    privilege) rather than leaking elevated access.
    """
    role = ws_manager.member_role(ctx.workspace_id, ctx.user.id) or "member"
    return MeContext(
        user_id=str(ctx.user.id),
        workspace_id=ctx.workspace_id,
        role=role,
        is_owner=role == "owner",
    )


@router.get("/flags", response_model=Flags)
def flags(_ctx: WorkspaceCtx = Depends(current_workspace)) -> Flags:
    """Return the runtime feature flags the UI branches on.

    ``pg_lead_store`` reports the *effective* availability (``use_pg_store()``)
    rather than the raw config bool, because that is what actually gates the
    Postgres-only surfaces (intent watches, ``on_signal`` triggers). It degrades
    to the raw config flag if the store module can't be imported.
    """
    try:
        from apps.api.services.leadgen.store import use_pg_store

        pg = bool(use_pg_store())
    except Exception:
        pg = bool(getattr(settings, "PG_LEAD_STORE", False))

    return Flags(
        automations_enabled=bool(getattr(settings, "AUTOMATIONS_ENABLED", False)),
        intent_poller_enabled=bool(getattr(settings, "INTENT_POLLER_ENABLED", False)),
        pg_lead_store=pg,
        allow_legacy_outreach=bool(
            getattr(settings, "AUTOMATIONS_ALLOW_LEGACY_OUTREACH", False)
        ),
    )
