"""Credit-ledger enforcement service (WI-9).

Pure(ish) functions operating on a SQLAlchemy session — the routers and the
pre-run chokepoint call these. The only HTTP concern leaked here is the
``InsufficientCreditsError`` carrying a 402, which callers translate.

Design notes
------------
* **Feature flag.** Everything keys off ``settings.BILLING_ENABLED`` (default
  False). When disabled, :func:`check_and_debit` is a no-op that never blocks a
  run, so self-host deployments are unaffected.

* **Non-BYOK only.** :func:`projected_platform_cost` walks the same provider
  economics the spend preview uses (``vendor_catalog``) and counts ONLY paid,
  non-BYOK providers. BYOK providers (user's own key) cost the platform nothing
  and are never debited.

* **Atomic + idempotent debit.** A debit (a) inserts an append-only ledger entry
  with a unique ``idempotency_key`` (``run:<run_id>``) and (b) decrements the
  balance row, in one transaction. A retry of the same run hits the unique
  constraint → we roll back and return the existing charge unchanged (no double
  charge). The balance read+decrement uses a row lock (``with_for_update``) on
  Postgres; SQLite serializes writers via its busy lock.
"""

from __future__ import annotations

import logging
from dataclasses import dataclass
from typing import Dict, List, Optional

from sqlalchemy.exc import IntegrityError
from sqlalchemy.orm import Session

from apps.api.core.config import settings
from apps.api.services.billing.models import WorkspaceCredit, CreditLedgerEntry

logger = logging.getLogger("billing.service")


class InsufficientCreditsError(Exception):
    """Raised when a workspace lacks credits to cover a run's platform cost.

    Routers translate this to HTTP 402 (Payment Required).
    """

    def __init__(self, balance_usd: float, required_usd: float):
        self.balance_usd = round(balance_usd, 4)
        self.required_usd = round(required_usd, 4)
        super().__init__(
            f"Insufficient credits: balance ${self.balance_usd:.4f} < "
            f"required ${self.required_usd:.4f}. Top up to run this enrichment."
        )


@dataclass
class DebitResult:
    charged_usd: float          # what we actually debited this call (0 if no-op)
    balance_usd: float          # balance AFTER this call
    idempotent_replay: bool = False   # True when the run was already charged


def billing_enabled() -> bool:
    return bool(getattr(settings, "BILLING_ENABLED", False))


# ── Cost projection (non-BYOK only) ─────────────────────────────────────────

def projected_platform_cost(
    num_rows: int, providers_by_column: Dict[str, List[str]]
) -> float:
    """Worst-case platform-billed spend for a run, in USD.

    Mirrors ``vendor_catalog.estimate_run_cost`` (worst case = every paid
    provider in a column's chain tried per row) but counts ONLY non-BYOK
    providers — BYOK spend is on the user's own key and is never debited.
    """
    from apps.api.services.workbook import vendor_catalog as vc

    total = 0.0
    for providers in providers_by_column.values():
        for p in providers:
            if vc.is_paid(p) and not vc.is_byok(p):
                total += vc.base_cost(p) * max(0, int(num_rows))
    return round(total, 4)


# ── Balance helpers ─────────────────────────────────────────────────────────

def _get_or_create_balance(db: Session, workspace_id: str, lock: bool = False) -> WorkspaceCredit:
    q = db.query(WorkspaceCredit).filter(WorkspaceCredit.workspace_id == workspace_id)
    if lock:
        # Row lock on Postgres; harmless/ignored on SQLite (single writer).
        try:
            q = q.with_for_update()
        except Exception:  # pragma: no cover - dialect without FOR UPDATE
            pass
    row = q.first()
    if row is None:
        row = WorkspaceCredit(workspace_id=workspace_id, balance_usd=0.0)
        db.add(row)
        db.flush()
    return row


def get_balance(db: Session, workspace_id: str) -> float:
    row = (
        db.query(WorkspaceCredit)
        .filter(WorkspaceCredit.workspace_id == workspace_id)
        .first()
    )
    return round(row.balance_usd, 4) if row else 0.0


def recent_entries(db: Session, workspace_id: str, limit: int = 50) -> List[dict]:
    rows = (
        db.query(CreditLedgerEntry)
        .filter(CreditLedgerEntry.workspace_id == workspace_id)
        .order_by(CreditLedgerEntry.id.desc())
        .limit(max(1, min(int(limit), 500)))
        .all()
    )
    return [r.to_api() for r in rows]


# ── Credit (top-up / grant) ─────────────────────────────────────────────────

def credit(
    db: Session,
    workspace_id: str,
    amount_usd: float,
    reason: str = "manual_grant",
    idempotency_key: Optional[str] = None,
) -> DebitResult:
    """Add credit to a workspace. Idempotent when ``idempotency_key`` is given
    (e.g. a Stripe checkout session id) — a duplicate event is a no-op."""
    amount_usd = float(amount_usd)
    if amount_usd <= 0:
        raise ValueError("credit amount must be positive")

    if idempotency_key:
        existing = (
            db.query(CreditLedgerEntry)
            .filter(CreditLedgerEntry.idempotency_key == idempotency_key)
            .first()
        )
        if existing is not None:
            bal = _get_or_create_balance(db, workspace_id)
            return DebitResult(charged_usd=0.0, balance_usd=round(bal.balance_usd, 4), idempotent_replay=True)

    bal = _get_or_create_balance(db, workspace_id, lock=True)
    entry = CreditLedgerEntry(
        workspace_id=workspace_id,
        amount_usd=amount_usd,
        reason=reason,
        run_id=None,
        idempotency_key=idempotency_key,
    )
    db.add(entry)
    bal.balance_usd = round((bal.balance_usd or 0.0) + amount_usd, 6)
    try:
        db.commit()
    except IntegrityError:
        # Concurrent duplicate credit (same idempotency_key) — already applied.
        db.rollback()
        bal = _get_or_create_balance(db, workspace_id)
        db.commit()
        return DebitResult(charged_usd=0.0, balance_usd=round(bal.balance_usd, 4), idempotent_replay=True)
    return DebitResult(charged_usd=amount_usd, balance_usd=round(bal.balance_usd, 4))


# ── Debit (pre-run enforcement) ─────────────────────────────────────────────

def check_and_debit(
    db: Session,
    workspace_id: str,
    projected_cost_usd: float,
    run_id: str,
    reason: str = "run_debit",
) -> DebitResult:
    """Enforce billing at the pre-run chokepoint.

    * Billing disabled → no-op, never blocks (returns charged 0).
    * ``projected_cost_usd <= 0`` (all-BYOK / all-free run) → no-op.
    * Already charged for this ``run_id`` → idempotent no-op (retry-safe).
    * Insufficient balance → :class:`InsufficientCreditsError` (HTTP 402).
    * Otherwise → atomically write the debit entry and decrement the balance.
    """
    if not billing_enabled():
        return DebitResult(charged_usd=0.0, balance_usd=get_balance(db, workspace_id))

    cost = round(float(projected_cost_usd), 4)
    if cost <= 0:
        return DebitResult(charged_usd=0.0, balance_usd=get_balance(db, workspace_id))

    idem = f"run:{run_id}"

    # Idempotency fast-path: this run was already charged (retry) → no double-charge.
    existing = (
        db.query(CreditLedgerEntry)
        .filter(CreditLedgerEntry.idempotency_key == idem)
        .first()
    )
    if existing is not None:
        bal = _get_or_create_balance(db, workspace_id)
        return DebitResult(
            charged_usd=0.0, balance_usd=round(bal.balance_usd, 4), idempotent_replay=True
        )

    bal = _get_or_create_balance(db, workspace_id, lock=True)
    current = bal.balance_usd or 0.0
    if current < cost:
        db.rollback()
        raise InsufficientCreditsError(balance_usd=current, required_usd=cost)

    entry = CreditLedgerEntry(
        workspace_id=workspace_id,
        amount_usd=-cost,
        reason=reason,
        run_id=run_id,
        idempotency_key=idem,
    )
    db.add(entry)
    bal.balance_usd = round(current - cost, 6)
    try:
        db.commit()
    except IntegrityError:
        # A concurrent request charged this same run first — treat as replay.
        db.rollback()
        bal = _get_or_create_balance(db, workspace_id)
        return DebitResult(
            charged_usd=0.0, balance_usd=round(bal.balance_usd, 4), idempotent_replay=True
        )
    return DebitResult(charged_usd=cost, balance_usd=round(bal.balance_usd, 4))
