"""Event emitters that enqueue ``trigger_eval`` jobs (§3.4 / §3.6 / §5).

All emitters are NO-OPs when ``AUTOMATIONS_ENABLED`` is False so the hot enrichment
/ signal write paths are untouched until the feature is turned on.

  * ``on_rows_changed`` — from the enrichment write-back paths (prior-value
    capture), fires ``on_row_changed`` rules whose watch_fields intersect the
    changed fields. Deterministic transition-aware ``fire_key``.
  * ``emit_row_added`` — from the four WorkbookRow insertion sites; fires
    ``on_row_added`` rules.
  * ``emit_signal_matches`` — from ``PgLeadStore.add_signal`` (the only signal
    writer with a real workspace_id); fires ``on_signal`` rules. PG-only.
"""

from __future__ import annotations

import hashlib
import logging
from typing import Iterable, Optional

from apps.api.core.config import settings

logger = logging.getLogger("automations.events")


def _enabled() -> bool:
    return bool(getattr(settings, "AUTOMATIONS_ENABLED", False))


def _sha8(v) -> str:
    return hashlib.sha1(str(v if v is not None else "").encode("utf-8")).hexdigest()[:8]


def _enqueue_eval(db, *, trigger_id, workspace_id, targets, fire_source, fire_key,
                  sweep_id=None, dry_run=False):
    from apps.api.services.queue_service import queue_service

    payload = {
        "trigger_id": trigger_id,
        "workspace_id": workspace_id,
        "targets": targets,
        "fire_source": fire_source,
        "fire_key": fire_key,
        "dry_run": bool(dry_run),
    }
    if sweep_id:
        payload["sweep_id"] = sweep_id
    queue_service.add_job(db, "trigger_eval", payload)


def _enabled_rules(db, workspace_id: str, trigger_type: str):
    """Enabled rules of a type in the workspace (RLS-scoped — caller is in scope)."""
    from apps.api.services.automations.models import Trigger

    return (
        db.query(Trigger)
        .filter(
            Trigger.workspace_id == workspace_id,
            Trigger.trigger_type == trigger_type,
            Trigger.enabled.is_(True),
        )
        .all()
    )


def _rule_covers_workbook(rule, workbook_id: str) -> bool:
    scope = rule.scope_workbook_ids or []
    return (not scope) or (workbook_id in scope)


# ── on_row_changed ──────────────────────────────────────────────────────────

def on_rows_changed(workspace_id: str, workbook_id: str, changes: list, *, db=None) -> int:
    """changes = [{row_id, field, old, new, cell_version?}]. Enqueue one
    trigger_eval per (rule, changed-row) for matching on_row_changed rules whose
    watch_fields intersect the changed fields. Returns # of jobs enqueued."""
    if not _enabled() or not changes:
        return 0
    own_session = db is None
    if own_session:
        from apps.api.database import SessionLocal
        db = SessionLocal()
    enqueued = 0
    try:
        from apps.api.core.tenancy import workspace_scope

        with workspace_scope(workspace_id):
            rules = _enabled_rules(db, workspace_id, "on_row_changed")
            for rule in rules:
                if not _rule_covers_workbook(rule, workbook_id):
                    continue
                watch = set((rule.trigger_config or {}).get("watch_fields") or [])
                # group changed rows for this rule that touch a watched field
                for ch in changes:
                    field = ch.get("field")
                    if watch and field not in watch:
                        continue
                    row_id = ch.get("row_id")
                    cell_version = ch.get("cell_version") or ch.get("new_version") or ""
                    fire_key = (
                        f"rowchg:{row_id}:{field}:"
                        f"{_sha8(ch.get('old'))}->{_sha8(ch.get('new'))}:{cell_version}"
                    )
                    _enqueue_eval(
                        db,
                        trigger_id=rule.id,
                        workspace_id=workspace_id,
                        targets=[{"workbook_id": workbook_id, "row_id": str(row_id)}],
                        fire_source="row_changed",
                        fire_key=fire_key,
                    )
                    enqueued += 1
        if own_session:
            db.commit()
    except Exception as e:  # never break the hot write path
        logger.warning("on_rows_changed emit failed: %s", e)
    finally:
        if own_session:
            db.close()
    return enqueued


# ── on_row_added ────────────────────────────────────────────────────────────

def emit_row_added(workspace_id: str, workbook_id: str, row_ids: Iterable, *, db=None) -> int:
    """Fire on_row_added rules for newly-inserted WorkbookRow ids."""
    if not _enabled():
        return 0
    row_ids = [str(r) for r in (row_ids or [])]
    if not row_ids:
        return 0
    own_session = db is None
    if own_session:
        from apps.api.database import SessionLocal
        db = SessionLocal()
    enqueued = 0
    try:
        from apps.api.core.tenancy import workspace_scope

        with workspace_scope(workspace_id):
            rules = _enabled_rules(db, workspace_id, "on_row_added")
            for rule in rules:
                if not _rule_covers_workbook(rule, workbook_id):
                    continue
                targets = [{"workbook_id": workbook_id, "row_id": rid} for rid in row_ids]
                fire_key = f"rowadd:{workbook_id}:{','.join(sorted(row_ids))[:120]}"
                _enqueue_eval(
                    db,
                    trigger_id=rule.id,
                    workspace_id=workspace_id,
                    targets=targets,
                    fire_source="row_added",
                    fire_key=fire_key,
                )
                enqueued += 1
        if own_session:
            db.commit()
    except Exception as e:
        logger.warning("emit_row_added failed: %s", e)
    finally:
        if own_session:
            db.close()
    return enqueued


# ── on_signal (PG-only, §3.4) ───────────────────────────────────────────────

def emit_signal_matches(db, workspace_id: str, signal_rows: list) -> int:
    """Called from PgLeadStore.add_signal (inside workspace scope) after insert.

    signal_rows = [{signal_pk, signal_type, lead_id}]. For each enabled on_signal
    rule whose signal_types intersect the signal type, map the signal's lead to
    its WorkbookRow(s) within the rule's scope and enqueue one trigger_eval per
    rule with fire_key="signal:<signal_pk>" (globally unique PK → idempotent)."""
    if not _enabled() or not signal_rows:
        return 0
    enqueued = 0
    try:
        from apps.api.services.workbook.models import WorkbookRow, Workbook

        rules = _enabled_rules(db, workspace_id, "on_signal")
        if not rules:
            return 0
        for sig in signal_rows:
            stype = sig.get("signal_type")
            lead_id = sig.get("lead_id")
            spk = sig.get("signal_pk")
            if lead_id is None:
                continue
            for rule in rules:
                wanted = set((rule.trigger_config or {}).get("signal_types") or [])
                if wanted and stype not in wanted:
                    continue
                scope = rule.scope_workbook_ids or []
                rq = (
                    db.query(WorkbookRow.id, WorkbookRow.workbook_id)
                    .join(Workbook, Workbook.id == WorkbookRow.workbook_id)
                    .filter(
                        Workbook.workspace_id == workspace_id,
                        WorkbookRow.lead_id == lead_id,
                    )
                )
                if scope:
                    rq = rq.filter(WorkbookRow.workbook_id.in_(scope))
                targets = [
                    {"workbook_id": wb_id, "row_id": str(rid)} for rid, wb_id in rq.all()
                ]
                if not targets:
                    # signal lead in no scoped workbook → no firing (§3.10)
                    continue
                _enqueue_eval(
                    db,
                    trigger_id=rule.id,
                    workspace_id=workspace_id,
                    targets=targets,
                    fire_source="signal",
                    fire_key=f"signal:{spk}",
                )
                enqueued += 1
    except Exception as e:
        logger.warning("emit_signal_matches failed: %s", e)
    return enqueued
