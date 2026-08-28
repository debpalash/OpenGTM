"""Intent-poller engine — the ``watch_poll`` job handler + scheduling (spec §2/§3/§7).

Mirrors ``automations.engine`` (scheduler / single-flight / mirror / bootstrap)
and ``outreach.sending`` (workspace_scope worker pattern). Key guarantees:

  * **Tenancy** — every watch / cursor / budget write runs inside ``workspace_scope``
    on the non-super RLS role (``assert_rls_role``), fail-closed on unset GUC.
  * **Budget** — atomic per-(ws, UTC-day) ``INSERT ... ON CONFLICT DO UPDATE
    SET n=n+1 RETURNING n``; over budget → early-exit + reschedule.
  * **Per-source atomicity** — each source emits its signals AND advances its
    cursor sub-key in ONE transaction; a crash rolls both back and deterministic
    ``signals.id`` makes the retry a no-op (no dropped first batch, spec §8.3).
  * **Single-flight** — ``_enqueue_poll_if_absent`` takes ``pg_advisory_xact_lock``
    + relies on the partial unique index ``uq_jobs_fire_key_active``; SQLite is
    best-effort read-then-insert (single process).
  * **Backoff / auto-disable** — exponential backoff on consecutive failures,
    hard disable after ``INTENT_POLLER_MAX_CONSECUTIVE_FAILURES``.

Webhooks are NEVER sent here — delivery is on_signal→webhook rules only.
"""

from __future__ import annotations

import logging
import time
from datetime import datetime, date, timedelta, timezone
from typing import Optional

from sqlalchemy import text
from sqlalchemy.exc import IntegrityError

from apps.api.core.config import settings
from apps.api.database import IS_SQLITE, SessionLocal
from apps.api.services.automations.engine import compute_next_run, _interval_delta
from apps.api.services.poller import keys, sources
from apps.api.services.poller.models import (
    PollBudgetLedger,
    WatchSchedule,
    WatchSubscription,
)

logger = logging.getLogger("poller.engine")


def _utcnow() -> datetime:
    return datetime.now(timezone.utc)


def _enabled() -> bool:
    return bool(getattr(settings, "INTENT_POLLER_ENABLED", False))


def _pg() -> bool:
    return not IS_SQLITE


def _is_pg(db) -> bool:
    """Dialect of the bound session (robust under test fixtures that bind a PG
    engine while the global IS_SQLITE flag reflects a different default URL)."""
    try:
        return db.get_bind().dialect.name == "postgresql"
    except Exception:
        return not IS_SQLITE


# ── single-flight enqueue (advisory lock + partial unique index, spec §3.4) ──

def _enqueue_poll_if_absent(db, *, fire_key: str, workspace_id: str, watch_id: str,
                            next_run_at: datetime) -> bool:
    """Enqueue a ``watch_poll`` job under a single-flight guard.

    Postgres: ``pg_advisory_xact_lock(hashtext('watch_poll:'||fire_key))`` then
    insert; an ``IntegrityError`` on ``uq_jobs_fire_key_active`` means another
    racer won → treat as already-enqueued. SQLite: best-effort read-then-insert
    (single process). Returns True when THIS call enqueued the job."""
    from apps.api.models import Job

    if _is_pg(db):
        db.execute(
            text("SELECT pg_advisory_xact_lock(hashtext(:k))"),
            {"k": f"watch_poll:{fire_key}"},
        )
    # read existing active job with this fire_key
    existing = (
        db.query(Job)
        .filter(
            Job.type == "watch_poll",
            Job.fire_key == fire_key,
            Job.status.in_(("pending", "processing")),
        )
        .first()
    )
    if existing is not None:
        return False
    job = Job(
        type="watch_poll",
        payload={"watch_id": watch_id, "workspace_id": workspace_id, "fire_key": fire_key},
        status="pending",
        priority=1,
        next_run_at=next_run_at,
        max_retries=3,
        fire_key=fire_key,
    )
    db.add(job)
    try:
        db.flush()
    except IntegrityError:
        db.rollback()
        return False
    return True


def scheduled_fire_key(watch_id: str, next_run_at: datetime) -> str:
    return f"watch:{watch_id}:{next_run_at.isoformat()}"


# ── mirror (non-RLS, ids/ts only) ────────────────────────────────────────────

def _mirror_upsert(db, watch_id, workspace_id, next_poll_at, enabled):
    row = db.query(WatchSchedule).filter(WatchSchedule.watch_id == watch_id).first()
    if row is None:
        row = WatchSchedule(watch_id=watch_id, workspace_id=workspace_id)
        db.add(row)
    row.workspace_id = workspace_id  # verbatim (spec §6 mirror integrity)
    row.next_poll_at = next_poll_at
    row.enabled = enabled
    row.updated_at = _utcnow()


def mirror_set(db, watch_id, workspace_id, next_poll_at, enabled):
    _mirror_upsert(db, watch_id, workspace_id, next_poll_at, enabled)


def mirror_delete(db, watch_id):
    db.query(WatchSchedule).filter(WatchSchedule.watch_id == watch_id).delete()


# ── budget (atomic per-(ws, UTC-day), spec §3.3 / §7) ────────────────────────

def _debit_budget(db, workspace_id: str, *, poll_now: bool = False) -> tuple[int, int]:
    """Atomic increment of the per-(ws, today) budget counter.

    Returns ``(scheduled_n, poll_now_n)`` AFTER this increment. Uses
    ``INSERT ... ON CONFLICT DO UPDATE SET n=n+1 RETURNING`` on PG; on SQLite the
    same upsert (single-writer serialised). ``poll_now`` increments BOTH the
    shared budget (closes the bypass) and the separate poll-now quota counter."""
    today = _utcnow().date()
    if _is_pg(db):
        # increment shared n always; increment n_poll_now only for poll-now.
        sql = text(
            "INSERT INTO poll_budget_ledger (workspace_id, day, n, n_poll_now) "
            "VALUES (:ws, :day, 1, :pn) "
            "ON CONFLICT (workspace_id, day) DO UPDATE "
            "SET n = poll_budget_ledger.n + 1, "
            "    n_poll_now = poll_budget_ledger.n_poll_now + :pn "
            "RETURNING n, n_poll_now"
        )
        row = db.execute(sql, {"ws": workspace_id, "day": today, "pn": 1 if poll_now else 0}).first()
        return int(row[0]), int(row[1])
    # SQLite upsert
    sql = text(
        "INSERT INTO poll_budget_ledger (workspace_id, day, n, n_poll_now) "
        "VALUES (:ws, :day, 1, :pn) "
        "ON CONFLICT (workspace_id, day) DO UPDATE "
        "SET n = n + 1, n_poll_now = n_poll_now + :pn"
    )
    db.execute(sql, {"ws": workspace_id, "day": today, "pn": 1 if poll_now else 0})
    rec = db.execute(
        text("SELECT n, n_poll_now FROM poll_budget_ledger WHERE workspace_id=:ws AND day=:day"),
        {"ws": workspace_id, "day": today},
    ).first()
    return int(rec[0]), int(rec[1])


def poll_now_quota_used(db, workspace_id: str) -> int:
    today = _utcnow().date()
    rec = db.execute(
        text("SELECT n_poll_now FROM poll_budget_ledger WHERE workspace_id=:ws AND day=:day"),
        {"ws": workspace_id, "day": today},
    ).first()
    return int(rec[0]) if rec else 0


# ── lead resolution (tenant-scoped + disambiguated, spec §6) ─────────────────

def _resolve_lead_id(db, watch) -> tuple[Optional[int], Optional[str]]:
    """Return ``(lead_id, error)``. Pinned watch.lead_id wins. Else exact
    normalized-name match → lowest lead_id; >1 fuzzy match → (None, 'ambiguous_lead');
    no match → (None, None) (skip silently)."""
    if watch.lead_id:
        return watch.lead_id, None
    if watch.kind in {"job_change", "account_group"}:
        # target is a free-form roster label, not a company name — per-contact
        # or per-account lead routing happens on the DetectedEvent itself.
        return None, None
    from apps.api.services.leadgen.orm_models import LeadRow

    target_norm = keys.normalize_target(watch.target)
    if not target_norm:
        return None, None
    rows = (
        db.query(LeadRow.id, LeadRow.company)
        .filter(LeadRow.workspace_id == watch.workspace_id)
        .all()
    )
    exact = []
    fuzzy = []
    for lid, company in rows:
        cn = keys.normalize_target(company or "")
        if not cn:
            continue
        if cn == target_norm:
            exact.append(lid)
        elif target_norm in cn or cn in target_norm:
            fuzzy.append(lid)
    if exact:
        return min(exact), None
    if len(fuzzy) > 1:
        return None, "ambiguous_lead"
    if len(fuzzy) == 1:
        return fuzzy[0], None
    return None, None


# ── signal emission (deterministic id → exactly-once, spec §8.2) ─────────────

def _emit_events(store, watch, lead_id: Optional[int], events) -> tuple[int, int]:
    """Write each DetectedEvent via PgLeadStore.add_signal (fires on_signal once).

    ``add_signal`` is idempotent on the deterministic ``signals.id`` (re-poll =
    no-op, no second on_signal fire, spec §8.2). Returns ``(count, 0)`` — the
    count is for observability; true inserted-vs-dupe is the store's concern.
    ``ev.lead_id`` / ``ev.company`` (job_change contacts) override the
    watch-level routing when set."""
    from apps.api.services.signals.monitor import Signal

    count = 0
    stable = keys.stable_company_id(watch.resolved_cik, watch.target)
    for ev in events:
        sig_id = keys.signal_event_id(
            watch.workspace_id, watch.kind, stable, ev.signal_type, ev.natural_event_id
        )
        ev_lead = getattr(ev, "lead_id", None)
        signal = Signal(
            id=sig_id,
            workspace_id=watch.workspace_id,
            lead_id=ev_lead if ev_lead is not None else (lead_id or 0),
            company=getattr(ev, "company", "") or watch.target,
            signal_type=ev.signal_type,
            title=ev.title,
            description=ev.description,
            source=ev.source,
            source_url=ev.source_url,
            weight=ev.weight,
            created_at=ev.occurred_at or time.time(),
        )
        store.add_signal(signal)
        count += 1
    return count, 0


# ── reschedule + backoff (spec §9.16) ────────────────────────────────────────

def _reschedule_watch(db, watch, *, failed: bool):
    interval = watch.interval or settings.INTENT_POLLER_DEFAULT_INTERVAL
    anchor = watch.schedule_anchor or _utcnow()
    if anchor.tzinfo is None:
        anchor = anchor.replace(tzinfo=timezone.utc)
    next_at = compute_next_run(anchor, interval)

    if failed:
        watch.consecutive_failures = (watch.consecutive_failures or 0) + 1
        max_fail = int(settings.INTENT_POLLER_MAX_CONSECUTIVE_FAILURES)
        if watch.consecutive_failures >= max_fail:
            watch.enabled = False
            watch.last_error = "auto_disabled"
            watch.next_poll_at = None
            _mirror_upsert(db, watch.id, watch.workspace_id, None, False)
            return
        base = _interval_delta(interval) / 4
        cap = timedelta(hours=24)
        backoff = min(base * (2 ** (watch.consecutive_failures - 1)), cap)
        next_at = max(next_at, _utcnow() + backoff)
    else:
        watch.consecutive_failures = 0

    watch.next_poll_at = next_at
    if watch.enabled:
        _enqueue_poll_if_absent(
            db, fire_key=scheduled_fire_key(watch.id, next_at),
            workspace_id=watch.workspace_id, watch_id=watch.id, next_run_at=next_at,
        )
        _mirror_upsert(db, watch.id, watch.workspace_id, next_at, True)


# ── per-source cost / billing (spec §7) ──────────────────────────────────────

_SOURCE_COST = {
    "funding": "POLLER_FUNDING_COST_USD",
    "hiring": "POLLER_HIRING_COST_USD",
    "feed": "POLLER_FEED_COST_USD",
}


def _debit_source(db, ws_id: str, source: str, fire_key: str) -> bool:
    """check_and_debit BEFORE a paid fetch. run_id = poll fire_key (idempotent
    across job retries of the SAME poll). Returns True to proceed, False on 402."""
    cost = float(getattr(settings, _SOURCE_COST.get(source, ""), 0.0) or 0.0)
    if cost <= 0:
        return True
    from apps.api.services.billing import service as billing

    try:
        billing.check_and_debit(
            db, ws_id, cost, run_id=f"{fire_key}:{source}", reason="poller_fetch",
        )
        return True
    except billing.InsufficientCreditsError:
        return False


# ── the handler ──────────────────────────────────────────────────────────────

async def handle_watch_poll(job_id: int, payload: dict) -> None:
    if not _enabled():
        return
    from apps.api.services.leadgen.store import use_pg_store

    workspace_id = payload.get("workspace_id")
    watch_id = payload.get("watch_id")
    fire_key = payload.get("fire_key") or f"job:{job_id}"
    poll_now = bool(payload.get("poll_now"))
    if not workspace_id or not watch_id:
        logger.warning("watch_poll missing workspace_id/watch_id: %s", payload)
        return
    if not use_pg_store():
        logger.critical("watch_poll requires the PG lead store (on_signal PG-only); skipping")
        return

    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.leadgen.store import get_lead_store
    from apps.api.services.workspace import manager as ws_manager

    started = time.monotonic()
    with workspace_scope(workspace_id):
        # ── load watch + budget under scope ──
        with SessionLocal() as db, db.begin():
            watch = (
                db.query(WatchSubscription)
                .filter(
                    WatchSubscription.id == watch_id,
                    WatchSubscription.workspace_id == workspace_id,
                )
                .first()
            )
            if watch is None or not watch.enabled:
                # gone/disabled → early-exit, no reschedule, drop mirror.
                mirror_delete(db, watch_id)
                return

            # budget (atomic). Over-budget → reschedule + exit.
            budget = int(settings.INTENT_POLLER_DAILY_POLL_BUDGET)
            n, n_pn = _debit_budget(db, workspace_id, poll_now=poll_now)
            if budget > 0 and n > budget:
                logger.info("poller budget exceeded ws=%s n=%d/%d — reschedule", workspace_id, n, budget)
                _reschedule_watch(db, watch, failed=False)
                return

            # snapshot the fields we need outside the txn
            kinds = _source_set(watch)
            backfill = bool(settings.INTENT_POLLER_BACKFILL)
            bootstrapped = bool((watch.cursor or {}).get("bootstrapped"))

        slug = ws_manager.workspace_slug(workspace_id) or ""
        store = get_lead_store(workspace_id, slug)

        any_failure = False
        total_emitted = total_dupe = 0

        # Resolve lead once (tenant-scoped).
        with SessionLocal() as db, db.begin():
            watch = db.query(WatchSubscription).filter(
                WatchSubscription.id == watch_id,
                WatchSubscription.workspace_id == workspace_id,
            ).first()
            lead_id, lead_err = _resolve_lead_id(db, watch)
            watch_snapshot = _snapshot(watch)

        if lead_err == "ambiguous_lead":
            with SessionLocal() as db, db.begin():
                watch = _load(db, watch_id, workspace_id)
                if watch:
                    watch.last_error = "ambiguous_lead"
                    watch.last_polled_at = _utcnow()
                    _reschedule_watch(db, watch, failed=False)
            return

        # ── per source: emit + advance cursor in ONE txn ──
        for src in kinds:
            ok = _poll_one_source(
                store, watch_id, workspace_id, src, fire_key,
                lead_id, backfill,
            )
            if ok is False:
                any_failure = True
            elif isinstance(ok, tuple):
                total_emitted += ok[0]
                total_dupe += ok[1]
                if len(ok) > 2 and ok[2]:
                    any_failure = True

        # ── finalize: set bootstrapped, last_polled_at, reschedule ──
        with SessionLocal() as db, db.begin():
            watch = _load(db, watch_id, workspace_id)
            if watch is None:
                mirror_delete(db, watch_id)
                return
            cur = dict(watch.cursor or {})
            cur["bootstrapped"] = True
            cur["attempt_count"] = int(cur.get("attempt_count") or 0) + 1
            watch.cursor = cur
            watch.last_polled_at = _utcnow()
            if not any_failure:
                watch.last_error = None
            _reschedule_watch(db, watch, failed=any_failure)

    logger.info(
        "watch_poll done watch=%s ws=%s emitted=%d dupe=%d failure=%s dur_ms=%d",
        watch_id, workspace_id, total_emitted, total_dupe, any_failure,
        int((time.monotonic() - started) * 1000),
    )


def _poll_one_source(store, watch_id, workspace_id, src, fire_key, lead_id, backfill):
    """Run one source in its OWN txn: billing → fetch → emit → advance cursor.

    Emission ordering (spec §8.3): signals are written first via
    ``store.add_signal`` (each committed in its own RLS-scoped txn, which fires
    on_signal), THEN this source's cursor sub-key is advanced and committed when
    ``db.begin()`` exits. A crash AFTER emit but BEFORE the cursor commit rolls
    back ONLY the cursor; the next poll re-detects the same items and the
    deterministic ``signals.id`` makes re-emit a no-op (no dropped first batch,
    no double fire — AC-24). The failed source's cursor is never advanced on a
    fetch failure, so it is retried next poll.

    Returns (emitted, dupe) on success, False on fetch failure, None when nothing
    to do (no lead / suppressed)."""
    with SessionLocal() as db, db.begin():
        watch = _load(db, watch_id, workspace_id)
        if watch is None:
            return None
        # billing gate (free sources no-op; job_change is keyless DDG → free)
        if src in ("feed", "job_change", "account_group"):
            bill_src = src
        else:
            bill_src = "funding" if src in ("funding", "exec") else "hiring"
        if not _debit_source(db, workspace_id, bill_src, fire_key):
            watch.last_error = "insufficient_credits"
            return None

        if src == "funding" or src == "exec":
            events, patch = sources.fetch_funding_and_exec(
                watch, want_funding=(src == "funding"), want_exec=(src == "exec"),
                backfill=backfill,
            )
        elif src == "hiring" or src == "tech":
            events, patch = sources.fetch_hiring_and_tech(
                watch, want_hiring=(src == "hiring"), want_tech=(src == "tech"),
                backfill=backfill,
            )
        elif src == "web_tech":
            # Website technographics path (selected over jobspy "tech" when
            # TECH_STACK_WEBSITE_FETCH_ENABLED) — needs the lead's website.
            website = None
            if lead_id:
                from apps.api.services.leadgen.orm_models import LeadRow
                row = db.query(LeadRow.website).filter(LeadRow.id == lead_id).first()
                website = row[0] if row else None
            events, patch = sources.fetch_web_tech(watch, website, backfill=backfill)
        elif src == "feed":
            events, patch = sources.fetch_feed(watch, backfill=backfill)
        elif src == "job_change":
            from apps.api.services.poller import job_change as jc

            contacts = jc.materialize_contacts(db, watch)
            events, patch = jc.fetch_job_changes(watch, contacts, backfill=backfill)
        elif src == "account_group":
            from apps.api.services.poller.account_group import fetch_account_group

            events, patch = fetch_account_group(watch, backfill=backfill)
        else:
            return None

        if events is None and patch is None:
            return False  # fetch failure → no cursor advance

        # apply resolved_cik side-channel
        if patch and "_resolved_cik" in patch:
            rc = patch.pop("_resolved_cik")
            if rc and not watch.resolved_cik:
                watch.resolved_cik = rc

        collector_failures = []
        if patch and "_collector_failures" in patch:
            collector_failures = list(patch.pop("_collector_failures") or [])
            if collector_failures:
                watch.last_error = str(collector_failures[0])[:255]

        emitted = dupe = 0
        # job_change events carry their OWN per-contact lead routing (ev.lead_id,
        # possibly none) — they emit regardless of a watch-level lead match.
        if events and (lead_id or src in {"job_change", "account_group"}):
            emitted, dupe = _emit_events(store, watch, lead_id, events)
        # advance cursor sub-key in the SAME txn (atomic emit+advance)
        if patch:
            cur = dict(watch.cursor or {})
            cur.update(patch)
            watch.cursor = cur
        return (emitted, dupe, bool(collector_failures))


def _source_set(watch) -> list:
    """Sources to fan in for a watch kind. company → funding+exec+hiring+tech.
    Each entry is one independent-txn source."""
    kind = watch.kind
    types = set(watch.signal_types or [])

    def wants(*sts):
        return (not types) or any(s in types for s in sts)

    # new_tech_adopted has TWO mutually-exclusive detectors: jobspy text mining
    # ("tech") by default, OR the SSRF-guarded website homepage path ("web_tech")
    # when TECH_STACK_WEBSITE_FETCH_ENABLED. Exactly one runs → no double-count.
    tech_src = "web_tech" if getattr(
        settings, "TECH_STACK_WEBSITE_FETCH_ENABLED", False
    ) else "tech"

    if kind == "funding":
        out = []
        if wants("company_funded"):
            out.append("funding")
        if wants("executive_hired"):
            out.append("exec")
        return out or ["funding"]
    if kind == "hiring":
        out = []
        if wants("hiring_surge"):
            out.append("hiring")
        if wants("new_tech_adopted"):
            out.append(tech_src)
        return out or ["hiring", tech_src]
    if kind == "feed":
        return ["feed"]
    if kind == "job_change":
        return ["job_change"]
    if kind == "account_group":
        return ["account_group"]
    if kind == "company":
        out = []
        if wants("company_funded"):
            out.append("funding")
        if wants("executive_hired"):
            out.append("exec")
        if wants("hiring_surge"):
            out.append("hiring")
        if wants("new_tech_adopted"):
            out.append(tech_src)
        return out or ["funding", "exec", "hiring", tech_src]
    return []


def _load(db, watch_id, workspace_id):
    return (
        db.query(WatchSubscription)
        .filter(
            WatchSubscription.id == watch_id,
            WatchSubscription.workspace_id == workspace_id,
        )
        .first()
    )


def _snapshot(watch) -> dict:
    return {
        "id": watch.id, "kind": watch.kind, "target": watch.target,
        "resolved_cik": watch.resolved_cik, "lead_id": watch.lead_id,
    }


# ── create/resume bootstrap (single watch) ───────────────────────────────────

def schedule_bootstrap_for_watch(db, watch) -> datetime:
    """Compute next_poll_at + enqueue the first aligned poll (create/resume)."""
    interval = watch.interval or settings.INTENT_POLLER_DEFAULT_INTERVAL
    anchor = watch.schedule_anchor or _utcnow()
    if anchor.tzinfo is None:
        anchor = anchor.replace(tzinfo=timezone.utc)
    next_at = compute_next_run(anchor, interval)
    watch.next_poll_at = next_at
    _enqueue_poll_if_absent(
        db, fire_key=scheduled_fire_key(watch.id, next_at),
        workspace_id=watch.workspace_id, watch_id=watch.id, next_run_at=next_at,
    )
    _mirror_upsert(db, watch.id, watch.workspace_id, next_at, watch.enabled)
    return next_at


# ── cold-start bootstrap from the non-RLS mirror (spec §2 / §6) ──────────────

def bootstrap_watch_schedules() -> int:
    """Cold-start: read the NON-RLS mirror (no workspace GUC), then per due watch
    re-enter workspace_scope to enqueue. Survives restarts; single-flight guarded."""
    if not _enabled():
        return 0
    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.leadgen.store import use_pg_store

    if not use_pg_store():
        logger.critical("intent poller is PG-only; bootstrap skipped on this backend")
        return 0

    now = _utcnow()
    enqueued = 0
    with SessionLocal() as db:
        due = db.query(WatchSchedule).filter(WatchSchedule.enabled.is_(True)).all()
        due_ids = [
            (r.watch_id, r.workspace_id) for r in due
            if r.next_poll_at is None or r.next_poll_at <= now
        ]
    for watch_id, workspace_id in due_ids:
        with workspace_scope(workspace_id):
            with SessionLocal() as db, db.begin():
                watch = _load(db, watch_id, workspace_id)
                if watch is None or not watch.enabled:
                    continue
                interval = watch.interval or settings.INTENT_POLLER_DEFAULT_INTERVAL
                anchor = watch.schedule_anchor or now
                if anchor.tzinfo is None:
                    anchor = anchor.replace(tzinfo=timezone.utc)
                next_at = compute_next_run(anchor, interval)
                if _enqueue_poll_if_absent(
                    db, fire_key=scheduled_fire_key(watch.id, next_at),
                    workspace_id=workspace_id, watch_id=watch.id, next_run_at=next_at,
                ):
                    watch.next_poll_at = next_at
                    _mirror_upsert(db, watch.id, workspace_id, next_at, True)
                    enqueued += 1
    logger.info("bootstrap_watch_schedules enqueued %d poll(s)", enqueued)
    return enqueued
