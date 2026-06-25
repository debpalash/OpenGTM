"""Trigger evaluation engine — the ``trigger_eval`` job handler (§2 / §3 / §7).

handle_trigger_eval:
  * early-exits when AUTOMATIONS_ENABLED is false,
  * wraps ALL DB work in tenancy.workspace_scope(payload["workspace_id"]) so RLS
    GUC is set (fail-closed otherwise),
  * commits the trigger_run row BEFORE any debit (check_and_debit commits/rolls
    back internally — the run record must survive a credit-failure rollback),
  * per action: idempotency replay-check → cap reservation → debit → committed
    in_flight pre-send marker → external side effect → terminal update,
  * finalizes the run, and for on_schedule rules re-enqueues the next aligned
    job under a single-flight guard.
"""

from __future__ import annotations

import logging
from datetime import datetime, timezone
from typing import Optional

from apps.api.core.config import settings
from apps.api.database import SessionLocal

logger = logging.getLogger("automations.engine")


# ── candidate row resolution ────────────────────────────────────────────────

def _row_cells(row) -> dict:
    """Flatten a WorkbookRow into {key: value} cells for the evaluator/templates.

    Lead fields from ``data`` + enrichment overlay from ``enrichments`` (whose
    values may be {"value": ...} dicts). Enrichment values win on conflict."""
    cells: dict = {}
    for k, v in (row.data or {}).items():
        cells[k] = v
    for cid, cell in (row.enrichments or {}).items():
        if isinstance(cell, dict) and "value" in cell:
            cells[cid] = cell.get("value")
        else:
            cells[cid] = cell
    return cells


def resolve_candidate_rows(db, trigger, payload) -> list:
    """Return [(workbook_id, row_id, row_obj, columns_config)] — always WorkbookRows."""
    from apps.api.services.workbook.models import WorkbookRow, Workbook

    max_rows = int(getattr(settings, "AUTOMATIONS_MAX_ROWS_PER_EVAL", 500))
    out: list = []
    cols_cache: dict = {}

    def cols_for(wb_id: str):
        if wb_id not in cols_cache:
            wb = db.query(Workbook).filter(Workbook.id == wb_id).first()
            cols_cache[wb_id] = (wb.columns_config or []) if wb else []
        return cols_cache[wb_id]

    targets = payload.get("targets")
    if targets:
        for t in targets[:max_rows]:
            wb_id = t.get("workbook_id")
            row_id = t.get("row_id")
            try:
                row = db.query(WorkbookRow).filter(WorkbookRow.id == int(row_id)).first()
            except (ValueError, TypeError):
                row = db.query(WorkbookRow).filter(WorkbookRow.id == row_id).first()
            if row is None or row.workbook_id != wb_id:
                continue
            out.append((wb_id, str(row.id), row, cols_for(wb_id)))
        return out

    # schedule / manual sweep over the rule's scoped workbooks
    scope = trigger.scope_workbook_ids or []
    wq = db.query(Workbook).filter(Workbook.workspace_id == trigger.workspace_id)
    if scope:
        wq = wq.filter(Workbook.id.in_(scope))
    wb_ids = [w.id for w in wq.all()]
    for wb_id in wb_ids:
        rows = (
            db.query(WorkbookRow)
            .filter(WorkbookRow.workbook_id == wb_id)
            .order_by(WorkbookRow.created_at.asc(), WorkbookRow.id.asc())
            .limit(max_rows - len(out))
            .all()
        )
        for row in rows:
            out.append((wb_id, str(row.id), row, cols_for(wb_id)))
        if len(out) >= max_rows:
            break
    return out


def _lead_data_for_action(row, columns_config) -> dict:
    """Build the lead_data dict execute_output_column / templates expect."""
    data = dict(row.data or {})
    # merge enrichment overlay (column_id -> value) so {col} placeholders resolve
    for cid, cell in (row.enrichments or {}).items():
        if isinstance(cell, dict) and "value" in cell:
            data.setdefault(cid, cell.get("value"))
        else:
            data.setdefault(cid, cell)
    data.setdefault("id", row.lead_id or row.id)
    if row.lead_id:
        data.setdefault("lead_id", row.lead_id)
    return data


# ── action execution with idempotency / caps / billing ──────────────────────

async def _run_one_action(db, trigger, run, ws_id, action, action_index,
                          wb_id, row_id, lead_id, lead_data, columns_config,
                          fire_key, dry_run, stats):
    from apps.api.services.automations import actions as actmod
    from apps.api.services.automations import caps as capmod
    from apps.api.services.automations.models import TriggerActionResult

    atype = action.get("type")
    idem = f"trig:{ws_id}:{trigger.id}:{wb_id}:{row_id}:{action_index}:{fire_key}"

    # ── dedup / replay check (steady-state + crash recovery) ──
    existing = (
        db.query(TriggerActionResult)
        .filter(
            TriggerActionResult.workspace_id == ws_id,
            TriggerActionResult.idempotency_key == idem,
        )
        .first()
    )
    if existing is not None:
        if existing.status in ("success", "skipped", "charged_only", "failed"):
            stats["skipped"] += 1
            return
        if existing.status == "in_flight":
            # crash-between-debit-and-send window: at-most-once for non-idempotent.
            if atype in actmod.NON_IDEMPOTENT_ACTION_TYPES:
                existing.status = "skipped"
                existing.skip_reason = "replay"
                db.commit()
                stats["skipped"] += 1
                return
            # idempotent action → safe to re-attempt below; reuse the row.

    stats["attempted"] += 1
    paid = actmod.project_action_cost(action, columns_config) > 0
    cost = actmod.project_action_cost(action, columns_config) if paid else 0.0

    # ── caps + billing for paid actions ──
    reservation = None
    if paid and not dry_run:
        reservation = capmod.try_reserve(db, trigger, ws_id, cost, idem)
        if not reservation.ok:
            _persist_result(db, ws_id, run, trigger, wb_id, row_id, lead_id,
                            action_index, atype, idem, "skipped",
                            skip_reason="cap", summary="daily cap reached")
            stats["skipped"] += 1
            return
        # debit (commits internally) — run row already committed beforehand.
        from apps.api.services.billing import service as billing
        try:
            res = billing.check_and_debit(
                db, ws_id, cost, run_id=idem, reason=f"automation_{atype}",
            )
            cost = res.charged_usd if not res.idempotent_replay else 0.0
        except billing.InsufficientCreditsError:
            capmod.release(db, reservation)
            db.commit()
            _persist_result(db, ws_id, run, trigger, wb_id, row_id, lead_id,
                            action_index, atype, idem, "skipped",
                            skip_reason="credits", summary="insufficient credits")
            stats["skipped"] += 1
            return

    # ── PRE-SEND marker (committed) — closes crash-between-debit-and-send ──
    if not dry_run:
        marker = existing if (existing is not None and existing.status == "in_flight") else None
        if marker is None:
            marker = _persist_result(db, ws_id, run, trigger, wb_id, row_id, lead_id,
                                     action_index, atype, idem, "in_flight",
                                     charged_usd=cost, commit=False)
        else:
            marker.charged_usd = cost
        db.commit()
    else:
        marker = None

    # ── external side effect ──
    result = await actmod.execute_action(
        ws_id, action, wb_id, row_id, lead_id, lead_data, columns_config,
    )

    if dry_run:
        # nothing persisted/charged in a dry run
        return

    # settle/release reservation based on outcome
    if reservation is not None:
        if result.status == "success":
            capmod.settle(db, reservation)
        else:
            capmod.release(db, reservation)
            # refund-on-failure is out of scope; debit already committed. We keep
            # charged_usd recorded so the ledger and run agree.

    # ── terminal update ──
    marker.status = result.status
    marker.skip_reason = result.skip_reason
    marker.result_summary = result.summary
    marker.error = result.error
    marker.charged_usd = cost
    db.commit()

    if result.status == "success":
        stats["succeeded"] += 1
        stats["charged"] += cost
    elif result.status == "skipped":
        stats["skipped"] += 1
    else:
        stats["failed"] += 1


def _persist_result(db, ws_id, run, trigger, wb_id, row_id, lead_id, action_index,
                    atype, idem, status, *, skip_reason=None, summary=None,
                    error=None, charged_usd=0.0, commit=True):
    from apps.api.services.automations.models import TriggerActionResult

    row = TriggerActionResult(
        workspace_id=ws_id,
        run_id=run.id,
        trigger_id=trigger.id,
        workbook_id=wb_id,
        row_id=str(row_id),
        lead_id=lead_id,
        action_index=action_index,
        action_type=atype,
        idempotency_key=idem,
        status=status,
        skip_reason=skip_reason,
        result_summary=summary,
        error=error,
        charged_usd=charged_usd,
    )
    db.add(row)
    if commit:
        db.commit()
    else:
        db.flush()
    return row


# ── main handler ────────────────────────────────────────────────────────────

async def handle_trigger_eval(job_id: int, payload: dict):
    if not getattr(settings, "AUTOMATIONS_ENABLED", False):
        return
    workspace_id = payload.get("workspace_id")
    trigger_id = payload.get("trigger_id")
    if not workspace_id or not trigger_id:
        logger.warning("trigger_eval missing workspace_id/trigger_id: %s", payload)
        return

    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.automations.models import Trigger, TriggerRun

    dry_run = bool(payload.get("dry_run"))
    fire_source = payload.get("fire_source", "manual")
    fire_key = payload.get("fire_key") or f"job:{job_id}"

    with workspace_scope(workspace_id):
        with SessionLocal() as db:
            trigger = (
                db.query(Trigger)
                .filter(Trigger.id == trigger_id, Trigger.workspace_id == workspace_id)
                .first()
            )
            if trigger is None or not trigger.enabled:
                # rule missing/paused → record a skipped run for observability
                run = TriggerRun(
                    workspace_id=workspace_id, trigger_id=trigger_id,
                    trigger_name_snapshot=(trigger.name if trigger else None),
                    job_id=job_id, status="skipped", fire_source=fire_source,
                    fire_key=fire_key, finished_at=datetime.now(timezone.utc),
                )
                db.add(run)
                db.commit()
                return

            # snapshot the rule + create the run row, COMMIT before any debit.
            run = TriggerRun(
                workspace_id=workspace_id, trigger_id=trigger.id,
                trigger_name_snapshot=trigger.name, job_id=job_id,
                status="dry_run" if dry_run else "running",
                fire_source=fire_source, fire_key=fire_key,
            )
            db.add(run)
            db.commit()

            actions = trigger.actions or []
            candidates = resolve_candidate_rows(db, trigger, payload)

            stats = {"attempted": 0, "succeeded": 0, "skipped": 0, "failed": 0, "charged": 0.0}
            matched = 0

            from apps.api.services.automations.safe_conditions import safe_evaluate_condition

            for wb_id, row_id, row, cols in candidates:
                cells = _row_cells(row)
                verdict = safe_evaluate_condition(trigger.condition or "", cells, cols)
                if not verdict.passed:
                    continue
                matched += 1
                lead_data = _lead_data_for_action(row, cols)
                lead_id = row.lead_id
                for idx, action in enumerate(actions):
                    try:
                        await _run_one_action(
                            db, trigger, run, workspace_id, action, idx,
                            wb_id, row_id, lead_id, lead_data, cols,
                            fire_key, dry_run, stats,
                        )
                    except Exception as e:
                        logger.exception("action %s failed on row %s: %s", idx, row_id, e)
                        stats["failed"] += 1
                        if trigger.stop_on_error:
                            break
                    if trigger.stop_on_error and stats["failed"] > 0:
                        break
                if trigger.stop_on_error and stats["failed"] > 0:
                    break

            # finalize run
            run.matched_rows = matched
            run.actions_attempted = stats["attempted"]
            run.actions_succeeded = stats["succeeded"]
            run.actions_skipped = stats["skipped"]
            run.actions_failed = stats["failed"]
            run.total_charged_usd = round(stats["charged"], 4)
            run.finished_at = datetime.now(timezone.utc)
            if dry_run:
                run.status = "dry_run"
            elif stats["failed"] and stats["succeeded"]:
                run.status = "partial"
            elif stats["failed"] and not stats["succeeded"]:
                run.status = "failed" if stats["attempted"] else "completed"
            elif stats["skipped"] and (stats["failed"] or not stats["succeeded"]) and stats["attempted"]:
                run.status = "partial"
            else:
                run.status = "completed"
            if not dry_run:
                trigger.last_fired_at = datetime.now(timezone.utc)
            db.commit()

            # activity feed (best-effort)
            try:
                _log_activity(db, candidates, trigger, run)
            except Exception as e:
                logger.debug("activity log failed: %s", e)

    # on_schedule rules re-enqueue the next aligned eval (single-flight).
    if not dry_run and fire_source == "schedule":
        try:
            _reschedule(workspace_id, trigger_id)
        except Exception as e:
            logger.warning("reschedule failed for %s: %s", trigger_id, e)


def _log_activity(db, candidates, trigger, run):
    from apps.api.services.workbook.activity_models import WorkbookActivity

    wbs = {wb_id for wb_id, _, _, _ in candidates}
    msg = (
        f"automation '{trigger.name}' {run.status}: "
        f"{run.matched_rows} matched, {run.actions_succeeded} ok, "
        f"{run.actions_skipped} skipped, {run.actions_failed} failed, "
        f"${round(run.total_charged_usd or 0.0, 4)}"
    )
    for wb_id in wbs:
        db.add(WorkbookActivity(workbook_id=wb_id, kind="automation", message=msg))
    db.commit()


# ── scheduling (wall-clock aligned, single-flight, mirror bootstrap) §3.7 ────

def _interval_delta(interval: str):
    from datetime import timedelta

    return {
        "hourly": timedelta(hours=1),
        "daily": timedelta(days=1),
        "weekly": timedelta(weeks=1),
    }.get(interval, timedelta(days=1))


def compute_next_run(anchor: datetime, interval: str, now: Optional[datetime] = None) -> datetime:
    """Wall-clock-aligned next run: anchor + ceil((now-anchor)/interval)*interval."""
    now = now or datetime.now(timezone.utc)
    if anchor.tzinfo is None:
        anchor = anchor.replace(tzinfo=timezone.utc)
    delta = _interval_delta(interval)
    if now <= anchor:
        return anchor
    elapsed = (now - anchor).total_seconds()
    step = delta.total_seconds()
    n = int(elapsed // step) + 1
    return anchor + n * delta


def _enqueue_schedule_if_absent(db, trigger, next_run_at):
    """Single-flight: enqueue a schedule trigger_eval only if no pending job with
    the same fire_key exists (§3.7). Also sets triggers.next_run_at atomically."""
    from apps.api.models import Job
    from apps.api.services.queue_service import queue_service

    fire_key = f"sched:{trigger.id}:{next_run_at.isoformat()}"
    # dedup on pending/processing job carrying this fire_key
    pending = (
        db.query(Job)
        .filter(Job.type == "trigger_eval", Job.status.in_(("pending", "processing")))
        .all()
    )
    for j in pending:
        if (j.payload or {}).get("fire_key") == fire_key:
            return False
    job = Job(
        type="trigger_eval",
        payload={
            "trigger_id": trigger.id,
            "workspace_id": trigger.workspace_id,
            "fire_source": "schedule",
            "fire_key": fire_key,
        },
        status="pending",
        priority=1,
        next_run_at=next_run_at,
        max_retries=3,
    )
    db.add(job)
    trigger.next_run_at = next_run_at
    _mirror_upsert(db, trigger.id, trigger.workspace_id, next_run_at, True)
    db.commit()
    return True


def schedule_bootstrap_for_trigger(db, trigger):
    """Compute + set next_run_at and enqueue the first aligned job. Used on
    create/resume for on_schedule rules."""
    interval = (trigger.trigger_config or {}).get("interval", "daily")
    anchor = trigger.schedule_anchor or datetime.now(timezone.utc)
    next_run = compute_next_run(anchor, interval)
    _enqueue_schedule_if_absent(db, trigger, next_run)
    return next_run


def _reschedule(workspace_id: str, trigger_id: str):
    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.automations.models import Trigger

    with workspace_scope(workspace_id):
        with SessionLocal() as db:
            trigger = (
                db.query(Trigger)
                .filter(Trigger.id == trigger_id, Trigger.workspace_id == workspace_id)
                .first()
            )
            if trigger is None or not trigger.enabled or trigger.trigger_type != "on_schedule":
                return
            interval = (trigger.trigger_config or {}).get("interval", "daily")
            anchor = trigger.schedule_anchor or datetime.now(timezone.utc)
            next_run = compute_next_run(anchor, interval)
            _enqueue_schedule_if_absent(db, trigger, next_run)


# ── non-RLS scheduled_triggers mirror (§3.7) ────────────────────────────────

def _mirror_upsert(db, trigger_id, workspace_id, next_run_at, enabled):
    from apps.api.services.automations.models import ScheduledTrigger

    row = db.query(ScheduledTrigger).filter(ScheduledTrigger.trigger_id == trigger_id).first()
    if row is None:
        row = ScheduledTrigger(trigger_id=trigger_id, workspace_id=workspace_id)
        db.add(row)
    row.workspace_id = workspace_id
    row.next_run_at = next_run_at
    row.enabled = enabled


def mirror_set(db, trigger_id, workspace_id, next_run_at, enabled):
    _mirror_upsert(db, trigger_id, workspace_id, next_run_at, enabled)


def mirror_delete(db, trigger_id):
    from apps.api.services.automations.models import ScheduledTrigger

    db.query(ScheduledTrigger).filter(ScheduledTrigger.trigger_id == trigger_id).delete()


def bootstrap_schedules():
    """Cold-start: read the NON-RLS scheduled_triggers mirror (no workspace GUC),
    then per due rule re-enter workspace_scope to enqueue. Survives restarts."""
    if not getattr(settings, "AUTOMATIONS_ENABLED", False):
        return 0
    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.automations.models import ScheduledTrigger, Trigger

    now = datetime.now(timezone.utc)
    enqueued = 0
    # read mirror WITHOUT a workspace scope (mirror is non-RLS by design)
    with SessionLocal() as db:
        due = (
            db.query(ScheduledTrigger)
            .filter(ScheduledTrigger.enabled.is_(True))
            .all()
        )
        due_ids = [(r.trigger_id, r.workspace_id) for r in due
                   if r.next_run_at is None or r.next_run_at <= now]
    for trigger_id, workspace_id in due_ids:
        with workspace_scope(workspace_id):
            with SessionLocal() as db:
                trigger = (
                    db.query(Trigger)
                    .filter(Trigger.id == trigger_id, Trigger.workspace_id == workspace_id)
                    .first()
                )
                if trigger is None or not trigger.enabled or trigger.trigger_type != "on_schedule":
                    continue
                interval = (trigger.trigger_config or {}).get("interval", "daily")
                anchor = trigger.schedule_anchor or now
                next_run = compute_next_run(anchor, interval)
                if _enqueue_schedule_if_absent(db, trigger, next_run):
                    enqueued += 1
    logger.info("bootstrap_schedules enqueued %d schedule eval(s)", enqueued)
    return enqueued
