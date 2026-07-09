"""
Smartlead destination — add a workbook row as a lead in a Smartlead campaign.

BYOK: a Smartlead API key (query param). Plain REST, no SDK.
Docs: https://api.smartlead.ai/api-reference/campaigns/add-leads
  POST https://server.smartlead.ai/api/v1/campaigns/{campaign_id}/leads?api_key=KEY
  body: {lead_list: [{email, first_name, last_name, company_name,
                      custom_fields, ...}], settings: {...}}

The key is resolved per-workspace via workspace secrets (spec WI-6), falling
back to the global settings DB / environment (same pattern as HubSpot).
"""

import logging
import os
from typing import Any, Dict, Optional

import httpx

logger = logging.getLogger("integrations.smartlead")

BASE_URL = "https://server.smartlead.ai/api/v1"

# Fields Smartlead accepts on a lead_list entry; anything else the caller
# maps goes into `custom_fields`.
TOP_LEVEL_FIELDS = {
    "email", "first_name", "last_name", "company_name", "phone_number",
    "website", "location", "linkedin_profile", "company_url",
}


def _api_key(workspace_id: Optional[str] = None) -> str:
    try:
        # get_secret() already does per-workspace → global settings → env.
        from apps.api.services.workspace.secrets import get_secret
        return get_secret(workspace_id, "SMARTLEAD_API_KEY", "")
    except Exception:
        return os.getenv("SMARTLEAD_API_KEY", "")


def is_connected(workspace_id: Optional[str] = None) -> bool:
    return bool(_api_key(workspace_id))


def _duplicate_count(data: Dict[str, Any]) -> int:
    total = 0
    for field in ("already_added_to_campaign", "duplicate_count", "skipped_count"):
        v = data.get(field)
        if isinstance(v, (int, float)):
            total += int(v)
    return total


async def add_lead_to_campaign(
    campaign_id: str,
    lead: Dict[str, Any],
    settings: Optional[Dict[str, Any]] = None,
    workspace_id: Optional[str] = None,
) -> Dict[str, Any]:
    """Add one lead to the given Smartlead campaign.

    `lead` maps Smartlead field names (email, first_name, …) to values;
    unknown keys are sent as custom_fields. Returns
    {"success": bool, "duplicate"?: bool, "error"?: str} and never raises.
    """
    key = _api_key(workspace_id)
    if not key:
        return {"success": False, "error": "Smartlead not connected (set SMARTLEAD_API_KEY)"}
    if not campaign_id:
        return {"success": False, "error": "Smartlead campaign_id is required"}
    email = (lead.get("email") or "").strip()
    if not email:
        return {"success": False, "error": "no_email"}

    entry: Dict[str, Any] = {}
    custom_fields: Dict[str, Any] = dict(lead.get("custom_fields") or {})
    for k, v in lead.items():
        if k == "custom_fields" or v in (None, "", []):
            continue
        if k in TOP_LEVEL_FIELDS:
            entry[k] = v
        else:
            custom_fields[k] = v
    if custom_fields:
        entry["custom_fields"] = custom_fields

    body: Dict[str, Any] = {"lead_list": [entry]}
    if settings:
        body["settings"] = settings

    url = f"{BASE_URL}/campaigns/{campaign_id}/leads"
    try:
        async with httpx.AsyncClient(timeout=15) as client:
            resp = await client.post(url, params={"api_key": key}, json=body)
        if 200 <= resp.status_code < 300:
            try:
                data = resp.json() or {}
            except Exception:
                data = {}
            if not isinstance(data, dict):
                data = {}
            # Smartlead reports duplicates inside a 200 body via counts
            # (already_added_to_campaign / duplicate_count / skipped_count).
            if _duplicate_count(data) > 0:
                return {"success": True, "duplicate": True}
            if data.get("ok") is False or data.get("success") is False:
                msg = str(data.get("message") or data.get("error") or "smartlead rejected lead")
                if "already" in msg.lower() or "duplicate" in msg.lower():
                    return {"success": True, "duplicate": True}
                return {"success": False, "error": msg[:200]}
            return {"success": True, "duplicate": False}
        # Idempotent-friendly: an "already in campaign" rejection is success —
        # the desired end state (lead in campaign) holds.
        txt = resp.text or ""
        if "already" in txt.lower() or "duplicate" in txt.lower():
            return {"success": True, "duplicate": True}
        if resp.status_code == 401:
            return {"success": False, "error": "Smartlead auth failed (invalid SMARTLEAD_API_KEY)"}
        return {"success": False, "error": f"HTTP {resp.status_code}: {txt[:160]}"}
    except Exception as e:
        logger.error(f"Smartlead push failed: {e}")
        return {"success": False, "error": str(e)[:200]}
