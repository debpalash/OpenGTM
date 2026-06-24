"""Credit-ledger ORM models (WI-9).

Two tables on the main ORM ``Base``:

  * ``workspace_credits`` — one row per workspace holding the current balance.
    The balance is a denormalized running total of the entries below; it is the
    value enforced at the pre-run chokepoint (cheap to read, atomically debited).

  * ``credit_ledger_entries`` — append-only journal. Every balance change (debit
    for a run, credit for a Stripe top-up, manual grant) is one immutable row:
    {amount, reason, run_id, created_at}. Debits are negative, credits positive.

Idempotency: a debit carries a unique ``idempotency_key`` (e.g. ``run:<run_id>``)
so retrying the same run cannot double-charge — the second insert hits the unique
constraint and is treated as a no-op.
"""

from sqlalchemy import (
    Column, String, Integer, Float, DateTime, UniqueConstraint, Index,
)
from sqlalchemy.sql import func

from apps.api.database import Base


class WorkspaceCredit(Base):
    """Per-workspace credit balance (USD)."""

    __tablename__ = "workspace_credits"

    workspace_id = Column(String(64), primary_key=True)
    # Current spendable balance in USD. Kept in sync with the sum of ledger
    # entries; this is the column the pre-run check reads and debits atomically.
    balance_usd = Column(Float, nullable=False, default=0.0)
    created_at = Column(DateTime, server_default=func.now())
    updated_at = Column(DateTime, server_default=func.now(), onupdate=func.now())

    def to_api(self) -> dict:
        return {
            "workspace_id": self.workspace_id,
            "balance_usd": round(self.balance_usd or 0.0, 4),
            "updated_at": self.updated_at.isoformat() if self.updated_at else None,
        }


class CreditLedgerEntry(Base):
    """Append-only ledger entry. Debits negative, credits positive."""

    __tablename__ = "credit_ledger_entries"
    __table_args__ = (
        # Idempotency: at most one entry per key (e.g. one debit per run, one
        # credit per Stripe checkout session). NULL keys are unconstrained.
        UniqueConstraint("idempotency_key", name="uq_ledger_idempotency_key"),
        Index("ix_ledger_workspace_created", "workspace_id", "created_at"),
    )

    id = Column(Integer, primary_key=True, autoincrement=True)
    workspace_id = Column(String(64), nullable=False, index=True)
    # Signed USD: debit < 0 (run spend), credit > 0 (top-up / grant).
    amount_usd = Column(Float, nullable=False)
    # Human/machine reason: "run_debit", "stripe_topup", "manual_grant", ...
    reason = Column(String(64), nullable=False)
    # The workbook run this entry is attributed to (debits); NULL for top-ups.
    run_id = Column(String(128), nullable=True, index=True)
    # Dedupe key making the write idempotent (e.g. "run:<run_id>",
    # "stripe:<session_id>"). NULL allowed for unconstrained manual entries.
    idempotency_key = Column(String(160), nullable=True)
    created_at = Column(DateTime, server_default=func.now())

    def to_api(self) -> dict:
        return {
            "id": self.id,
            "workspace_id": self.workspace_id,
            "amount_usd": round(self.amount_usd or 0.0, 4),
            "reason": self.reason,
            "run_id": self.run_id,
            "created_at": self.created_at.isoformat() if self.created_at else None,
        }
