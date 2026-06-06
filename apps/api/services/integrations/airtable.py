"""
Airtable destination — append a workbook row as an Airtable record.

BYOK: a Personal Access Token (PAT) + base id + table name/id. Plain REST, no
SDK. Docs: https://airtable.com/developers/web/api/create-records
"""

import os
import logging
from typing import Dict, Any, Optional

import httpx

logger = logging.getLogger("integrations.airtable")


def _token() -> str:
    try:
        from apps.api.routers.settings import _db_get
        return _db_get("AIRTABLE_TOKEN", "") or os.getenv("AIRTABLE_TOKEN", "")
    except Exception:
        return os.getenv("AIRTABLE_TOKEN", "")


def is_connected() -> bool:
    return bool(_token())


async def push_record(fields: Dict[str, Any], base_id: str, table: str,
                      typecast: bool = True) -> Dict[str, Any]:
    """Create one record in the given base/table. `fields` maps Airtable column
    names to values (must already exist in the table)."""
    token = _token()
    if not token:
        return {"success": False, "error": "Airtable not connected (set AIRTABLE_TOKEN)"}
    if not base_id or not table:
        return {"success": False, "error": "Airtable base_id and table are required"}

    # Drop empty values so we don't clobber Airtable cells with blanks.
    clean = {k: v for k, v in fields.items() if v not in (None, "", [])}
    url = f"https://api.airtable.com/v0/{base_id}/{table}"
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
