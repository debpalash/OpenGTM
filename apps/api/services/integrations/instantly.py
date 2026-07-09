"""
Instantly destination — add a workbook row as a lead in an Instantly campaign.

BYOK: an Instantly API v2 key (Bearer). Plain REST, no SDK.
Docs: https://developer.instantly.ai/api-reference/lead/create-lead.md
  POST https://api.instantly.ai/api/v2/leads
  Authorization: Bearer <key>
  body: {campaign, email, first_name, last_name, company_name,
         personalization, custom_variables, skip_if_in_campaign, ...}

The key is resolved per-workspace via workspace secrets (spec WI-6), falling
back to the global settings DB / environment (same pattern as HubSpot).
"""

import logging
import os
from typing import Any, Dict, Optional

import httpx

logger = logging.getLogger("integrations.instantly")

API_URL = "https://api.instantly.ai/api/v2/leads"

# Body fields Instantly accepts at the top level; anything else the caller
# maps goes into `custom_variables` (values must be primitives).
TOP_LEVEL_FIELDS = {
    "email", "first_name", "last_name", "company_name",
    "personalization", "website", "phone", "job_title",
}


def _api_key(workspace_id: Optional[str] = None) -> str:
    try:
        # get_secret() already does per-workspace → global settings → env.
        from apps.api.services.workspace.secrets import get_secret
        return get_secret(workspace_id, "INSTANTLY_API_KEY", "")
    except Exception:
        return os.getenv("INSTANTLY_API_KEY", "")


def is_connected(workspace_id: Optional[str] = None) -> bool:
    return bool(_api_key(workspace_id))


def _looks_like_duplicate(text: str) -> bool:
    t = (text or "").lower()
    return "already exists" in t or "already in" in t or "duplicate" in t


async def add_lead_to_campaign(
    campaign_id: str,
    lead: Dict[str, Any],
    skip_if_in_campaign: bool = True,
    workspace_id: Optional[str] = None,
) -> Dict[str, Any]:
    """Create one lead in the given Instantly campaign.

    `lead` maps Instantly field names (email, first_name, …) to values;
    unknown keys are sent as custom_variables. Returns
    {"success": bool, "lead_id"?: str, "duplicate"?: bool, "error"?: str}
    and never raises.
    """
    key = _api_key(workspace_id)
    if not key:
        return {"success": False, "error": "Instantly not connected (set INSTANTLY_API_KEY)"}
    if not campaign_id:
        return {"success": False, "error": "Instantly campaign_id is required"}
    email = (lead.get("email") or "").strip()
    if not email:
        return {"success": False, "error": "no_email"}

    body: Dict[str, Any] = {"campaign": campaign_id, "skip_if_in_campaign": bool(skip_if_in_campaign)}
    custom_vars: Dict[str, Any] = dict(lead.get("custom_variables") or {})
    for k, v in lead.items():
        if k == "custom_variables" or v in (None, "", []):
            continue
        if k in TOP_LEVEL_FIELDS:
            body[k] = v
        else:
            custom_vars[k] = v
    if custom_vars:
        body["custom_variables"] = custom_vars

    headers = {"Authorization": f"Bearer {key}", "Content-Type": "application/json"}
    try:
        async with httpx.AsyncClient(timeout=15) as client:
            resp = await client.post(API_URL, headers=headers, json=body)
        if 200 <= resp.status_code < 300:
            try:
                data = resp.json()
            except Exception:
                data = {}
            # Lead status -3 = Skipped (e.g. skip_if_in_campaign hit).
            if isinstance(data, dict) and data.get("status") == -3:
                return {"success": True, "lead_id": data.get("id"), "duplicate": True}
            return {"success": True, "lead_id": (data or {}).get("id"), "duplicate": False}
        # Idempotent-friendly: a "lead already exists/in campaign" rejection is
        # treated as success — the desired end state (lead in campaign) holds.
        if _looks_like_duplicate(resp.text):
            return {"success": True, "duplicate": True}
        if resp.status_code == 401:
            return {"success": False, "error": "Instantly auth failed (invalid INSTANTLY_API_KEY)"}
        return {"success": False, "error": f"HTTP {resp.status_code}: {resp.text[:160]}"}
    except Exception as e:
        logger.error(f"Instantly push failed: {e}")
        return {"success": False, "error": str(e)[:200]}
