"""
HTTP action column — call an arbitrary HTTP API per row and extract a value.

Ported from eliasstravik/rowbound (action type "http"). Part of the action-column
engine (see docs/research/clay-alternatives-ingestion-catalog.md, Phase 4). Reuses the
existing primitives:
  - {column} templating (same resolver as AI/research/output columns)
  - core.url_guard.check_url (SSRF guard — mandatory for user-supplied URLs)
  - declarative.template.project_value (JSONPath-lite response extraction)
  - validate.validate_field (output accept-gate)

Column config fields (on ColumnConfig):
  http_url       : URL template, e.g. "https://api.x.com/find?domain={website}"
  http_method    : GET | POST | PUT (default GET)
  http_headers   : {name: template}
  http_body      : dict/string body template (for POST/PUT)
  http_extract   : JSONPath-lite expr for the cell value, e.g. "$.data.email"
                   (omit → store the raw text response, truncated)
"""

import json
import logging
from typing import Any, Dict

import httpx

from apps.api.services.workbook.ai_column import _resolve_prompt, _get_row_values
from apps.api.core.url_guard import check_url, BlockedUrlError
from apps.api.services.leadgen.enrichment.declarative.template import project_value

logger = logging.getLogger("workbook.http_column")

_MAX_RAW = 2000


def _resolve_any(value: Any, row_values: Dict[str, str]) -> Any:
    """Recursively resolve {column} placeholders in strings within a structure."""
    if isinstance(value, str):
        return _resolve_prompt(value, row_values)
    if isinstance(value, dict):
        return {k: _resolve_any(v, row_values) for k, v in value.items()}
    if isinstance(value, list):
        return [_resolve_any(v, row_values) for v in value]
    return value


async def execute_http_column(
    col_config: dict,
    lead_data: dict,
    columns_config: list,
) -> Dict[str, Any]:
    """Run one HTTP action cell. Returns {success, value, error}."""
    url_tmpl = (col_config.get("http_url") or "").strip()
    if not url_tmpl:
        return {"success": False, "value": None, "error": "no_url"}

    cells = {k: {"value": v} for k, v in lead_data.items()}
    row_values = _get_row_values(cells, columns_config)

    url = _resolve_prompt(url_tmpl, row_values)
    if "[" in url and ": not found]" in url:
        return {"success": False, "value": None, "error": "unresolved_url_placeholder"}

    # SSRF guard — never let a row value point us at a private/metadata host.
    # resolve=True: also resolve the host and reject if it maps to a private/
    # metadata IP (closes most of the DNS-rebinding gap).
    try:
        check_url(url, allow_http=True, resolve=True)
    except BlockedUrlError as e:
        return {"success": False, "value": None, "error": f"blocked_url: {str(e)[:60]}"}

    method = (col_config.get("http_method") or "GET").upper()
    headers = {k: _resolve_prompt(str(v), row_values) for k, v in (col_config.get("http_headers") or {}).items()}
    body = None
    if method in ("POST", "PUT", "PATCH") and col_config.get("http_body") is not None:
        body = _resolve_any(col_config["http_body"], row_values)

    try:
        # API action columns do not need browser-style redirect following.  A
        # redirect can change an already-approved public URL into a private or
        # metadata target, so fail closed and let the cell surface the 3xx.
        async with httpx.AsyncClient(timeout=20.0, follow_redirects=False) as client:
            resp = await client.request(
                method, url,
                headers=headers or None,
                json=body if isinstance(body, (dict, list)) else None,
                content=body if isinstance(body, str) else None,
            )
    except httpx.TimeoutException:
        return {"success": False, "value": None, "error": "timeout"}
    except Exception as e:
        return {"success": False, "value": None, "error": str(e)[:120]}

    if not 200 <= resp.status_code < 300:
        return {"success": False, "value": None, "error": f"http_{resp.status_code}"}

    extract = (col_config.get("http_extract") or "").strip()
    if not extract:
        # no extractor → store the raw text (truncated)
        text = resp.text[:_MAX_RAW]
        return {"success": bool(text), "value": text or None, "error": None if text else "empty"}

    try:
        data = resp.json()
    except Exception:
        return {"success": False, "value": None, "error": "non_json_response"}

    value = project_value(data, extract)
    if value is None or value == "":
        return {"success": False, "value": None, "error": "no_match"}
    # keep cells scalar
    if isinstance(value, (dict, list)):
        value = json.dumps(value, default=str)[:_MAX_RAW]
    return {"success": True, "value": value, "error": None}
