"""
Workbook Output Columns — push an enriched row to an external destination.

This is the "enrich → push" half of the Clay loop. An `output` column doesn't
compute a value; it sends the row somewhere and records the outcome in the cell.

Destinations (col_config["destination"]):
  - "webhook"   → templated HTTP request to a user URL
  - "crm"       → push the row as a contact (HubSpot today; Salesforce later)
  - "sequencer" → enroll the lead into an email sequence
  - "instantly" → add the lead to an Instantly campaign (API v2)
  - "smartlead" → add the lead to a Smartlead campaign

Output columns are side-effecting, so the engine treats them as run-once by
default (see enrich_cell's run_once guard).

Integration credentials (HubSpot/Salesforce tokens, SMTP) are resolved
PER-WORKSPACE via services/workspace/secrets.get_secret(workspace_id, key)
(spec WI-6): the per-workspace encrypted secret is used when present, otherwise
we fall back to the existing GLOBAL setting so single-tenant installs are
unaffected. The `workspace_id` arg is threaded through to the CRM/SMTP clients.
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


def _revalidate_public_url(url: str) -> tuple[bool, str]:
    """Re-resolve a URL's host at connect time; fail closed on any private IP.

    DNS-rebinding (TOCTOU) mitigation for the native research `fetch`: the
    initial `_is_safe_public_url` check resolves DNS once, but a rebinding
    attacker can flip the A record to a private/metadata IP before the scraper
    resolves it again. The native fetch path calls this immediately before
    scraping; if the host now resolves to a non-public address the fetch is
    aborted. (Pinning the exact socket would need a custom transport in the
    shared scraper; re-validating at connect time closes the realistic window
    without forking the scraper.) Returns (ok, reason). Never raises.
    """
    try:
        return _is_safe_public_url(url)
    except Exception as e:  # defensive — never let the guard crash the fetch
        return False, f"revalidation error: {e}"


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


def _crm_update_fields(crm_type: str, cfg: dict, lead_data: dict) -> Dict[str, str]:
    """Build the CRM property/field payload for an update-by-external-id.

    Only mapped fields are sent: destination_config.field_map
    ({lead_field: crm_field}, same orientation as the push path) when set,
    else the CRM's default map. contact_person is split into first/last name
    fields, mirroring push_lead_as_contact.
    """
    if crm_type == "hubspot":
        from apps.api.services.crm.hubspot import DEFAULT_FIELD_MAP as default_map
        first_key, last_key = "firstname", "lastname"
    else:  # salesforce Contact
        from apps.api.services.crm.salesforce import CONTACT_UPDATE_FIELD_MAP as default_map
        first_key, last_key = "FirstName", "LastName"

    fmap = cfg.get("field_map") or default_map
    fields: Dict[str, str] = {}
    for lead_field, crm_field in fmap.items():
        value = lead_data.get(lead_field)
        if value in (None, ""):
            continue
        if lead_field == "contact_person" and " " in str(value):
            first, last = str(value).split(" ", 1)
            fields[first_key] = first
            fields[last_key] = last
        else:
            fields[crm_field] = str(value)
    return fields


def _store_row_crm_id(workbook_id: str, lead_id: int, crm_type: str,
                      old_ext_id: str, new_id: str, new_object: str) -> bool:
    """After a 404→create fallback, stamp the fresh CRM id onto the row.

    Best-effort: locate the WorkbookRow this cell ran for (crm_import rows have
    lead_id NULL, so enrichment passes their row id as lead_id) and confirm it
    carries the stale sync key before overwriting. Returns False (logged) when
    the row can't be identified — the push itself already succeeded.
    """
    try:
        from apps.api.database import SessionLocal
        from apps.api.services.workbook.models import WorkbookRow

        with SessionLocal() as db:
            candidates = db.query(WorkbookRow).filter(
                WorkbookRow.workbook_id == workbook_id,
                ((WorkbookRow.lead_id == lead_id) | (WorkbookRow.id == lead_id)),
            ).all()
            row = next(
                (r for r in candidates
                 if str((r.data or {}).get("crm_external_id") or "") == str(old_ext_id)
                 and str((r.data or {}).get("crm_type") or "").lower() == crm_type),
                None,
            )
            if row is None:
                return False
            data = dict(row.data or {})
            data["crm_external_id"] = str(new_id)
            data["crm_type"] = crm_type
            data["crm_object"] = new_object
            row.data = data
            db.commit()
            return True
    except Exception as e:
        logger.warning(f"Failed to store new {crm_type} id onto row {lead_id}: {e}")
        return False


async def _push_crm(cfg: dict, lead_data: dict, workspace_id: Optional[str],
                    workbook_id: Optional[str] = None,
                    lead_id: Optional[int] = None) -> Dict[str, Any]:
    crm_type = (cfg.get("type") or "hubspot").lower()
    if crm_type == "hubspot":
        from apps.api.services.crm import hubspot as crm
        label, id_key = "HubSpot", "hubspot_id"
    elif crm_type == "salesforce":
        from apps.api.services.crm import salesforce as crm
        label, id_key = "Salesforce", "salesforce_id"
    else:
        return {"success": False, "value": "", "error": f"unsupported crm '{crm_type}'"}

    if not crm.is_connected(workspace_id):
        return {"success": False, "value": "", "error": f"{label} not connected"}

    # ── Write-back by external id (CRM sync) ──────────────────────────
    # Rows imported via the crm_import source carry their origin record's id
    # (data.crm_external_id + crm_type). When it matches this column's CRM,
    # UPDATE that record — sending only the mapped fields — instead of the
    # search-by-email/create push. A 404 (record deleted in the CRM since
    # import) falls through to the create path below, and the fresh id is
    # stored back onto the row so later runs keep updating.
    ext_id = str(lead_data.get("crm_external_id") or "").strip()
    row_crm = str(lead_data.get("crm_type") or "").strip().lower()
    crm_deleted = False
    if ext_id and row_crm == crm_type:
        fields = _crm_update_fields(crm_type, cfg, lead_data)
        if crm_type == "hubspot":
            res = await crm.update_contact_by_id(ext_id, fields, workspace_id=workspace_id)
        else:
            crm_object = str(lead_data.get("crm_object") or "contact").lower()
            sobject = {"contact": "Contact", "lead": "Lead"}.get(crm_object, "Contact")
            res = await crm.update_record(sobject, ext_id, fields, workspace_id=workspace_id)
        if res.get("success"):
            return {"success": True, "value": f"{label}: updated {ext_id}", "error": None}
        if not res.get("not_found"):
            return {"success": False, "value": "",
                    "error": res.get("error", f"{label} update failed")}
        crm_deleted = True  # deleted in CRM → recreate below

    # ── Create path (no external id, or the CRM record is gone) ───────
    # push_lead_as_contact reads attributes off a Lead object.
    from apps.api.services.workbook.enrichment import _lead_dict_to_lead
    lead = _lead_dict_to_lead(lead_data)

    res = await crm.push_lead_as_contact(lead, cfg.get("field_map"), workspace_id=workspace_id)
    if res.get("success"):
        new_id = res.get(id_key)
        if crm_deleted and new_id and workbook_id and lead_id is not None:
            # Salesforce's create path makes a Lead sObject, not a Contact.
            new_object = "contact" if crm_type == "hubspot" else "lead"
            _store_row_crm_id(workbook_id, lead_id, crm_type,
                              ext_id, str(new_id), new_object)
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


# Default workbook-column → vendor-field mapping for the cold-email
# destinations (Instantly / Smartlead). Keys are workbook columns / lead
# fields (resolved with the same placeholder resolver the webhook uses);
# values are vendor field names.
_SEQUENCER_DEFAULT_FIELD_MAP = {
    "email": "email",
    "first_name": "first_name",
    "last_name": "last_name",
    "company": "company_name",
}


def _map_lead_fields(fmap: dict, lead_data: dict, columns_config: list) -> Dict[str, Any]:
    """Build {vendor_field: value} from a {workbook_column: vendor_field} map.

    Each workbook-column key is resolved through the same {placeholder}
    resolver the webhook destination uses, so column ids, column names, and
    raw lead fields all work (a key may itself be a template like
    "{first_name} {last_name}"). Unresolvable/empty values are dropped.
    """
    out: Dict[str, Any] = {}
    for src, vendor_field in (fmap or {}).items():
        template = src if "{" in str(src) else "{" + str(src) + "}"
        val = _resolve(template, lead_data, columns_config)
        # The resolver marks unknown single placeholders as "[key: not found]".
        if not val or (val.startswith("[") and val.endswith(": not found]")):
            continue
        out[str(vendor_field)] = val
    return out


async def _push_instantly(cfg: dict, lead_data: dict, columns_config: list,
                          workspace_id: Optional[str]) -> Dict[str, Any]:
    from apps.api.services.integrations import instantly
    campaign_id = str(cfg.get("campaign_id") or cfg.get("campaign") or "").strip()
    if not campaign_id:
        return {"success": False, "value": "", "error": "Instantly campaign_id not configured"}
    lead = _map_lead_fields(cfg.get("field_map") or _SEQUENCER_DEFAULT_FIELD_MAP,
                            lead_data, columns_config)
    res = await instantly.add_lead_to_campaign(
        campaign_id, lead,
        skip_if_in_campaign=cfg.get("skip_if_in_campaign", True),
        workspace_id=workspace_id,
    )
    if res.get("success"):
        note = "already in campaign" if res.get("duplicate") else "added"
        return {"success": True, "value": f"Instantly: {note}", "error": None}
    return {"success": False, "value": "", "error": res.get("error", "instantly push failed")}


async def _push_smartlead(cfg: dict, lead_data: dict, columns_config: list,
                          workspace_id: Optional[str]) -> Dict[str, Any]:
    from apps.api.services.integrations import smartlead
    campaign_id = str(cfg.get("campaign_id") or "").strip()
    if not campaign_id:
        return {"success": False, "value": "", "error": "Smartlead campaign_id not configured"}
    lead = _map_lead_fields(cfg.get("field_map") or _SEQUENCER_DEFAULT_FIELD_MAP,
                            lead_data, columns_config)
    res = await smartlead.add_lead_to_campaign(
        campaign_id, lead,
        settings=cfg.get("settings"),
        workspace_id=workspace_id,
    )
    if res.get("success"):
        note = "already in campaign" if res.get("duplicate") else "added"
        return {"success": True, "value": f"Smartlead: {note}", "error": None}
    return {"success": False, "value": "", "error": res.get("error", "smartlead push failed")}


def _enroll_sequence(cfg: dict, lead_id: int, lead_data: dict, workspace_id: Optional[str]) -> Dict[str, Any]:
    seq_id = cfg.get("sequence_id")
    if not seq_id:
        return {"success": False, "value": "", "error": "sequence_id not configured"}
    if not workspace_id:
        return {"success": False, "value": "", "error": "workspace_id required for sequencer"}
    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.outreach.normalize import normalize_email
    from apps.api.services.outreach.store import get_outreach_store

    email = normalize_email((lead_data or {}).get("email", ""))
    if not email:
        return {"success": False, "value": "", "error": "no_email"}
    try:
        with workspace_scope(workspace_id):
            store = get_outreach_store(workspace_id)
            if not store.sequence_exists(seq_id):
                return {"success": False, "value": "", "error": "sequence not found in workspace"}
            if store.is_suppressed(email):
                return {"success": False, "value": "suppressed", "error": None}
            eid = store.enroll(seq_id, lead_id, email, consent_source="workbook_output")
        ok = eid is not None
        return {"success": ok, "value": "enrolled" if ok else "already enrolled", "error": None}
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
        return await _push_crm(cfg, lead_data, workspace_id,
                               workbook_id=workbook_id, lead_id=lead_id)
    if dest == "sequencer":
        return _enroll_sequence(cfg, lead_id, lead_data, workspace_id)
    if dest == "airtable":
        return await _push_airtable(cfg, lead_data)
    if dest == "sheets":
        return await _push_sheets(cfg, lead_data)
    if dest == "instantly":
        return await _push_instantly(cfg, lead_data, columns_config, workspace_id)
    if dest == "smartlead":
        return await _push_smartlead(cfg, lead_data, columns_config, workspace_id)
    return {"success": False, "value": "", "error": f"unknown destination '{dest}'"}
