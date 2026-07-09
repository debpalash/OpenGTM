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


def _get_token(workspace_id: Optional[str] = None) -> str:
    """Read HubSpot token.

    When ``workspace_id`` is given, resolve the per-workspace encrypted secret
    first (spec WI-6), falling back to the global settings DB / environment so
    single-tenant installs are unaffected.
    """
    try:
        # get_secret() already does per-workspace → global settings → env.
        from apps.api.services.workspace.secrets import get_secret
        return get_secret(workspace_id, "HUBSPOT_TOKEN", "")
    except Exception:
        return os.getenv("HUBSPOT_TOKEN", "")


def is_connected(workspace_id: Optional[str] = None) -> bool:
    """Check if a HubSpot token is configured (per-workspace, else global)."""
    return bool(_get_token(workspace_id))


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


async def test_connection(workspace_id: Optional[str] = None) -> Dict[str, Any]:
    """Test HubSpot connection by fetching account info (per-workspace token)."""
    token = _get_token(workspace_id)
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


async def push_lead_as_contact(lead, field_map: Dict[str, str] = None,
                               workspace_id: Optional[str] = None) -> Dict[str, Any]:
    """Push a single lead to HubSpot as a contact.

    ``workspace_id`` selects the per-workspace token when set (spec WI-6).
    """
    token = _get_token(workspace_id)
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


async def push_leads_batch(leads, field_map: Dict[str, str] = None,
                           workspace_id: Optional[str] = None) -> SyncResult:
    """Push multiple leads to HubSpot using the workspace's token (spec WI-6)."""
    result = SyncResult()

    for lead in leads:
        push_result = await push_lead_as_contact(lead, field_map, workspace_id=workspace_id)
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


async def update_contact_by_id(contact_id: str, properties: Dict[str, str],
                               workspace_id: Optional[str] = None) -> Dict[str, Any]:
    """PATCH an existing contact by its HubSpot id (CRM-sync write-back).

    Returns {"success": True, "action": "updated", "hubspot_id": id} on 200,
    {"success": False, "not_found": True, ...} on 404 (deleted in HubSpot —
    caller falls back to create), a clean error dict otherwise. Never raises
    for HTTP-level failures; 429 is retried (bounded, Retry-After honored).
    """
    token = _get_token(workspace_id)
    if not token:
        return {"success": False, "error": "No HubSpot token"}
    if not properties:
        return {"success": False, "error": "no mapped fields to update"}

    from apps.api.services.crm import request_with_retry
    try:
        async with httpx.AsyncClient(timeout=15) as client:
            resp = await request_with_retry(
                client, "PATCH",
                f"{BASE_URL}/crm/v3/objects/contacts/{contact_id}",
                headers={
                    "Authorization": f"Bearer {token}",
                    "Content-Type": "application/json",
                },
                json={"properties": properties},
            )
        if resp.status_code == 200:
            return {"success": True, "action": "updated", "hubspot_id": str(contact_id)}
        if resp.status_code == 404:
            return {"success": False, "not_found": True,
                    "error": f"contact {contact_id} not found in HubSpot"}
        return {"success": False,
                "error": f"HTTP {resp.status_code}: {resp.text[:160]}"}
    except Exception as e:
        logger.error(f"HubSpot update failed: {e}")
        return {"success": False, "error": str(e)[:200]}


# ── Import (pull contacts INTO Yupcha) ────────────────────────────────

# Default HubSpot contact properties to pull when no field_map is given.
IMPORT_DEFAULT_PROPERTIES = [
    "email", "firstname", "lastname", "company", "phone", "website", "jobtitle",
]


def _map_import_properties(props: Dict[str, Any],
                           field_map: Optional[Dict[str, str]]) -> Dict[str, Any]:
    """Map a HubSpot contact's properties dict → Yupcha lead fields.

    ``field_map`` is {hubspot_property: lead_field}. Without one, the default
    properties map to lead fields, with firstname+lastname merged into
    contact_person (the inverse of the push-side split).
    """
    props = props or {}
    if field_map:
        return {
            lead_field: props.get(crm_prop)
            for crm_prop, lead_field in field_map.items()
            if props.get(crm_prop) not in (None, "")
        }
    out: Dict[str, Any] = {}
    for crm_prop, lead_field in (
        ("email", "email"), ("company", "company"), ("phone", "phone"),
        ("website", "website"), ("jobtitle", "contact_title"),
    ):
        if props.get(crm_prop) not in (None, ""):
            out[lead_field] = props[crm_prop]
    name = " ".join(
        str(props.get(p) or "").strip() for p in ("firstname", "lastname")
    ).strip()
    if name:
        out["contact_person"] = name
    return out


async def fetch_contacts(limit: int = 500, list_id: Optional[str] = None,
                         field_map: Optional[Dict[str, str]] = None,
                         workspace_id: Optional[str] = None) -> Dict[str, Any]:
    """Pull up to ``limit`` contacts from HubSpot (CRM v3, cursor paging).

    ``list_id`` restricts the pull to a HubSpot list (memberships API + batch
    read); otherwise all contacts are paged. Returns
    {"success": True, "contacts": [lead-field dicts]} — each dict carries the
    sync key (crm_external_id / crm_type / crm_object) — or a clean
    {"success": False, "error": ...}. Never raises for HTTP failures.
    """
    token = _get_token(workspace_id)
    if not token:
        return {"success": False, "error": "No HubSpot token configured"}

    from apps.api.services.crm import request_with_retry
    properties = list(field_map.keys()) if field_map else list(IMPORT_DEFAULT_PROPERTIES)
    headers = {"Authorization": f"Bearer {token}", "Content-Type": "application/json"}

    def _row(obj: Dict[str, Any]) -> Dict[str, Any]:
        row = _map_import_properties(obj.get("properties") or {}, field_map)
        row["crm_external_id"] = str(obj.get("id"))
        row["crm_type"] = "hubspot"
        row["crm_object"] = "contact"
        return row

    contacts: List[Dict[str, Any]] = []
    try:
        async with httpx.AsyncClient(timeout=20) as client:
            if list_id:
                # List filter: page member record ids, then batch-read properties.
                record_ids: List[str] = []
                after: Optional[str] = None
                while len(record_ids) < limit:
                    params: Dict[str, Any] = {"limit": min(250, limit - len(record_ids))}
                    if after:
                        params["after"] = after
                    resp = await request_with_retry(
                        client, "GET",
                        f"{BASE_URL}/crm/v3/lists/{list_id}/memberships",
                        headers=headers, params=params,
                    )
                    if resp.status_code in (401, 403):
                        return {"success": False,
                                "error": f"HubSpot token invalid or expired (HTTP {resp.status_code})"}
                    if resp.status_code != 200:
                        return {"success": False,
                                "error": f"HTTP {resp.status_code}: {resp.text[:160]}"}
                    data = resp.json()
                    record_ids.extend(str(r.get("recordId")) for r in data.get("results", []))
                    after = ((data.get("paging") or {}).get("next") or {}).get("after")
                    if not after:
                        break
                record_ids = record_ids[:limit]
                for i in range(0, len(record_ids), 100):
                    chunk = record_ids[i:i + 100]
                    resp = await request_with_retry(
                        client, "POST",
                        f"{BASE_URL}/crm/v3/objects/contacts/batch/read",
                        headers=headers,
                        json={"properties": properties,
                              "inputs": [{"id": rid} for rid in chunk]},
                    )
                    if resp.status_code not in (200, 207):
                        return {"success": False,
                                "error": f"HTTP {resp.status_code}: {resp.text[:160]}"}
                    contacts.extend(_row(o) for o in resp.json().get("results", []))
            else:
                after = None
                while len(contacts) < limit:
                    params = {
                        "limit": min(100, limit - len(contacts)),
                        "properties": ",".join(properties),
                    }
                    if after:
                        params["after"] = after
                    resp = await request_with_retry(
                        client, "GET", f"{BASE_URL}/crm/v3/objects/contacts",
                        headers=headers, params=params,
                    )
                    if resp.status_code in (401, 403):
                        return {"success": False,
                                "error": f"HubSpot token invalid or expired (HTTP {resp.status_code})"}
                    if resp.status_code != 200:
                        return {"success": False,
                                "error": f"HTTP {resp.status_code}: {resp.text[:160]}"}
                    data = resp.json()
                    contacts.extend(_row(o) for o in data.get("results", []))
                    after = ((data.get("paging") or {}).get("next") or {}).get("after")
                    if not after:
                        break
        return {"success": True, "contacts": contacts[:limit]}
    except Exception as e:
        logger.error(f"HubSpot import fetch failed: {e}")
        return {"success": False, "error": str(e)[:200]}


async def get_contacts(limit: int = 20, workspace_id: Optional[str] = None) -> List[Dict]:
    """Fetch recent contacts from HubSpot (per-workspace token)."""
    token = _get_token(workspace_id)
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
