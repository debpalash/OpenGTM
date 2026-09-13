"""
Google Sheets destination — append a workbook row to a spreadsheet.

BYOK, dependency-light: uses the Sheets REST API directly via httpx with an
OAuth2 bearer access token (no google client libs). The token can be a user
OAuth token or one minted from a service account; either way it goes in settings
as GOOGLE_SHEETS_TOKEN. Append docs:
https://developers.google.com/sheets/api/reference/rest/v4/spreadsheets.values/append
"""

import logging
import os
from typing import Any, Dict, List, Optional
from urllib.parse import quote

import httpx

logger = logging.getLogger("integrations.sheets")


def _token(workspace_id: Optional[str] = None) -> str:
    try:
        from apps.api.services.workspace.secrets import get_secret

        return get_secret(workspace_id, "GOOGLE_SHEETS_TOKEN", "")
    except Exception:
        return os.getenv("GOOGLE_SHEETS_TOKEN", "")


def is_connected(workspace_id: Optional[str] = None) -> bool:
    return bool(_token(workspace_id))


async def append_row(spreadsheet_id: str, values: List[Any],
                     sheet_range: str = "Sheet1",
                     workspace_id: Optional[str] = None) -> Dict[str, Any]:
    """Append a single row (list of cell values) to the given spreadsheet."""
    token = _token(workspace_id)
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


def _column_name(number: int) -> str:
    result = ""
    while number:
        number, remainder = divmod(number - 1, 26)
        result = chr(65 + remainder) + result
    return result


async def upsert_row(
    spreadsheet_id: str,
    values: List[Any],
    idempotency_key: str,
    sheet_range: str = "Sheet1!A:ZZ",
    workspace_id: Optional[str] = None,
) -> Dict[str, Any]:
    """Upsert a row whose first cell is OpenGTM's stable delivery key."""
    token = _token(workspace_id)
    if not token:
        return {"success": False, "error": "Google Sheets not connected"}
    if not spreadsheet_id or not idempotency_key:
        return {"success": False, "error": "spreadsheet_id and idempotency_key are required"}
    row = [idempotency_key, *("" if value is None else str(value) for value in values)]
    base = f"https://sheets.googleapis.com/v4/spreadsheets/{spreadsheet_id}/values"
    headers = {"Authorization": f"Bearer {token}", "Content-Type": "application/json"}
    try:
        async with httpx.AsyncClient(timeout=15) as client:
            lookup = await client.get(
                f"{base}/{quote(sheet_range, safe='')}",
                headers=headers,
                params={"majorDimension": "ROWS"},
            )
            if lookup.status_code != 200:
                return {"success": False, "error": f"HTTP {lookup.status_code}: {lookup.text[:160]}"}
            rows = lookup.json().get("values") or []
            existing_row = next(
                (index for index, item in enumerate(rows, start=1) if item and item[0] == idempotency_key),
                None,
            )
            if existing_row:
                sheet = sheet_range.split("!", 1)[0]
                target = f"{sheet}!A{existing_row}:{_column_name(len(row))}{existing_row}"
                response = await client.put(
                    f"{base}/{quote(target, safe='')}",
                    headers=headers,
                    params={"valueInputOption": "USER_ENTERED"},
                    json={"values": [row]},
                )
                operation = "updated"
            else:
                response = await client.post(
                    f"{base}/{quote(sheet_range, safe='')}:append",
                    headers=headers,
                    params={"valueInputOption": "USER_ENTERED", "insertDataOption": "INSERT_ROWS"},
                    json={"values": [row]},
                )
                operation = "appended"
        if response.status_code != 200:
            return {"success": False, "error": f"HTTP {response.status_code}: {response.text[:160]}"}
        data = response.json()
        updated_range = data.get("updatedRange") or data.get("updates", {}).get("updatedRange", "")
        return {"success": True, "range": updated_range, "operation": operation}
    except Exception as exc:
        logger.error("Sheets upsert failed: %s", exc)
        return {"success": False, "error": str(exc)[:200]}
