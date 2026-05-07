"""
Workspaces Router — Multi-workspace management for agencies.
"""

from fastapi import APIRouter, HTTPException
from pydantic import BaseModel
from typing import Optional

router = APIRouter(prefix="/api/workspaces", tags=["workspaces"])


class CreateWorkspaceRequest(BaseModel):
    name: str
    description: str = ""
    icon: str = "🏢"


class SwitchWorkspaceRequest(BaseModel):
    workspace_id: str


@router.get("")
def list_workspaces():
    """List all workspaces."""
    from apps.api.services.workspace.manager import list_workspaces as _list, _get_active_workspace_id
    workspaces = _list()
    active_id = _get_active_workspace_id()
    return {
        "workspaces": [
            {
                "id": ws.id,
                "name": ws.name,
                "slug": ws.slug,
                "description": ws.description,
                "icon": ws.icon,
                "leads_count": ws.leads_count,
                "is_active": ws.id == active_id,
                "created_at": ws.created_at,
            }
            for ws in workspaces
        ],
        "active_id": active_id,
    }


@router.post("")
def create_workspace(req: CreateWorkspaceRequest):
    """Create a new workspace."""
    from apps.api.services.workspace.manager import create_workspace as _create
    ws = _create(name=req.name, description=req.description, icon=req.icon)
    return {"id": ws.id, "name": ws.name, "slug": ws.slug}


@router.post("/switch")
def switch_workspace(req: SwitchWorkspaceRequest):
    """Switch the active workspace."""
    from apps.api.services.workspace.manager import get_workspace, _set_active_workspace
    ws = get_workspace(req.workspace_id)
    if not ws:
        raise HTTPException(status_code=404, detail="Workspace not found")
    _set_active_workspace(ws.id)
    return {"active": ws.name, "id": ws.id}


@router.get("/dashboard")
def agency_dashboard():
    """Cross-workspace analytics."""
    from apps.api.services.workspace.manager import get_agency_dashboard
    return get_agency_dashboard()


@router.get("/{ws_id}")
def get_workspace(ws_id: str):
    """Get workspace details."""
    from apps.api.services.workspace.manager import get_workspace as _get
    ws = _get(ws_id)
    if not ws:
        raise HTTPException(status_code=404, detail="Workspace not found")
    return {
        "id": ws.id, "name": ws.name, "slug": ws.slug,
        "description": ws.description, "icon": ws.icon,
        "leads_count": ws.leads_count, "created_at": ws.created_at,
    }


@router.delete("/{ws_id}")
def delete_workspace(ws_id: str):
    """Delete a workspace (cannot delete default)."""
    from apps.api.services.workspace.manager import delete_workspace as _delete
    if not _delete(ws_id):
        raise HTTPException(status_code=400, detail="Cannot delete default workspace")
    return {"status": "ok"}
