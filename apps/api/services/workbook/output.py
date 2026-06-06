"""
Workbook Output Columns — push an enriched row to an external destination.

This is the "enrich → push" half of the Clay loop. An `output` column doesn't
compute a value; it sends the row somewhere and records the outcome in the cell.

Destinations (col_config["destination"]):
  - "webhook"   → templated HTTP request to a user URL
  - "crm"       → push the row as a contact (HubSpot today; Salesforce later)
  - "sequencer" → enroll the lead into an email sequence

Output columns are side-effecting, so the engine treats them as run-once by
default (see enrich_cell's run_once guard).

NOTE: integration credentials (HubSpot token, SMTP) are read from GLOBAL
settings for now — per-workspace credentials are deferred (spec WI-6) until
multi-tenancy matures. The `workspace_id` arg is threaded through so that
becomes a localized change later.
"""

import ipaddress
import json
import logging
import socket
from typing import Any, Dict, Optional
from urllib.parse import urlparse

import httpx

logger = logging.getLogger("workbook.output")


# ── Minimal SSRF guard (webhook destinations) ─────────────────────────────
# Full allowlisting across all scrapers is spec WI-4; this is the localized
# guard for the new outbound-webhook vector so we don't ship a fresh hole.

def _is_safe_public_url(url: str) -> tuple[bool, str]:
    """Return (ok, reason). Blocks non-http(s), credentials, and any host that
    resolves to a loopback/private/link-local/reserved address (incl. the cloud
    metadata IP 169.254.169.254)."""
    try:
        parsed = urlparse(url)
    except Exception as e:
        return False, f"unparseable url: {e}"
    if parsed.scheme not in ("http", "https"):
        return False, "only http/https allowed"
    if parsed.username or parsed.password:
        return False, "credentials in url not allowed"
    host = parsed.hostname
    if not host:
        return False, "missing host"
    try:
        infos = socket.getaddrinfo(host, parsed.port or (443 if parsed.scheme == "https" else 80))
    except socket.gaierror as e:
        return False, f"dns resolution failed: {e}"
    for info in infos:
        ip_str = info[4][0]
        try:
            ip = ipaddress.ip_address(ip_str)
        except ValueError:
            continue
        if (ip.is_loopback or ip.is_private or ip.is_link_local
                or ip.is_reserved or ip.is_multicast or ip.is_unspecified):
            return False, f"host resolves to non-public address {ip_str}"
    return True, ""


# ── Template resolution ───────────────────────────────────────────────────

def _resolve(template: str, lead_data: dict, columns_config: list) -> str:
    """Resolve {column}/{field} placeholders in a string using the row's values.
    Reuses the same resolver as AI columns. Lazy imports avoid a circular import
    (enrichment imports this module at load time)."""
    from apps.api.services.workbook.ai_column import _resolve_prompt
    from apps.api.services.workbook.enrichment import _get_lead_values

    values = _get_lead_values(lead_data, columns_config)
    return _resolve_prompt(template, values)


def _resolve_deep(value: Any, lead_data: dict, columns_config: list) -> Any:
    """Resolve {placeholders} inside the string values of a dict/list/str.
    Used for JSON-object webhook bodies so the {...} syntax never collides with
    the structural braces of a JSON *string* template."""
    if isinstance(value, str):
        return _resolve(value, lead_data, columns_config)
    if isinstance(value, dict):
        return {k: _resolve_deep(v, lead_data, columns_config) for k, v in value.items()}
    if isinstance(value, list):
        return [_resolve_deep(v, lead_data, columns_config) for v in value]
    return value


# ── Destination handlers ──────────────────────────────────────────────────

async def _send_webhook(cfg: dict, lead_data: dict, columns_config: list) -> Dict[str, Any]:
    raw_url = (cfg.get("url") or "").strip()
    if not raw_url:
        return {"success": False, "value": "", "error": "webhook url not configured"}
    url = _resolve(raw_url, lead_data, columns_config)

    ok, reason = _is_safe_public_url(url)
    if not ok:
        return {"success": False, "value": "", "error": f"blocked url: {reason}"}

    method = (cfg.get("method") or "POST").upper()
    headers = {k: _resolve(str(v), lead_data, columns_config) for k, v in (cfg.get("headers") or {}).items()}

    # Body: resolve template, send as JSON when it parses, else raw text.
    # Default body is the full row as JSON.
    kwargs: Dict[str, Any] = {"headers": headers}
    if method in ("POST", "PUT", "PATCH"):
        raw_body = cfg.get("body")
        if raw_body is None:
            # Default: the full row as JSON.
            kwargs["json"] = lead_data
        elif isinstance(raw_body, (dict, list)):
            # Canonical, brace-safe form: a JSON object whose string values may
            # contain {placeholders}.
            kwargs["json"] = _resolve_deep(raw_body, lead_data, columns_config)
        else:
            # String template → resolve, then send as JSON if it parses, else text.
            resolved = _resolve(str(raw_body), lead_data, columns_config)
            try:
                kwargs["json"] = json.loads(resolved)
            except (json.JSONDecodeError, TypeError):
                kwargs["content"] = resolved
                headers.setdefault("Content-Type", "text/plain")

    try:
        async with httpx.AsyncClient(timeout=15, follow_redirects=False) as client:
            resp = await client.request(method, url, **kwargs)
        ok_status = 200 <= resp.status_code < 300
        return {
            "success": ok_status,
            "value": f"{method} {resp.status_code}",
            "error": None if ok_status else f"HTTP {resp.status_code}: {resp.text[:160]}",
        }
    except Exception as e:
        return {"success": False, "value": "", "error": str(e)[:200]}


async def _push_crm(cfg: dict, lead_data: dict, workspace_id: Optional[str]) -> Dict[str, Any]:
    crm_type = (cfg.get("type") or "hubspot").lower()
    if crm_type == "hubspot":
        from apps.api.services.crm import hubspot as crm
        label, id_key = "HubSpot", "hubspot_id"
    elif crm_type == "salesforce":
        from apps.api.services.crm import salesforce as crm
        label, id_key = "Salesforce", "salesforce_id"
    else:
        return {"success": False, "value": "", "error": f"unsupported crm '{crm_type}'"}

    if not crm.is_connected():
        return {"success": False, "value": "", "error": f"{label} not connected"}

    # push_lead_as_contact reads attributes off a Lead object.
    from apps.api.services.workbook.enrichment import _lead_dict_to_lead
    lead = _lead_dict_to_lead(lead_data)

    res = await crm.push_lead_as_contact(lead, cfg.get("field_map"))
    if res.get("success"):
        return {
            "success": True,
            "value": f"{label}: {res.get('action', 'synced')} {res.get(id_key, '')}".strip(),
            "error": None,
        }
    return {"success": False, "value": "", "error": res.get("error", f"{label} push failed")}


async def _push_airtable(cfg: dict, lead_data: dict) -> Dict[str, Any]:
    from apps.api.services.integrations.airtable import push_record
    fmap = cfg.get("field_map") or {
        "company": "Company", "email": "Email", "phone": "Phone",
        "website": "Website", "city": "City",
    }
    fields = {col: lead_data.get(lf) for lf, col in fmap.items()}
    res = await push_record(fields, cfg.get("base_id", ""), cfg.get("table", ""))
    if res.get("success"):
        return {"success": True, "value": f"Airtable: {res.get('record_id', 'created')}", "error": None}
    return {"success": False, "value": "", "error": res.get("error", "airtable push failed")}


async def _push_sheets(cfg: dict, lead_data: dict) -> Dict[str, Any]:
    from apps.api.services.integrations.sheets import append_row
    columns = cfg.get("columns") or ["company", "email", "phone", "website", "city"]
    values = [lead_data.get(c, "") for c in columns]
    res = await append_row(cfg.get("spreadsheet_id", ""), values, cfg.get("range", "Sheet1"))
    if res.get("success"):
        return {"success": True, "value": f"Sheets: {res.get('range', 'appended')}", "error": None}
    return {"success": False, "value": "", "error": res.get("error", "sheets append failed")}


def _enroll_sequence(cfg: dict, lead_id: int) -> Dict[str, Any]:
    seq_id = cfg.get("sequence_id")
    if not seq_id:
        return {"success": False, "value": "", "error": "sequence_id not configured"}
    from apps.api.services.outreach.sequence import enroll_leads
    try:
        n = enroll_leads(seq_id, [lead_id])
        return {"success": n > 0, "value": "enrolled" if n > 0 else "already enrolled",
                "error": None if n > 0 else None}
    except Exception as e:
        return {"success": False, "value": "", "error": str(e)[:200]}


# ── Entry point (called from enrich_cell) ─────────────────────────────────

async def execute_output_column(
    col_config: dict,
    lead_data: dict,
    columns_config: list,
    workbook_id: str,
    lead_id: int,
    workspace_id: Optional[str] = None,
) -> Dict[str, Any]:
    """Push the row to its configured destination.

    Returns {"success": bool, "value": <cell summary str>, "error": str|None}.
    """
    dest = (col_config.get("destination") or "").lower()
    cfg = col_config.get("destination_config") or {}

    if dest == "webhook":
        return await _send_webhook(cfg, lead_data, columns_config)
    if dest == "crm":
        return await _push_crm(cfg, lead_data, workspace_id)
    if dest == "sequencer":
        return _enroll_sequence(cfg, lead_id)
    if dest == "airtable":
        return await _push_airtable(cfg, lead_data)
    if dest == "sheets":
        return await _push_sheets(cfg, lead_data)
    return {"success": False, "value": "", "error": f"unknown destination '{dest}'"}
