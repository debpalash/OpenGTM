"""
Workspaces Router — tenant-aware multi-workspace management.

Canonical workspace API (backed by workspaces.db, which carries ownership and
membership). All endpoints require auth and are scoped to the caller's
memberships.
"""

from fastapi import APIRouter, HTTPException, Depends
from pydantic import BaseModel

from apps.api.models import User
from apps.api.core.security import get_current_active_user
from apps.api.services.workspace import manager as ws

router = APIRouter(prefix="/api/workspaces", tags=["workspaces"])


class CreateWorkspaceRequest(BaseModel):
    name: str
    description: str = ""
    icon: str = "🏢"


class SwitchWorkspaceRequest(BaseModel):
    workspace_id: str


def _serialize(w: ws.Workspace, active_id: str) -> dict:
    return {
        "id": w.id,
        "name": w.name,
        "slug": w.slug,
        "description": w.description,
        "icon": w.icon,
        "owner_id": w.owner_id,
        "leads_count": w.leads_count,
        "is_active": w.id == active_id,
        "created_at": w.created_at,
    }


@router.get("")
def list_workspaces(user: User = Depends(get_current_active_user)):
    """List workspaces the caller belongs to."""
    active_id = ws.get_user_active_workspace(user.id)
    workspaces = ws.list_user_workspaces(user.id)
    return {
        "workspaces": [_serialize(w, active_id) for w in workspaces],
        "active_id": active_id,
    }


@router.post("")
def create_workspace(
    req: CreateWorkspaceRequest, user: User = Depends(get_current_active_user)
):
    """Create a workspace owned by the caller (who becomes a member)."""
    w = ws.create_workspace(
        name=req.name, description=req.description, icon=req.icon, owner_id=user.id
    )
    # New workspaces become the caller's active one for convenience.
    ws.set_user_active_workspace(user.id, w.id)
    return {"id": w.id, "name": w.name, "slug": w.slug}


@router.post("/switch")
def switch_workspace(
    req: SwitchWorkspaceRequest, user: User = Depends(get_current_active_user)
):
    """Switch the caller's active workspace (must be a member)."""
    if not ws.set_user_active_workspace(user.id, req.workspace_id):
        raise HTTPException(status_code=403, detail="Workspace access denied")
    w = ws.get_workspace(req.workspace_id)
    return {"active": w.name, "id": w.id}


@router.get("/dashboard")
def agency_dashboard(user: User = Depends(get_current_active_user)):
    """Cross-workspace analytics over the caller's workspaces."""
    workspaces = ws.list_user_workspaces(user.id)
    total_leads = sum(w.leads_count for w in workspaces)
    return {
        "total_workspaces": len(workspaces),
        "total_leads": total_leads,
        "workspaces": [
            {
                "id": w.id, "name": w.name, "slug": w.slug, "icon": w.icon,
                "leads_count": w.leads_count, "created_at": w.created_at,
            }
            for w in workspaces
        ],
    }


@router.get("/{ws_id}")
def get_workspace(ws_id: str, user: User = Depends(get_current_active_user)):
    """Get workspace details (must be a member)."""
    if not ws.is_member(ws_id, user.id):
        raise HTTPException(status_code=404, detail="Workspace not found")
    w = ws.get_workspace(ws_id)
    if not w:
        raise HTTPException(status_code=404, detail="Workspace not found")
    return {
        "id": w.id, "name": w.name, "slug": w.slug,
        "description": w.description, "icon": w.icon, "owner_id": w.owner_id,
        "leads_count": w.leads_count, "created_at": w.created_at,
    }


@router.delete("/{ws_id}")
def delete_workspace(ws_id: str, user: User = Depends(get_current_active_user)):
    """Delete a workspace (owner only; cannot delete the default)."""
    w = ws.get_workspace(ws_id)
    if not w or not ws.is_member(ws_id, user.id):
        raise HTTPException(status_code=404, detail="Workspace not found")
    if w.owner_id != user.id:
        raise HTTPException(status_code=403, detail="Only the owner can delete a workspace")
    if not ws.delete_workspace(ws_id):
        raise HTTPException(status_code=400, detail="Cannot delete default workspace")
    return {"status": "ok"}
