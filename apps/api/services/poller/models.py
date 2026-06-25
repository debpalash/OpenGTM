"""Intent-poller ORM models — the RLS-hardened tenant tables (spec §3).

Three tables on the shared ``Base`` (so ``create_all`` on SQLite/tests stays
consistent; RLS + GRANTs are Postgres-only and live in the Alembic migration
``<rev>_intent_poller``):

  * :class:`WatchSubscription` — RLS tenant table. One watch = one ``kind``
    (funding | hiring | feed | company). Carries the per-source cursor JSON.
  * :class:`PollBudgetLedger` — RLS tenant table. Atomic per-(ws, UTC-day) poll
    counter (``INSERT ... ON CONFLICT DO UPDATE SET n=n+1 RETURNING n``).
  * :class:`WatchSchedule` — NON-RLS mirror (ids/ts only), byte-for-byte from
    ``outreach_schedules`` / ``scheduled_triggers``. Drives cold-start bootstrap
    read WITHOUT a workspace GUC.
"""

from __future__ import annotations

from sqlalchemy import (
    Boolean,
    Column,
    Date,
    DateTime,
    Index,
    Integer,
    JSON,
    String,
    UniqueConstraint,
    func,
)

from apps.api.database import Base


# Versioned dedup-key scheme (spec §8). Frozen; bump only with a migration.
KEY_SCHEMA_VERSION = 1

WATCH_KINDS = ("funding", "hiring", "feed", "company")
WATCH_INTERVALS = ("hourly", "daily", "weekly")


def _empty_cursor() -> dict:
    return {"bootstrapped": False}


class WatchSubscription(Base):
    """A per-company/feed watch subscription (RLS tenant table, spec §3.1)."""

    __tablename__ = "watch_subscriptions"
    __table_args__ = (
        UniqueConstraint("workspace_id", "kind", "target", name="uq_watch_ws_kind_target"),
        Index("ix_watch_ws_enabled", "workspace_id", "enabled"),
        Index("ix_watch_ws_kind", "workspace_id", "kind"),
    )

    # Server-side uuid4, globally unique, never client-supplied.
    id = Column(String(36), primary_key=True)
    workspace_id = Column(String(64), nullable=False)
    kind = Column(String(20), nullable=False)
    target = Column(String, nullable=False)
    # Cached canonical CIK once resolved (freezes funding/exec dedup key against
    # display-name edits — CIK supremacy, spec §8.1).
    resolved_cik = Column(String(10), nullable=True)
    # Pinned lead match (routing only; NOT part of signals.id).
    lead_id = Column(Integer, nullable=True)
    # Emittable signal_type subset (aligns with on_signal trigger_config).
    signal_types = Column(JSON, nullable=True)
    interval = Column(String(10), nullable=True, server_default="daily")
    schedule_anchor = Column(DateTime(timezone=True), nullable=True)
    enabled = Column(Boolean, nullable=False, server_default="true", default=True)
    next_poll_at = Column(DateTime(timezone=True), nullable=True)
    # Per-source watermark (spec §8.3).
    cursor = Column(JSON, nullable=True, default=_empty_cursor)
    last_polled_at = Column(DateTime(timezone=True), nullable=True)
    last_error = Column(String, nullable=True)
    consecutive_failures = Column(Integer, nullable=True, server_default="0")
    created_at = Column(DateTime(timezone=True), server_default=func.now(), nullable=True)
    updated_at = Column(DateTime(timezone=True), server_default=func.now(), nullable=True)


class PollBudgetLedger(Base):
    """Atomic per-(ws, UTC-day) poll budget (RLS tenant table, spec §3.3)."""

    __tablename__ = "poll_budget_ledger"

    workspace_id = Column(String(64), primary_key=True)
    day = Column(Date, primary_key=True)
    n = Column(Integer, nullable=False, server_default="0")
    # Separate poll-now counter (manual poll-now quota, spec locked decision 3).
    n_poll_now = Column(Integer, nullable=False, server_default="0")


class WatchSchedule(Base):
    """NON-RLS mirror that drives cold-start bootstrap (spec §3.2)."""

    __tablename__ = "watch_schedules"
    __table_args__ = (
        Index("ix_watch_sched_due", "enabled", "next_poll_at"),
    )

    watch_id = Column(String(36), primary_key=True)
    workspace_id = Column(String(64), nullable=False)
    next_poll_at = Column(DateTime(timezone=True), nullable=True)
    enabled = Column(Boolean, nullable=False, server_default="false")
    updated_at = Column(DateTime(timezone=True), server_default=func.now(), nullable=True)
