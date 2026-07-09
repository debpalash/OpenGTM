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


# Salesforce Contact field -> Yupcha lead field (import default; FirstName/
# LastName merge into contact_person, the inverse of the push-side split).
IMPORT_DEFAULT_FIELDS = {
    "Email": "email",
    "Account.Name": "company",
    "Phone": "phone",
    "Title": "contact_title",
}

# Update write-back default (Contact sObject): Company/Account.Name is not a
# writable Contact field, so it is deliberately absent here.
CONTACT_UPDATE_FIELD_MAP = {
    "email": "Email",
    "phone": "Phone",
    "contact_title": "Title",
}


def _record_value(record: Dict[str, Any], path: str) -> Any:
    """Resolve a (possibly dotted) SOQL field path against a query record."""
    cur: Any = record
    for part in path.split("."):
        if not isinstance(cur, dict):
            return None
        cur = cur.get(part)
    return cur


def _map_import_record(record: Dict[str, Any],
                       field_map: Optional[Dict[str, str]]) -> Dict[str, Any]:
    """Map a Salesforce Contact query record → Yupcha lead fields."""
    if field_map:
        out = {
            lead_field: _record_value(record, sf_field)
            for sf_field, lead_field in field_map.items()
            if _record_value(record, sf_field) not in (None, "")
        }
    else:
        out = {
            lead_field: _record_value(record, sf_field)
            for sf_field, lead_field in IMPORT_DEFAULT_FIELDS.items()
            if _record_value(record, sf_field) not in (None, "")
        }
        name = " ".join(
            str(record.get(p) or "").strip() for p in ("FirstName", "LastName")
        ).strip()
        if name:
            out["contact_person"] = name
    return out


async def fetch_contacts(limit: int = 500, where: Optional[str] = None,
                         field_map: Optional[Dict[str, str]] = None,
                         workspace_id: Optional[str] = None) -> Dict[str, Any]:
    """Pull up to ``limit`` Contacts from Salesforce (REST query + paging).

    ``where`` is an optional raw SOQL WHERE fragment supplied by the workbook
    owner — it runs under THEIR OWN Salesforce token, so it carries no more
    authority than the owner already has. Pagination follows nextRecordsUrl.
    Returns {"success": True, "contacts": [...]} with the sync key stamped on
    each row, or a clean {"success": False, "error": ...}. Never raises for
    HTTP failures.
    """
    inst, token = _creds(workspace_id)
    if not (inst and token):
        return {"success": False,
                "error": "Salesforce not connected (set SALESFORCE_INSTANCE_URL + SALESFORCE_ACCESS_TOKEN)"}

    from apps.api.services.crm import request_with_retry
    if field_map:
        select_fields = [f for f in field_map.keys() if f.lower() != "id"]
    else:
        select_fields = ["Email", "FirstName", "LastName", "Account.Name", "Phone", "Title"]
    soql = f"SELECT Id, {', '.join(select_fields)} FROM Contact"
    if where and str(where).strip():
        soql += f" WHERE {str(where).strip()}"
    soql += f" LIMIT {int(limit)}"

    headers = {"Authorization": f"Bearer {token}"}
    base = f"{inst}/services/data/{API_VERSION}"
    contacts: list[Dict[str, Any]] = []
    try:
        async with httpx.AsyncClient(timeout=20) as client:
            resp = await request_with_retry(
                client, "GET", f"{base}/query", headers=headers, params={"q": soql},
            )
            while True:
                if resp.status_code == 401:
                    return {"success": False,
                            "error": "Salesforce token invalid or expired (HTTP 401)"}
                if resp.status_code != 200:
                    return {"success": False,
                            "error": f"HTTP {resp.status_code}: {resp.text[:160]}"}
                data = resp.json()
                for record in data.get("records", []):
                    row = _map_import_record(record, field_map)
                    row["crm_external_id"] = str(record.get("Id"))
                    row["crm_type"] = "salesforce"
                    row["crm_object"] = "contact"
                    contacts.append(row)
                    if len(contacts) >= limit:
                        break
                next_url = data.get("nextRecordsUrl")
                if data.get("done") or not next_url or len(contacts) >= limit:
                    break
                resp = await request_with_retry(
                    client, "GET", f"{inst}{next_url}", headers=headers,
                )
        return {"success": True, "contacts": contacts[:limit]}
    except Exception as e:
        logger.error(f"Salesforce import fetch failed: {e}")
        return {"success": False, "error": str(e)[:200]}


async def update_record(sobject: str, record_id: str, fields: Dict[str, Any],
                        workspace_id: Optional[str] = None) -> Dict[str, Any]:
    """PATCH an existing Salesforce record by Id (CRM-sync write-back).

    Returns {"success": True, "action": "updated", "salesforce_id": id} on
    200/204, {"success": False, "not_found": True, ...} on 404 (deleted in
    Salesforce — caller falls back to create), a clean error dict otherwise.
    429 is retried (bounded, Retry-After honored). Never raises for HTTP
    failures.
    """
    inst, token = _creds(workspace_id)
    if not (inst and token):
        return {"success": False,
                "error": "Salesforce not connected (set SALESFORCE_INSTANCE_URL + SALESFORCE_ACCESS_TOKEN)"}
    if not fields:
        return {"success": False, "error": "no mapped fields to update"}

    from apps.api.services.crm import request_with_retry
    headers = {"Authorization": f"Bearer {token}", "Content-Type": "application/json"}
    try:
        async with httpx.AsyncClient(timeout=15) as client:
            resp = await request_with_retry(
                client, "PATCH",
                f"{inst}/services/data/{API_VERSION}/sobjects/{sobject}/{record_id}",
                headers=headers, json=fields,
            )
        if resp.status_code in (200, 204):
            return {"success": True, "action": "updated", "salesforce_id": str(record_id)}
        if resp.status_code == 404:
            return {"success": False, "not_found": True,
                    "error": f"{sobject} {record_id} not found in Salesforce"}
        return {"success": False,
                "error": f"HTTP {resp.status_code}: {resp.text[:160]}"}
    except Exception as e:
        logger.error(f"Salesforce update failed: {e}")
        return {"success": False, "error": str(e)[:200]}


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
