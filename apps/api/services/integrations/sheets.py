"""
Google Sheets destination — append a workbook row to a spreadsheet.

BYOK, dependency-light: uses the Sheets REST API directly via httpx with an
OAuth2 bearer access token (no google client libs). The token can be a user
OAuth token or one minted from a service account; either way it goes in settings
as GOOGLE_SHEETS_TOKEN. Append docs:
https://developers.google.com/sheets/api/reference/rest/v4/spreadsheets.values/append
"""

import os
import logging
from typing import Dict, Any, List

import httpx

logger = logging.getLogger("integrations.sheets")


def _token() -> str:
    try:
        from apps.api.routers.settings import _db_get
        return _db_get("GOOGLE_SHEETS_TOKEN", "") or os.getenv("GOOGLE_SHEETS_TOKEN", "")
    except Exception:
        return os.getenv("GOOGLE_SHEETS_TOKEN", "")


def is_connected() -> bool:
    return bool(_token())


async def append_row(spreadsheet_id: str, values: List[Any],
                     sheet_range: str = "Sheet1") -> Dict[str, Any]:
    """Append a single row (list of cell values) to the given spreadsheet."""
    token = _token()
    if not token:
        return {"success": False, "error": "Google Sheets not connected (set GOOGLE_SHEETS_TOKEN — an OAuth2 access token)"}
    if not spreadsheet_id:
        return {"success": False, "error": "spreadsheet_id is required"}

    url = (
        f"https://sheets.googleapis.com/v4/spreadsheets/{spreadsheet_id}"
        f"/values/{sheet_range}:append"
    )
    headers = {"Authorization": f"Bearer {token}", "Content-Type": "application/json"}
    params = {"valueInputOption": "USER_ENTERED", "insertDataOption": "INSERT_ROWS"}
    body = {"values": [[("" if v is None else str(v)) for v in values]]}
    try:
        async with httpx.AsyncClient(timeout=15) as client:
            resp = await client.post(url, headers=headers, params=params, json=body)
        if resp.status_code == 200:
            updated = resp.json().get("updates", {}).get("updatedRange", "")
            return {"success": True, "range": updated}
        return {"success": False, "error": f"HTTP {resp.status_code}: {resp.text[:160]}"}
    except Exception as e:
        logger.error(f"Sheets append failed: {e}")
        return {"success": False, "error": str(e)}
