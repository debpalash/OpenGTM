"""
HubSpot CRM Router — Connect, sync leads, and manage field mappings.

Every endpoint is authenticated and workspace-scoped (spec WI-6): the HubSpot
token is stored as a per-workspace encrypted secret, lead reads go through the
caller's tenant-scoped lead store, and pushes carry the workspace_id so Team A's
token never touches Team B's data. (Previously these routes were anonymous — a
public /connect could overwrite a shared global token and a public /sync could
exfiltrate every lead to an attacker's portal.)
"""

from fastapi import APIRouter, Depends, HTTPException
from pydantic import BaseModel
from typing import List, Optional, Dict

from apps.api.core.tenancy import WorkspaceCtx, current_workspace, require_workspace_role

router = APIRouter(prefix="/api/crm/hubspot", tags=["CRM"])
require_editor = require_workspace_role("editor", "admin")


class ConnectRequest(BaseModel):
    token: str


class SyncRequest(BaseModel):
    lead_ids: Optional[List[int]] = None
    tier: Optional[str] = None
    limit: int = 50


class FieldMapUpdate(BaseModel):
    field_map: Dict[str, str]


@router.get("/status")
async def hubspot_status(ctx: WorkspaceCtx = Depends(current_workspace)):
    """Check HubSpot connection status for the caller's workspace."""
    from apps.api.services.crm.hubspot import is_connected, test_connection
    if not is_connected(ctx.workspace_id):
        return {"connected": False, "error": "No HubSpot token configured"}
    result = await test_connection(ctx.workspace_id)
    return result


@router.post("/connect")
def hubspot_connect(req: ConnectRequest, ctx: WorkspaceCtx = Depends(require_editor)):
    """Save the HubSpot Private App token as this workspace's encrypted secret."""
    from apps.api.services.workspace.secrets import set_secret
    set_secret(ctx.workspace_id, "HUBSPOT_TOKEN", req.token)
    return {"status": "ok", "message": "HubSpot token saved"}


@router.post("/sync")
async def hubspot_sync(req: SyncRequest, ctx: WorkspaceCtx = Depends(require_editor)):
    """Push this workspace's leads to HubSpot as contacts."""
    from apps.api.services.crm.hubspot import push_leads_batch, is_connected

    if not is_connected(ctx.workspace_id):
        raise HTTPException(status_code=400, detail="HubSpot not connected. Add token first.")

    db = ctx.lead_db()
    try:
        if req.lead_ids:
            leads = [db.get_lead(lid) for lid in req.lead_ids]
            leads = [l for l in leads if l and l.email]
        elif req.tier:
            all_leads = db.get_leads(score_tier=req.tier, limit=req.limit)
            leads = [l for l in all_leads if l.email]
        else:
            all_leads = db.get_leads(limit=req.limit)
            leads = [l for l in all_leads if l.email]
    finally:
        db.close()

    if not leads:
        return {"created": 0, "updated": 0, "failed": 0, "message": "No leads with emails to sync"}

    result = await push_leads_batch(leads, workspace_id=ctx.workspace_id)
    return {
        "created": result.created,
        "updated": result.updated,
        "failed": result.failed,
        "errors": result.errors[:5],  # Cap displayed errors
        "total_synced": result.created + result.updated,
    }


@router.get("/contacts")
async def hubspot_contacts(limit: int = 20, ctx: WorkspaceCtx = Depends(current_workspace)):
    """Fetch recent contacts from the workspace's connected HubSpot portal."""
    from apps.api.services.crm.hubspot import get_contacts, is_connected
    if not is_connected(ctx.workspace_id):
        raise HTTPException(status_code=400, detail="HubSpot not connected")
    contacts = await get_contacts(limit, workspace_id=ctx.workspace_id)
    return {"contacts": contacts, "count": len(contacts)}
