"""
HubSpot CRM Client — Push leads as contacts, pull updates.

Uses HubSpot API v3 (contacts, companies).
BYOK: requires user's HubSpot Private App Access Token.
Docs: https://developers.hubspot.com/docs/api/crm/contacts
"""

import os
import logging
import time
from typing import Dict, List, Optional, Any
from dataclasses import dataclass

import httpx

logger = logging.getLogger("crm.hubspot")

BASE_URL = "https://api.hubapi.com"


def _get_token() -> str:
    """Read HubSpot token from settings DB or environment."""
    try:
        from apps.api.routers.settings import _db_get
        return _db_get("HUBSPOT_TOKEN", "") or os.getenv("HUBSPOT_TOKEN", "")
    except Exception:
        return os.getenv("HUBSPOT_TOKEN", "")


def is_connected() -> bool:
    """Check if HubSpot token is configured."""
    return bool(_get_token())


# ── Default Field Mapping ─────────────────────────────────────
# Maps Yupcha lead fields → HubSpot contact properties

DEFAULT_FIELD_MAP = {
    "company": "company",
    "email": "email",
    "phone": "phone",
    "website": "website",
    "city": "city",
    "contact_person": "firstname",  # HubSpot splits first/last
    "contact_title": "jobtitle",
    "linkedin_url": "hs_linkedinid",
    "score": "hs_lead_status",
}


@dataclass
class SyncResult:
    created: int = 0
    updated: int = 0
    failed: int = 0
    errors: List[str] = None

    def __post_init__(self):
        if self.errors is None:
            self.errors = []


async def test_connection() -> Dict[str, Any]:
    """Test HubSpot connection by fetching account info."""
    token = _get_token()
    if not token:
        return {"connected": False, "error": "No HubSpot token configured"}

    try:
        async with httpx.AsyncClient(timeout=10) as client:
            resp = await client.get(
                f"{BASE_URL}/account-info/v3/details",
                headers={"Authorization": f"Bearer {token}"},
            )
            if resp.status_code == 200:
                data = resp.json()
                return {
                    "connected": True,
                    "portal_id": data.get("portalId"),
                    "time_zone": data.get("timeZone"),
                }
            else:
                return {"connected": False, "error": f"HTTP {resp.status_code}: {resp.text[:200]}"}
    except Exception as e:
        return {"connected": False, "error": str(e)}


async def push_lead_as_contact(lead, field_map: Dict[str, str] = None) -> Dict[str, Any]:
    """Push a single lead to HubSpot as a contact."""
    token = _get_token()
    if not token:
        return {"success": False, "error": "No HubSpot token"}

    fmap = field_map or DEFAULT_FIELD_MAP
    properties = {}

    for lead_field, hs_field in fmap.items():
        value = getattr(lead, lead_field, None)
        if value:
            # Special handling for contact_person → split into first/last
            if lead_field == "contact_person" and " " in str(value):
                parts = str(value).split(" ", 1)
                properties["firstname"] = parts[0]
                properties["lastname"] = parts[1]
            else:
                properties[hs_field] = str(value)

    if not properties.get("email"):
        return {"success": False, "error": "Lead has no email — required for HubSpot contact"}

    try:
        async with httpx.AsyncClient(timeout=15) as client:
            # First try to find existing contact by email
            search_resp = await client.post(
                f"{BASE_URL}/crm/v3/objects/contacts/search",
                headers={
                    "Authorization": f"Bearer {token}",
                    "Content-Type": "application/json",
                },
                json={
                    "filterGroups": [{
                        "filters": [{
                            "propertyName": "email",
                            "operator": "EQ",
                            "value": properties["email"],
                        }]
                    }],
                    "limit": 1,
                },
            )

            if search_resp.status_code == 200:
                results = search_resp.json().get("results", [])
                if results:
                    # Update existing contact
                    contact_id = results[0]["id"]
                    update_resp = await client.patch(
                        f"{BASE_URL}/crm/v3/objects/contacts/{contact_id}",
                        headers={
                            "Authorization": f"Bearer {token}",
                            "Content-Type": "application/json",
                        },
                        json={"properties": properties},
                    )
                    return {
                        "success": update_resp.status_code == 200,
                        "action": "updated",
                        "hubspot_id": contact_id,
                    }

            # Create new contact
            create_resp = await client.post(
                f"{BASE_URL}/crm/v3/objects/contacts",
                headers={
                    "Authorization": f"Bearer {token}",
                    "Content-Type": "application/json",
                },
                json={"properties": properties},
            )

            if create_resp.status_code == 201:
                data = create_resp.json()
                return {
                    "success": True,
                    "action": "created",
                    "hubspot_id": data.get("id"),
                }
            else:
                return {
                    "success": False,
                    "error": f"HTTP {create_resp.status_code}: {create_resp.text[:200]}",
                }

    except Exception as e:
        logger.error(f"HubSpot push failed: {e}")
        return {"success": False, "error": str(e)}


async def push_leads_batch(leads, field_map: Dict[str, str] = None) -> SyncResult:
    """Push multiple leads to HubSpot."""
    result = SyncResult()

    for lead in leads:
        push_result = await push_lead_as_contact(lead, field_map)
        if push_result.get("success"):
            if push_result.get("action") == "created":
                result.created += 1
            else:
                result.updated += 1
        else:
            result.failed += 1
            result.errors.append(f"Lead {lead.id}: {push_result.get('error', 'Unknown error')}")

        # Rate limit — HubSpot allows 100/10s for private apps
        import asyncio
        await asyncio.sleep(0.15)

    return result


async def get_contacts(limit: int = 20) -> List[Dict]:
    """Fetch recent contacts from HubSpot."""
    token = _get_token()
    if not token:
        return []

    try:
        async with httpx.AsyncClient(timeout=15) as client:
            resp = await client.get(
                f"{BASE_URL}/crm/v3/objects/contacts",
                headers={"Authorization": f"Bearer {token}"},
                params={
                    "limit": limit,
                    "properties": "email,firstname,lastname,company,phone,jobtitle",
                },
            )
            if resp.status_code == 200:
                return resp.json().get("results", [])
            return []
    except Exception as e:
        logger.error(f"HubSpot fetch failed: {e}")
        return []
