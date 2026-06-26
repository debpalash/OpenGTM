"""Per-workspace MCP write caps + idempotency — reuses the automations ledger.

Rather than build a parallel cap system, every MCP write reserves against the
SAME ``trigger_cap_reservations`` table and the SAME ``automations/caps.py``
reservation machinery the trigger engine uses (owner decision #6). This means:

  * MCP writes share the tenant's daily budget — they can never exceed (or be
    used to bypass) the automations caps, and any cost-incurring write is bounded
    by ``AUTOMATIONS_GLOBAL_DAILY_USD`` for free via ``caps.try_reserve``.
  * The reservation row's ``UniqueConstraint(workspace_id, idempotency_key)`` is
    the idempotency ledger: a retried write with the same key is detected as a
    replay and the underlying store mutation is skipped (no double-apply).

All MCP writes are modelled as one synthetic per-workspace "rule"
(``MCP_WRITE_TRIGGER_ID``) whose per-rule action cap is
``settings.MCP_MAX_WRITES_PER_DAY`` (0/None = unlimited). v1 write tools incur no
provider spend (``cost=0``), but routing through ``try_reserve`` keeps the spend
axis wired so a future cost-bearing MCP tool is capped without new plumbing.

The reservation table is RLS-scoped, so every call here runs inside
``workspace_scope`` and as the app DB role exactly like the trigger engine.
"""

from __future__ import annotations

import uuid
from dataclasses import dataclass
from typing import Optional

from apps.api.core.config import settings
from apps.api.core.tenancy import workspace_scope
from apps.api.services.automations import caps as auto_caps

# Synthetic, per-workspace "rule" id under which all MCP writes are counted.
MCP_WRITE_TRIGGER_ID = "__mcp_write__"


@dataclass
class WriteReservation:
    """Outcome of reserving one MCP write against the daily cap.

    * ``ok`` + ``replay=False``  → fresh reservation held; caller does the write
      then must ``settle`` (success) or ``release`` (failure).
    * ``ok=True`` + ``replay=True`` → an identical idempotency key already
      reserved; the write already happened — caller MUST NOT re-apply it.
    * ``ok=False`` → the daily write cap is exhausted; caller refuses + audits.
    """

    ok: bool
    replay: bool = False
    reason: Optional[str] = None
    idempotency_key: Optional[str] = None
    _res: Optional[auto_caps.Reservation] = None


class _MCPWriteRule:
    """Trigger-shaped object so ``caps.try_reserve`` enforces the MCP daily cap.

    ``try_reserve`` reads ``id`` / ``max_actions_per_day`` / ``max_spend_usd_per_day``
    off the rule. We expose the MCP write cap as the per-rule action cap and leave
    the per-rule spend cap unset (the workspace-global spend cap still applies to
    any ``cost > 0`` write via ``try_reserve``).
    """

    def __init__(self) -> None:
        self.id = MCP_WRITE_TRIGGER_ID
        cap = getattr(settings, "MCP_MAX_WRITES_PER_DAY", 0) or 0
        self.max_actions_per_day = int(cap) if cap and cap > 0 else None
        self.max_spend_usd_per_day = None


def reserve_write(workspace_id: str, idempotency_key: Optional[str], cost: float = 0.0) -> WriteReservation:
    """Reserve one MCP write for ``workspace_id`` (idempotent on ``idempotency_key``).

    A missing key gets a fresh random one so each distinct call still counts
    against the daily cap (only an explicit, repeated key dedupes).
    """
    idem = idempotency_key or f"mcpw_{uuid.uuid4().hex}"

    with workspace_scope(workspace_id):
        from apps.api.database import SessionLocal
        from apps.api.services.automations.models import TriggerCapReservation

        with SessionLocal() as db, db.begin():
            existing = (
                db.query(TriggerCapReservation)
                .filter(
                    TriggerCapReservation.workspace_id == workspace_id,
                    TriggerCapReservation.idempotency_key == idem,
                )
                .first()
            )
            if existing is not None and existing.state in ("held", "settled"):
                # Same logical write already reserved (and applied) — replay.
                return WriteReservation(ok=True, replay=True, idempotency_key=idem)

            res = auto_caps.try_reserve(db, _MCPWriteRule(), workspace_id, cost, idem)
            if not res.ok:
                return WriteReservation(ok=False, reason=res.reason or "cap", idempotency_key=idem)
            # Commit the held row on context exit so the cap is reserved even if
            # the actual store write runs in a later transaction.
            return WriteReservation(ok=True, idempotency_key=idem, _res=res)


def settle_write(workspace_id: str, reservation: WriteReservation) -> None:
    """Mark a held reservation as settled after a successful write."""
    if reservation is None or not reservation.ok or reservation._res is None:
        return
    with workspace_scope(workspace_id):
        from apps.api.database import SessionLocal

        with SessionLocal() as db, db.begin():
            auto_caps.settle(db, reservation._res)


def release_write(workspace_id: str, reservation: WriteReservation) -> None:
    """Release a held reservation when the write failed (frees the cap slot)."""
    if reservation is None or not reservation.ok or reservation._res is None:
        return
    with workspace_scope(workspace_id):
        from apps.api.database import SessionLocal

        with SessionLocal() as db, db.begin():
            auto_caps.release(db, reservation._res)
