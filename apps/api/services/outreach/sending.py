"""Outreach send job + autonomous ticker (spec §6.2, §6.8).

``handle_send`` is the safety-critical at-most-once path (§6.2.1), reimplemented
from the ``_run_one_action`` ordering. The autonomous ticker reuses the
automations schedule-mirror / bootstrap / single-flight pattern verbatim in
shape.

NEVER import-time side effects: registration happens in worker.py / main.py.
"""

from __future__ import annotations

import html as _html
import logging
from datetime import datetime, timedelta, timezone
from typing import Optional, Tuple
from zoneinfo import ZoneInfo, ZoneInfoNotFoundError

from apps.api.core.config import settings
from apps.api.database import SessionLocal
from apps.api.services.outreach.normalize import normalize_email
from apps.api.services.outreach.orm_models import OutreachSend
from apps.api.services.outreach.store import PgOutreachStore, get_outreach_store
from apps.api.services.outreach.tokens import unsubscribe_url

logger = logging.getLogger("outreach.sender")


def _utcnow() -> datetime:
    return datetime.now(timezone.utc)


# ── email body composition (footer + List-Unsubscribe, BOTH parts) ───────────

def _footer(workspace_id: str) -> str:
    from apps.api.services.workspace.secrets import get_secret

    return (get_secret(workspace_id, "OUTREACH_FOOTER", "") or "").strip()


def build_message(
    workspace_id: str,
    to_email: str,
    subject: str,
    body_html: str,
    sequence_id: str = "",
) -> Tuple[str, str, dict]:
    """Return (html, text, headers) with unsubscribe link + footer in BOTH parts.

    Raises ``ValueError('no_footer')`` when the mandatory physical-address footer
    is missing (sending is BLOCKED, spec §6.5/§6.7).
    """
    footer = _footer(workspace_id)
    if not footer:
        raise ValueError("no_footer")

    unsub = unsubscribe_url(workspace_id, to_email, sequence_id)

    # Plaintext body derived from the HTML (the handler owns plaintext so the
    # conspicuous link survives — send_email no longer strips it, §6.5).
    import re
    text_body = re.sub(r"<[^>]+>", "", body_html)
    text_body = re.sub(r"\s+\n", "\n", text_body)
    text_body = re.sub(r"[ \t]+", " ", text_body).strip()

    html_block = (
        f'{body_html}'
        f'<hr style="border:none;border-top:1px solid #eee;margin:16px 0">'
        f'<p style="color:#999;font-size:12px">{_html.escape(footer)}</p>'
        f'<p style="color:#999;font-size:12px">'
        f'<a href="{_html.escape(unsub)}">Unsubscribe</a></p>'
    )
    text_full = (
        f"{text_body}\n\n--\n{footer}\n\n"
        f"Unsubscribe: {unsub}\n"
    )
    headers = {
        "List-Unsubscribe": f"<{unsub}>",
        "List-Unsubscribe-Post": "List-Unsubscribe=One-Click",
    }
    return html_block, text_full, headers


# ── send-window evaluation (§6.3) ────────────────────────────────────────────

def _in_send_window(seq: dict, now: Optional[datetime] = None) -> bool:
    start = seq.get("send_window_start")
    end = seq.get("send_window_end")
    if start is None or end is None or start == end:
        return True  # no window configured
    tzname = seq.get("send_window_tz") or "UTC"
    try:
        tz = ZoneInfo(tzname)
    except (ZoneInfoNotFoundError, ValueError):
        tz = timezone.utc
    now = now or _utcnow()
    local_hour = now.astimezone(tz).hour
    if start < end:
        return start <= local_hour < end
    # window wraps midnight
    return local_hour >= start or local_hour < end


def _next_window_slot(seq: dict, now: Optional[datetime] = None) -> datetime:
    """The next in-window UTC instant (top of the start hour, local)."""
    start = seq.get("send_window_start") or 0
    tzname = seq.get("send_window_tz") or "UTC"
    try:
        tz = ZoneInfo(tzname)
    except (ZoneInfoNotFoundError, ValueError):
        tz = timezone.utc
    now = now or _utcnow()
    local = now.astimezone(tz)
    candidate = local.replace(hour=int(start), minute=0, second=0, microsecond=0)
    if candidate <= local:
        candidate = candidate + timedelta(days=1)
    return candidate.astimezone(timezone.utc)


# ── the at-most-once send handler (§6.2.1) ───────────────────────────────────

async def handle_send(job_id: int, payload: dict) -> None:
    """Process one ``send`` job. Wraps ALL DB work in workspace_scope + the store
    (so ``assert_rls_role`` fires in the worker). See spec §6.2.1 for the exact,
    crash-safe ordering."""
    from apps.api.core.tenancy import workspace_scope

    if not getattr(settings, "AUTOMATIONS_ENABLED", False):
        return  # master switch off — early exit like handle_trigger_eval

    ws_id = payload["workspace_id"]
    idem = payload["idempotency_key"]

    with workspace_scope(ws_id):
        store = get_outreach_store(ws_id)
        await _do_send(store, ws_id, idem, payload)


async def _do_send(store: PgOutreachStore, ws_id: str, idem: str, payload: dict) -> None:
    from apps.api.services.outreach.sender import get_smtp_config, send_email

    seq_id = payload.get("sequence_id") or None
    enrollment_id = payload.get("enrollment_id")
    step_number = int(payload.get("step_number", 0) or 0)
    lead_id = payload.get("lead_id")

    # 1+2. Replay / dedup on (workspace_id, idempotency_key).
    existing = store.get_send_by_idem(idem)
    if existing is not None:
        if existing.status in ("sent", "failed", "skipped", "bounced"):
            return  # terminal — already handled, no send
        # in_flight → crash-replay; send_email is non-idempotent.
        _finalize_send(store, idem, status="skipped", skip_reason="replay")
        return

    # 3. Load enrollment (sequence-driven). Missing → skipped.
    seq = store.get_sequence(seq_id) if seq_id else None
    to_email = normalize_email(payload.get("to_email", ""))
    if enrollment_id is not None:
        enr = store.get_enrollment(enrollment_id)
        if enr is None:
            _record_terminal(store, ws_id, idem, payload, "skipped", skip_reason="lead_not_found")
            return
        to_email = normalize_email(enr.to_email_snapshot)
        if not seq_id:
            seq_id = enr.sequence_id
            seq = store.get_sequence(seq_id)

    if not to_email:
        _record_terminal(store, ws_id, idem, payload, "skipped", skip_reason="no_email")
        return

    # Resolve subject/body for this step (sequence-driven), else from payload.
    subject = payload.get("subject", "")
    body_html = payload.get("body_html", "")
    if seq:
        steps = seq.get("steps") or []
        step = next((s for s in steps if int(s.get("step_number", 0)) == step_number), None)
        if step is None and steps:
            step = steps[0]
        if step:
            from apps.api.services.outreach.sender import render_template

            variables = payload.get("variables") or {"email": to_email}
            subject = render_template(step.get("subject", ""), variables)
            body_html = render_template(step.get("body_html", ""), variables)

    # 4. Send-window check → reschedule (NOT terminal).
    if seq and not _in_send_window(seq):
        if enrollment_id is not None:
            store.reschedule_enrollment(enrollment_id, _next_window_slot(seq))
        return

    # 5. Suppression check immediately before send.
    if store.is_suppressed(to_email):
        _record_terminal(store, ws_id, idem, payload, "skipped", skip_reason="suppressed",
                         to_email=to_email, seq_id=seq_id)
        if enrollment_id is not None:
            store.advance_enrollment(enrollment_id, status="suppressed")
        return

    # 6. Durable per-workspace hourly rate → reschedule (NOT terminal).
    cfg = get_smtp_config(ws_id)
    if not (cfg.host and cfg.email and cfg.password):
        _record_terminal(store, ws_id, idem, payload, "failed", error="SMTP not configured",
                         to_email=to_email, seq_id=seq_id)
        return
    if store.sent_count_last_hour() >= cfg.max_per_hour:
        if enrollment_id is not None:
            store.reschedule_enrollment(enrollment_id, _utcnow() + timedelta(minutes=15))
        return
    # Per-sequence daily limit.
    if seq and store.sent_count_today(seq_id) >= (seq.get("daily_limit") or 50):
        if enrollment_id is not None:
            store.reschedule_enrollment(enrollment_id, _utcnow() + timedelta(hours=1))
        return

    # 7. Caps reservation (only when cost > 0). The DEBIT is deferred to AFTER a
    # successful SMTP handoff (LOCKED SCOPE decision 3: debit-on-success).
    cost = float(settings.OUTREACH_SEND_COST_USD or 0.0)
    paid = cost > 0
    reservation = None
    if paid:
        from apps.api.services.automations import caps as capmod

        class _CapTrigger:
            id = seq_id or "outreach"
            max_actions_per_day = None
            max_spend_usd_per_day = None

        with store._session() as db:
            reservation = capmod.try_reserve(db, _CapTrigger(), ws_id, cost, idem)
            db.commit()  # try_reserve only flushes; persist the held row.
        if reservation is not None and not reservation.ok:
            _record_terminal(store, ws_id, idem, payload, "skipped", skip_reason="cap",
                             to_email=to_email, seq_id=seq_id)
            return

    # 8. PRE-SEND in_flight marker — committed BEFORE SMTP, UNCONDITIONALLY (even
    # when unmetered). This is what makes the queue's automatic retry at-most-once.
    try:
        with store._session() as db, db.begin():
            db.add(OutreachSend(
                workspace_id=ws_id,
                sequence_id=seq_id,
                enrollment_id=enrollment_id,
                lead_id=lead_id,
                step_number=step_number,
                to_email=to_email,
                subject=subject,
                status="in_flight",
                idempotency_key=idem,
                charged_usd=0.0,
            ))
    except Exception:
        # Concurrent insert hit uq_outreach_send_idem → treat as replay (step 2).
        _settle_reservation(store, ws_id, reservation, ok=False)
        re_read = store.get_send_by_idem(idem)
        if re_read is not None and re_read.status == "in_flight":
            _finalize_send(store, idem, status="skipped", skip_reason="replay")
        return

    # 9. Build message (footer + List-Unsubscribe in BOTH parts) + 10. SMTP.
    try:
        html_body, text_body, headers = build_message(
            ws_id, to_email, subject, body_html, seq_id or ""
        )
    except ValueError as e:
        if str(e) == "no_footer":
            _settle_reservation(store, ws_id, reservation, ok=False)
            _finalize_send(store, idem, status="skipped", skip_reason="no_footer")
            return
        raise

    result = await send_email(
        to_email=to_email,
        subject=subject,
        body_html=html_body,
        body_text=text_body,
        config=cfg,
        headers=headers,
    )

    # 10. Classify + 11. DEBIT-ON-SUCCESS + settle/terminal.
    if result.success:
        charged = _debit_on_success(store, ws_id, idem, cost) if paid else 0.0
        _settle_reservation(store, ws_id, reservation, ok=True)
        _finalize_send(store, idem, status="sent", message_id=result.message_id,
                       sent_at=_utcnow(), charged_usd=charged)
        if enrollment_id is not None:
            _advance_after_send(store, enrollment_id, seq, step_number)
        return

    # Failure: NEVER charged (no debit ran). Release the reservation.
    err = result.error or ""
    if _is_hard_bounce(err):
        _settle_reservation(store, ws_id, reservation, ok=False)
        _finalize_send(store, idem, status="bounced", error=err, bounced_at=_utcnow())
        store.add_suppression(to_email, reason="bounce", source=seq_id or "send")
        if enrollment_id is not None:
            store.advance_enrollment(enrollment_id, status="bounced", error=err)
        store.record_bounce_and_maybe_pause(seq_id, complaint=False)
        return

    # Transient: mark failed; the in_flight marker makes the queue retry
    # short-circuit to skipped/replay (at-most-once). Re-raise so the queue
    # records the failure + schedules a retry.
    _settle_reservation(store, ws_id, reservation, ok=False)
    _finalize_send(store, idem, status="failed", error=err)
    raise RuntimeError(f"outreach send transient failure: {err[:160]}")


def _debit_on_success(store: PgOutreachStore, ws_id: str, idem: str, cost: float) -> float:
    """Charge the workspace AFTER a successful SMTP handoff. Idempotent on
    run:<idem> so a retry that already debited never double-charges."""
    from apps.api.services.billing import service as billing

    with store._session() as db:
        res = billing.check_and_debit(db, ws_id, cost, run_id=idem, reason="outreach_send")
        return res.charged_usd if not res.idempotent_replay else 0.0


def _advance_after_send(store: PgOutreachStore, enrollment_id: int, seq: Optional[dict], step_number: int) -> None:
    steps = (seq or {}).get("steps") or []
    next_step = step_number + 1
    next_def = next((s for s in steps if int(s.get("step_number", 0)) == next_step), None)
    if next_def is None:
        store.advance_enrollment(enrollment_id, status="completed", increment_sent=True,
                                 current_step=step_number)
        return
    delay_h = int(next_def.get("delay_hours", 0) or 0)
    store.advance_enrollment(
        enrollment_id, status="scheduled", increment_sent=True,
        current_step=next_step, next_send_at=_utcnow() + timedelta(hours=delay_h),
    )


def _is_hard_bounce(error: str) -> bool:
    e = (error or "").lower()
    if " 5" in f" {e}":
        # crude 5xx detection: look for a 5xx code token
        import re
        if re.search(r"\b5\d\d\b", e):
            return True
    return any(t in e for t in ("mailbox unavailable", "user unknown", "no such user",
                                "recipient rejected", "does not exist", "550", "551",
                                "553", "554"))


def _settle_reservation(store, ws_id, reservation, ok: bool) -> None:
    if reservation is None or not getattr(reservation, "ok", False):
        return
    from apps.api.services.automations import caps as capmod

    with store._session() as db, db.begin():
        if ok:
            capmod.settle(db, reservation)
        else:
            capmod.release(db, reservation)


def _finalize_send(store: PgOutreachStore, idem: str, *, status: str,
                   skip_reason: Optional[str] = None, message_id: str = "",
                   error: str = "", sent_at: Optional[datetime] = None,
                   bounced_at: Optional[datetime] = None,
                   charged_usd: Optional[float] = None) -> None:
    with store._session() as db, db.begin():
        row = (
            db.query(OutreachSend)
            .filter(
                OutreachSend.workspace_id == store.workspace_id,
                OutreachSend.idempotency_key == idem,
            )
            .first()
        )
        if row is None:
            return
        row.status = status
        if skip_reason:
            row.skip_reason = skip_reason
        if message_id:
            row.message_id = message_id
        if error:
            row.error = error
        if sent_at:
            row.sent_at = sent_at
        if bounced_at:
            row.bounced_at = bounced_at
        if charged_usd is not None:
            row.charged_usd = charged_usd


def _record_terminal(store: PgOutreachStore, ws_id: str, idem: str, payload: dict,
                     status: str, *, skip_reason: Optional[str] = None, error: str = "",
                     to_email: str = "", seq_id: Optional[str] = None) -> None:
    """Insert a terminal send row directly (for skips that never reach in_flight)."""
    to_email = to_email or normalize_email(payload.get("to_email", "")) or "unknown@unknown"
    try:
        with store._session() as db, db.begin():
            db.add(OutreachSend(
                workspace_id=ws_id,
                sequence_id=seq_id or payload.get("sequence_id"),
                enrollment_id=payload.get("enrollment_id"),
                lead_id=payload.get("lead_id"),
                step_number=int(payload.get("step_number", 0) or 0),
                to_email=to_email,
                subject=payload.get("subject", ""),
                status=status,
                skip_reason=skip_reason,
                error=error,
                idempotency_key=idem,
            ))
    except Exception:
        # idem already present → leave existing row.
        pass


# ── enqueue helpers (idempotency key scheme §8.1) ────────────────────────────

def sequence_step_idem(ws_id: str, sequence_id: str, enrollment_id: int, step: int) -> str:
    return f"seq:{ws_id}:{sequence_id}:{enrollment_id}:{step}"


def _enqueue_if_absent(db, ws_id: str, idem: str, payload: dict) -> bool:
    """Single-flight: enqueue a ``send`` job only if no pending/processing job
    with this idem key exists AND no terminal/in_flight send row exists."""
    from apps.api.models import Job
    from apps.api.services.queue_service import queue_service

    pending = (
        db.query(Job)
        .filter(Job.type == "send", Job.status.in_(("pending", "processing")))
        .all()
    )
    for j in pending:
        if (j.payload or {}).get("idempotency_key") == idem:
            return False
    # Also dedup against an already-recorded send (collapses /execute + ticker).
    existing = (
        db.query(OutreachSend)
        .filter(OutreachSend.workspace_id == ws_id, OutreachSend.idempotency_key == idem)
        .first()
    )
    if existing is not None:
        return False
    queue_service.add_job(db, "send", {**payload, "idempotency_key": idem})
    return True


def enqueue_due_sends(store: PgOutreachStore, ws_id: str, seq_id: str, limit: int) -> int:
    """Enqueue ``send`` jobs for due enrollment steps. Returns count enqueued."""
    due = store.due_enrollments(seq_id, limit=limit)
    enqueued = 0
    # No outer transaction: add_job commits per job (matches the automations
    # enqueue path); the dedup read shares the same session.
    with store._session() as db:
        for enr in due:
            # The step to send is the current_step (0-indexed → step_number).
            step_number = int(enr["current_step"] or 0)
            idem = sequence_step_idem(ws_id, seq_id, enr["id"], step_number)
            payload = {
                "workspace_id": ws_id,
                "sequence_id": seq_id,
                "enrollment_id": enr["id"],
                "lead_id": enr["lead_id"],
                "step_number": step_number,
                "to_email": enr["to_email_snapshot"],
            }
            if _enqueue_if_absent(db, ws_id, idem, payload):
                enqueued += 1
                if enqueued >= limit:
                    break
    return enqueued


# ── autonomous ticker + bootstrap (§6.8) ─────────────────────────────────────

def tick_sequence(ws_id: str, seq_id: str) -> int:
    """One tick for a single active sequence: enqueue due sends + reschedule."""
    from apps.api.core.tenancy import workspace_scope

    with workspace_scope(ws_id):
        store = get_outreach_store(ws_id)
        seq = store.get_sequence(seq_id)
        if not seq or seq.get("status") != "active":
            store.disable_schedule(seq_id)
            return 0
        n = enqueue_due_sends(store, ws_id, seq_id, settings.OUTREACH_TICK_MAX_ENQUEUE)
        next_tick = _utcnow() + timedelta(seconds=settings.OUTREACH_TICK_INTERVAL)
        store.upsert_schedule(seq_id, next_tick, enabled=True)
        return n


def bootstrap_outreach_schedules() -> int:
    """Cold-start: read the NON-RLS outreach_schedules mirror (no workspace GUC),
    then per due sequence re-enter workspace_scope to enqueue due sends. Survives
    restarts. Mirrors automations.bootstrap_schedules."""
    if not getattr(settings, "AUTOMATIONS_ENABLED", False):
        return 0
    from apps.api.services.outreach.orm_models import OutreachSchedule

    now = _utcnow()
    enqueued = 0
    with SessionLocal() as db:
        due = (
            db.query(OutreachSchedule)
            .filter(OutreachSchedule.enabled.is_(True))
            .all()
        )
        due_ids = [
            (r.sequence_id, r.workspace_id)
            for r in due
            if r.next_tick_at is None or r.next_tick_at <= now
        ]
    for seq_id, ws_id in due_ids:
        try:
            enqueued += tick_sequence(ws_id, seq_id)
        except Exception as e:  # never let one bad sequence break bootstrap
            logger.warning("outreach tick failed seq=%s ws=%s: %s", seq_id, ws_id, e)
    logger.info("bootstrap_outreach_schedules enqueued %d send job(s)", enqueued)
    return enqueued
