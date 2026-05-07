"""
HubSpot CRM Router — Connect, sync leads, and manage field mappings.
"""

from fastapi import APIRouter, HTTPException
from pydantic import BaseModel
from typing import List, Optional, Dict

router = APIRouter(prefix="/api/crm/hubspot", tags=["CRM"])


class ConnectRequest(BaseModel):
    token: str


class SyncRequest(BaseModel):
    lead_ids: Optional[List[int]] = None
    tier: Optional[str] = None
    limit: int = 50


class FieldMapUpdate(BaseModel):
    field_map: Dict[str, str]


@router.get("/status")
async def hubspot_status():
    """Check HubSpot connection status."""
    from apps.api.services.crm.hubspot import is_connected, test_connection
    if not is_connected():
        return {"connected": False, "error": "No HubSpot token configured"}
    result = await test_connection()
    return result


@router.post("/connect")
def hubspot_connect(req: ConnectRequest):
    """Save HubSpot Private App token."""
    from apps.api.routers.settings import _db_set
    _db_set("HUBSPOT_TOKEN", req.token)
    return {"status": "ok", "message": "HubSpot token saved"}


@router.post("/sync")
async def hubspot_sync(req: SyncRequest):
    """Push leads to HubSpot as contacts."""
    from apps.api.services.crm.hubspot import push_leads_batch, is_connected
    from apps.api.services.leadgen.db import LeadDB

    if not is_connected():
        raise HTTPException(status_code=400, detail="HubSpot not connected. Add token first.")

    db = LeadDB()

    if req.lead_ids:
        leads = [db.get_lead(lid) for lid in req.lead_ids]
        leads = [l for l in leads if l and l.email]
    elif req.tier:
        all_leads = db.get_leads(score_tier=req.tier, limit=req.limit)
        leads = [l for l in all_leads if l.email]
    else:
        all_leads = db.get_leads(limit=req.limit)
        leads = [l for l in all_leads if l.email]

    db.close()

    if not leads:
        return {"created": 0, "updated": 0, "failed": 0, "message": "No leads with emails to sync"}

    result = await push_leads_batch(leads)
    return {
        "created": result.created,
        "updated": result.updated,
        "failed": result.failed,
        "errors": result.errors[:5],  # Cap displayed errors
        "total_synced": result.created + result.updated,
    }


@router.get("/contacts")
async def hubspot_contacts(limit: int = 20):
    """Fetch recent contacts from HubSpot."""
    from apps.api.services.crm.hubspot import get_contacts, is_connected
    if not is_connected():
        raise HTTPException(status_code=400, detail="HubSpot not connected")
    contacts = await get_contacts(limit)
    return {"contacts": contacts, "count": len(contacts)}
