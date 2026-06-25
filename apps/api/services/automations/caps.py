"""Atomic spend/action caps via the reservation table (§3.3a / §7).

Concurrent ``trigger_eval`` jobs must not both pass a near-full cap. We reserve
BEFORE the debit and settle/release AFTER, computing committed+held spend under a
row lock so the check-and-increment is atomic per (workspace, day).

Three cap axes are enforced together:
  * per-rule daily spend   (``trigger.max_spend_usd_per_day``)
  * per-rule daily actions (``trigger.max_actions_per_day``)
  * workspace-global daily spend (``settings.AUTOMATIONS_GLOBAL_DAILY_USD``)

The day bucket is an explicit ``YYYY-MM-DD`` in UTC (closes the naive-tz/DST
cap-window edge).
"""

from __future__ import annotations

import logging
from dataclasses import dataclass
from datetime import datetime, timezone
from typing import Optional

from sqlalchemy import text
from sqlalchemy.orm import Session

from apps.api.core.config import settings
from apps.api.services.automations.models import TriggerCapReservation

logger = logging.getLogger("automations.caps")


def day_utc(now: Optional[datetime] = None) -> str:
    now = now or datetime.now(timezone.utc)
    return now.astimezone(timezone.utc).strftime("%Y-%m-%d")


@dataclass
class Reservation:
    ok: bool
    reason: Optional[str] = None     # "cap" when refused
    id: Optional[int] = None         # reservation row id (when ok and cost>0)
    idempotency_key: Optional[str] = None


def _is_pg(db: Session) -> bool:
    return db.bind is not None and db.bind.dialect.name == "postgresql"


def _held_committed_spend(db: Session, ws_id: str, day: str, trigger_id: Optional[str]) -> float:
    """Sum reserved_usd of held+settled reservations for the day (optionally per-rule)."""
    q = (
        db.query(TriggerCapReservation)
        .filter(
            TriggerCapReservation.workspace_id == ws_id,
            TriggerCapReservation.day_utc == day,
            TriggerCapReservation.state.in_(("held", "settled")),
        )
    )
    if trigger_id is not None:
        q = q.filter(TriggerCapReservation.trigger_id == trigger_id)
    return round(sum((r.reserved_usd or 0.0) for r in q.all()), 6)


def _held_committed_actions(db: Session, ws_id: str, day: str, trigger_id: str) -> int:
    q = (
        db.query(TriggerCapReservation)
        .filter(
            TriggerCapReservation.workspace_id == ws_id,
            TriggerCapReservation.trigger_id == trigger_id,
            TriggerCapReservation.day_utc == day,
            TriggerCapReservation.state.in_(("held", "settled")),
        )
    )
    return sum(int(r.reserved_actions or 0) for r in q.all())


def actions_today(db: Session, trigger_id: str, ws_id: str, day: Optional[str] = None) -> int:
    """Held+settled action count for a rule today (observability/tests)."""
    return _held_committed_actions(db, ws_id, day or day_utc(), trigger_id)


def try_reserve(db: Session, trigger, ws_id: str, cost: float, idem: str) -> Reservation:
    """Atomically reserve one action (and ``cost`` USD) against all cap axes.

    Returns ``Reservation(ok=True, id=...)`` on success (a ``held`` row is
    inserted), or ``Reservation(ok=False, reason="cap")`` when any cap would be
    exceeded. Idempotent: a retry with the same ``idem`` returns the existing
    reservation instead of double-counting.
    """
    day = day_utc()

    # Idempotency: a prior attempt for this exact action already reserved.
    existing = (
        db.query(TriggerCapReservation)
        .filter(
            TriggerCapReservation.workspace_id == ws_id,
            TriggerCapReservation.idempotency_key == idem,
        )
        .first()
    )
    if existing is not None:
        if existing.state == "released":
            return Reservation(ok=False, reason="cap", idempotency_key=idem)
        return Reservation(ok=True, id=existing.id, idempotency_key=idem)

    # Serialize concurrent reservers on PG by locking the day-bucket rows.
    if _is_pg(db):
        try:
            db.execute(
                text(
                    "SELECT id FROM trigger_cap_reservations "
                    "WHERE workspace_id = :ws AND day_utc = :day FOR UPDATE"
                ),
                {"ws": ws_id, "day": day},
            )
        except Exception as e:  # pragma: no cover - defensive
            logger.warning("cap row-lock failed (continuing): %s", e)

    cost = round(max(0.0, float(cost or 0.0)), 6)

    # per-rule action cap
    max_actions = getattr(trigger, "max_actions_per_day", None)
    if max_actions is not None:
        used = _held_committed_actions(db, ws_id, day, trigger.id)
        if used + 1 > int(max_actions):
            return Reservation(ok=False, reason="cap")

    # per-rule spend cap
    max_spend = getattr(trigger, "max_spend_usd_per_day", None)
    if max_spend is not None and cost > 0:
        used_spend = _held_committed_spend(db, ws_id, day, trigger.id)
        if round(used_spend + cost, 6) > float(max_spend):
            return Reservation(ok=False, reason="cap")

    # workspace-global spend cap
    global_cap = float(getattr(settings, "AUTOMATIONS_GLOBAL_DAILY_USD", 0.0) or 0.0)
    if global_cap > 0 and cost > 0:
        used_global = _held_committed_spend(db, ws_id, day, trigger_id=None)
        if round(used_global + cost, 6) > global_cap:
            return Reservation(ok=False, reason="cap")

    res = TriggerCapReservation(
        workspace_id=ws_id,
        trigger_id=trigger.id,
        day_utc=day,
        reserved_usd=cost,
        reserved_actions=1,
        idempotency_key=idem,
        state="held",
    )
    db.add(res)
    db.flush()
    return Reservation(ok=True, id=res.id, idempotency_key=idem)


def settle(db: Session, reservation: Reservation) -> None:
    if not reservation or not reservation.ok or reservation.id is None:
        return
    row = db.query(TriggerCapReservation).filter(TriggerCapReservation.id == reservation.id).first()
    if row is not None and row.state == "held":
        row.state = "settled"
        db.flush()


def release(db: Session, reservation: Reservation) -> None:
    if not reservation or not reservation.ok or reservation.id is None:
        return
    row = db.query(TriggerCapReservation).filter(TriggerCapReservation.id == reservation.id).first()
    if row is not None and row.state == "held":
        row.state = "released"
        db.flush()
