"""
Airtable destination — append a workbook row as an Airtable record.

BYOK: a Personal Access Token (PAT) + base id + table name/id. Plain REST, no
SDK. Docs: https://airtable.com/developers/web/api/create-records
"""

import os
import logging
from typing import Dict, Any, Optional
from urllib.parse import quote

import httpx

logger = logging.getLogger("integrations.airtable")


def _token(workspace_id: Optional[str] = None) -> str:
    try:
        from apps.api.services.workspace.secrets import get_secret

        return get_secret(workspace_id, "AIRTABLE_TOKEN", "")
    except Exception:
        return os.getenv("AIRTABLE_TOKEN", "")


def is_connected(workspace_id: Optional[str] = None) -> bool:
    return bool(_token(workspace_id))


async def push_record(fields: Dict[str, Any], base_id: str, table: str,
                      typecast: bool = True,
                      workspace_id: Optional[str] = None) -> Dict[str, Any]:
    """Create one record in the given base/table. `fields` maps Airtable column
    names to values (must already exist in the table)."""
    token = _token(workspace_id)
    if not token:
        return {"success": False, "error": "Airtable not connected (set AIRTABLE_TOKEN)"}
    if not base_id or not table:
        return {"success": False, "error": "Airtable base_id and table are required"}

    # Drop empty values so we don't clobber Airtable cells with blanks.
    clean = {k: v for k, v in fields.items() if v not in (None, "", [])}
    url = f"https://api.airtable.com/v0/{quote(base_id, safe='')}/{quote(table, safe='')}"
    headers = {"Authorization": f"Bearer {token}", "Content-Type": "application/json"}
    try:
        async with httpx.AsyncClient(timeout=15) as client:
            resp = await client.post(url, headers=headers, json={"fields": clean, "typecast": typecast})
        if resp.status_code in (200, 201):
            return {"success": True, "record_id": resp.json().get("id")}
        return {"success": False, "error": f"HTTP {resp.status_code}: {resp.text[:160]}"}
    except Exception as e:
        logger.error(f"Airtable push failed: {e}")
        return {"success": False, "error": str(e)}


async def upsert_record(
    fields: Dict[str, Any],
    base_id: str,
    table: str,
    idempotency_key: str,
    idempotency_field: str = "OpenGTM ID",
    typecast: bool = True,
    workspace_id: Optional[str] = None,
) -> Dict[str, Any]:
    """Atomically create or update a record using Airtable performUpsert."""
    token = _token(workspace_id)
    if not token:
        return {"success": False, "error": "Airtable not connected"}
    if not base_id or not table or not idempotency_key or not idempotency_field:
        return {"success": False, "error": "Airtable upsert configuration is incomplete"}
    clean = {key: value for key, value in fields.items() if value not in (None, "", [])}
    clean[idempotency_field] = idempotency_key
    url = f"https://api.airtable.com/v0/{quote(base_id, safe='')}/{quote(table, safe='')}"
    body = {
        "performUpsert": {"fieldsToMergeOn": [idempotency_field]},
        "records": [{"fields": clean}],
        "typecast": typecast,
    }
    headers = {"Authorization": f"Bearer {token}", "Content-Type": "application/json"}
    try:
        async with httpx.AsyncClient(timeout=15) as client:
            response = await client.patch(url, headers=headers, json=body)
        if response.status_code in (200, 201):
            data = response.json()
            records = data.get("records") or []
            record_id = records[0].get("id") if records else None
            operation = "created" if data.get("createdRecords") else "updated"
            return {"success": True, "record_id": record_id, "operation": operation}
        return {"success": False, "error": f"HTTP {response.status_code}: {response.text[:160]}"}
    except Exception as exc:
        logger.error("Airtable upsert failed: %s", exc)
        return {"success": False, "error": str(exc)[:200]}
