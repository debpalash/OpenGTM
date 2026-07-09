"""Workbook `crm_import` source kind — pull HubSpot/Salesforce contacts into
a workbook (the import half of CRM enrichment/hygiene; flag-gated).

Source column config (col["source"], or inline on the column):

    {
      "kind": "crm_import",
      "crm": "hubspot" | "salesforce",
      "object": "contact",                  # only contacts in v1
      "filter": "<hubspot list id | SOQL WHERE fragment>",   # optional
      "limit": 500,                          # capped at 5000
      "field_map": {"crm_prop": "lead_field"}                # optional
    }

Rows materialize through the same substrate as ICP sourcing (see
source_engine.materialize_source, which dispatches here): workspace-scoped,
status transitions, row_added broadcasts, and on_row_added automation events
all behave identically. Identity/dedup differs deliberately: CRM contacts are
PERSON-level, so the sync key — data.crm_external_id + data.crm_type — is the
dedup key (re-running the import never duplicates a contact), and rows are NOT
resolved into the company entity graph (a company entity would collapse two
contacts at the same company into one row).

Gated by settings.CRM_IMPORT_ENABLED (default OFF → clean refusal, zero
network calls). Credentials are the same per-workspace-then-global secrets the
push path uses (spec WI-6).
"""

import logging
from typing import Any, Dict, Optional

from apps.api.database import SessionLocal
from apps.api.services.workbook.models import Workbook, WorkbookRow
from apps.api.services.workbook.enrichment import _make_redis, _broadcast

logger = logging.getLogger("workbook.crm_import")

DEFAULT_LIMIT = 500
MAX_LIMIT = 5000

# Lead fields a CRM contact row may carry (the sync-key fields ride alongside).
_ROW_FIELDS = (
    "company", "website", "email", "phone", "city", "state", "address",
    "contact_person", "contact_title", "description", "linkedin_url", "score",
)
_SYNC_FIELDS = ("crm_external_id", "crm_type", "crm_object")


def effective_limit(cfg: dict) -> int:
    """The row cap for an import run: default 500, hard cap 5000, floor 1."""
    try:
        limit = int((cfg or {}).get("limit") or DEFAULT_LIMIT)
    except (TypeError, ValueError):
        limit = DEFAULT_LIMIT
    return max(1, min(limit, MAX_LIMIT))


async def _fetch(cfg: dict, workspace_id: str) -> Dict[str, Any]:
    """Dispatch to the right CRM adapter. Returns the adapter's result dict."""
    crm = str(cfg.get("crm") or "").lower()
    obj = str(cfg.get("object") or "contact").lower()
    if obj != "contact":
        return {"success": False, "error": f"unsupported crm object '{obj}' (only 'contact' in v1)"}
    limit = effective_limit(cfg)
    field_map = cfg.get("field_map") or None
    filt = cfg.get("filter")

    if crm == "hubspot":
        from apps.api.services.crm import hubspot
        return await hubspot.fetch_contacts(
            limit=limit, list_id=(str(filt) if filt else None),
            field_map=field_map, workspace_id=workspace_id,
        )
    if crm == "salesforce":
        from apps.api.services.crm import salesforce
        return await salesforce.fetch_contacts(
            limit=limit, where=(str(filt) if filt else None),
            field_map=field_map, workspace_id=workspace_id,
        )
    return {"success": False, "error": f"unsupported crm '{crm}'"}


def _set_status(workbook_id: str, status: str) -> None:
    with SessionLocal() as db:
        wb = db.query(Workbook).filter(Workbook.id == workbook_id).first()
        if wb:
            wb.status = status
            db.commit()


async def materialize_crm_import(
    workbook_id: str, column_id: str, workspace_id: str, cfg: dict,
) -> Dict[str, Any]:
    """Run a crm_import source column: pull contacts, append new rows.

    Called from source_engine.materialize_source, which has ALREADY entered
    workspace_scope(workspace_id) and validated the column. Returns a summary
    {found, added, skipped, crm} or {"error": ...} — errors are per-run data,
    never exceptions.
    """
    from apps.api.core.config import settings

    if not getattr(settings, "CRM_IMPORT_ENABLED", False):
        return {"error": "crm_import_disabled", "found": 0, "added": 0,
                "detail": "CRM import is disabled (set CRM_IMPORT_ENABLED=true)"}

    crm = str(cfg.get("crm") or "").lower()
    _set_status(workbook_id, "running")
    try:
        res = await _fetch(cfg, workspace_id)
    except Exception as e:  # defensive — adapters already catch, but never leak
        logger.error(f"CRM import fetch crashed for {workbook_id}/{column_id}: {e}")
        res = {"success": False, "error": str(e)[:200]}

    if not res.get("success"):
        _set_status(workbook_id, "draft")
        return {"error": res.get("error", "crm import failed"),
                "found": 0, "added": 0, "crm": crm}

    contacts = res.get("contacts") or []
    found = len(contacts)
    redis_client = _make_redis()
    added = skipped = 0
    _added_row_ids: list = []
    try:
        with SessionLocal() as db:
            # Sync keys already present in THIS workbook (re-run idempotency).
            present = set()
            for (data,) in (
                db.query(WorkbookRow.data)
                .filter(WorkbookRow.workbook_id == workbook_id)
                .all()
            ):
                d = data or {}
                if d.get("crm_external_id"):
                    present.add((str(d.get("crm_type") or "").lower(),
                                 str(d["crm_external_id"])))
            max_pos = (
                db.query(WorkbookRow.position)
                .filter(WorkbookRow.workbook_id == workbook_id)
                .order_by(WorkbookRow.position.desc())
                .limit(1)
                .scalar()
            ) or 0

            for contact in contacts:
                ext_id = str(contact.get("crm_external_id") or "").strip()
                if not ext_id:
                    skipped += 1
                    continue
                key = (str(contact.get("crm_type") or "").lower(), ext_id)
                if key in present:
                    skipped += 1
                    continue
                present.add(key)

                row_data = {
                    k: contact.get(k) for k in _ROW_FIELDS
                    if contact.get(k) not in (None, "")
                }
                # field_map may target lead fields outside _ROW_FIELDS; keep them.
                for k, v in contact.items():
                    if k not in row_data and k not in _SYNC_FIELDS and v not in (None, ""):
                        row_data[k] = v
                for k in _SYNC_FIELDS:
                    if contact.get(k):
                        row_data[k] = str(contact[k])

                max_pos += 1
                row = WorkbookRow(
                    workbook_id=workbook_id,
                    workspace_id=workspace_id,
                    position=max_pos,
                    data=row_data,
                    lead_id=None,
                    enrichments={},
                )
                db.add(row)
                db.flush()  # get row.id
                added += 1
                _added_row_ids.append(row.id)

                if redis_client is not None:
                    await _broadcast(redis_client, workbook_id, {
                        "type": "row_added",
                        "rowId": row.id,
                        "data": row_data,
                    })

            db.commit()

            wb = db.query(Workbook).filter(Workbook.id == workbook_id).first()
            if wb:
                wb.total_rows = db.query(WorkbookRow).filter(
                    WorkbookRow.workbook_id == workbook_id
                ).count()
                wb.status = "draft"  # import done; ready to enrich
                db.commit()

        # Automations: on_row_added (same contract as ICP sourcing).
        if _added_row_ids and workspace_id:
            try:
                from apps.api.services.automations import events as _auto_events
                _auto_events.emit_row_added(workspace_id, workbook_id, _added_row_ids)
            except Exception as _e:
                logger.warning("on_row_added emit (crm_import) failed: %s", _e)
    finally:
        if redis_client is not None:
            try:
                await _broadcast(redis_client, workbook_id, {
                    "type": "source_done",
                    "columnId": column_id,
                    "found": found, "added": added, "skipped": skipped,
                })
            finally:
                try:
                    await redis_client.aclose()
                except Exception:
                    pass

    logger.info(
        f"CRM import {workbook_id}/{column_id} ({crm}): "
        f"found={found} added={added} skipped={skipped}"
    )
    return {"found": found, "added": added, "skipped": skipped, "crm": crm}
