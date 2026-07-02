"""
Salesforce CRM Client — push leads as Salesforce Lead records.

BYOK: requires an instance URL + OAuth2 access token (Connected App / session).
Mirrors the hubspot.py interface so the workbook output column can dispatch to
either CRM. Pushes to the Lead sObject (has Company, which our leads carry).
Docs: https://developer.salesforce.com/docs/api-explorer/sobject/Lead
"""

import os
import logging
from typing import Dict, Any, Optional

import httpx

logger = logging.getLogger("crm.salesforce")

API_VERSION = "v59.0"


def _soql_escape(value: str) -> str:
    """Escape a value for safe interpolation into a SOQL string literal.

    Salesforce SOQL has no bind-parameter API over the REST /query endpoint, so
    a raw f-string (``... WHERE Email = '{email}'``) is injectable: a value like
    ``x' OR Name != '`` would broaden or subvert the query. Per Salesforce's
    escaping rules we backslash-escape the reserved characters. Control chars are
    stripped outright (they can't appear in a real email and only serve to break
    out of the literal).
    """
    out = []
    for ch in str(value):
        if ch in ("\\", "'", '"'):
            out.append("\\" + ch)
        elif ch in ("\n", "\r", "\t"):
            continue
        else:
            out.append(ch)
    return "".join(out)


def _creds(workspace_id: Optional[str] = None) -> tuple[str, str]:
    """(instance_url, access_token).

    When ``workspace_id`` is given, resolve the per-workspace encrypted secrets
    first (spec WI-6), falling back to the global settings DB / environment.
    """
    try:
        from apps.api.services.workspace.secrets import get_secret
        inst = get_secret(workspace_id, "SALESFORCE_INSTANCE_URL", "")
        tok = get_secret(workspace_id, "SALESFORCE_ACCESS_TOKEN", "")
    except Exception:
        inst = os.getenv("SALESFORCE_INSTANCE_URL", "")
        tok = os.getenv("SALESFORCE_ACCESS_TOKEN", "")
    return inst.rstrip("/"), tok


def is_connected(workspace_id: Optional[str] = None) -> bool:
    inst, tok = _creds(workspace_id)
    return bool(inst and tok)


# Yupcha lead field -> Salesforce Lead field
DEFAULT_FIELD_MAP = {
    "company": "Company",
    "email": "Email",
    "phone": "Phone",
    "website": "Website",
    "city": "City",
    "contact_title": "Title",
}


async def push_lead_as_contact(lead, field_map: Dict[str, str] = None,
                               workspace_id: Optional[str] = None) -> Dict[str, Any]:
    """Push a single lead to Salesforce as a Lead record (upsert by email).

    ``workspace_id`` selects the per-workspace credentials when set (spec WI-6).
    """
    inst, token = _creds(workspace_id)
    if not (inst and token):
        return {"success": False, "error": "Salesforce not connected (set SALESFORCE_INSTANCE_URL + SALESFORCE_ACCESS_TOKEN)"}

    fmap = field_map or DEFAULT_FIELD_MAP
    fields: Dict[str, Any] = {}
    for lead_field, sf_field in fmap.items():
        value = getattr(lead, lead_field, None)
        if value:
            fields[sf_field] = str(value)

    # Salesforce Lead requires LastName + Company.
    contact = getattr(lead, "contact_person", None)
    if contact and " " in str(contact):
        first, last = str(contact).split(" ", 1)
        fields["FirstName"] = first
        fields["LastName"] = last
    else:
        fields["LastName"] = str(contact or fields.get("Company") or "Unknown")
    if not fields.get("Company"):
        return {"success": False, "error": "Lead has no company — required for Salesforce Lead"}

    headers = {"Authorization": f"Bearer {token}", "Content-Type": "application/json"}
    base = f"{inst}/services/data/{API_VERSION}"
    try:
        async with httpx.AsyncClient(timeout=15) as client:
            # Upsert by email when present (SOQL lookup → PATCH), else create.
            email = fields.get("Email")
            if email:
                q = f"SELECT Id FROM Lead WHERE Email = '{_soql_escape(email)}' LIMIT 1"
                sr = await client.get(f"{base}/query", headers=headers, params={"q": q})
                if sr.status_code == 200 and sr.json().get("records"):
                    sid = sr.json()["records"][0]["Id"]
                    ur = await client.patch(f"{base}/sobjects/Lead/{sid}", headers=headers, json=fields)
                    ok = ur.status_code in (200, 204)
                    return {"success": ok, "action": "updated", "salesforce_id": sid,
                            "error": None if ok else f"HTTP {ur.status_code}: {ur.text[:160]}"}

            cr = await client.post(f"{base}/sobjects/Lead", headers=headers, json=fields)
            if cr.status_code in (200, 201):
                return {"success": True, "action": "created", "salesforce_id": cr.json().get("id")}
            return {"success": False, "error": f"HTTP {cr.status_code}: {cr.text[:160]}"}
    except Exception as e:
        logger.error(f"Salesforce push failed: {e}")
        return {"success": False, "error": str(e)}
