"""Automations ORM models (§3 of the trigger-engine spec).

Five tables on the single Base from apps.api.database:

  * ``triggers``                  — one row = one rule (RLS, ws-scoped)
  * ``trigger_runs``              — one per evaluation (RLS)
  * ``trigger_action_results``    — one per action-per-row; the idempotency ledger (RLS)
  * ``trigger_cap_reservations``  — atomic spend-cap reservations (RLS)
  * ``scheduled_triggers``        — non-RLS mirror for cold-start scheduler bootstrap

The four tenant tables carry ``workspace_id`` + a fail-closed RLS policy created in
the migration. ``scheduled_triggers`` is the ONE deliberate non-RLS mirror: it holds
only ids + timestamps (no lead/row content) so ``bootstrap_schedules`` can read it
with no workspace GUC set.
"""

import uuid

from sqlalchemy import (
    Column, String, Integer, Text, DateTime, JSON, Float, Boolean,
    UniqueConstraint, Index,
)
from sqlalchemy.sql import func

from apps.api.database import Base


def _uuid() -> str:
    return str(uuid.uuid4())


# Enumerations (validated at the API layer; stored as plain strings).
TRIGGER_TYPES = ("on_signal", "on_row_changed", "on_row_added", "on_schedule")
# v1 LOCKED SCOPE: only these three action types are accepted at rule-create.
# sequencer / send_email are deferred (rejected at create) until the legacy
# outreach.db store is RLS-hardened.
ACTION_TYPES_V1 = ("re_enrich", "push_crm", "webhook")
ACTION_TYPES_LEGACY = ("sequencer", "send_email")  # gated; rejected in v1
SCHEDULE_INTERVALS = ("hourly", "daily", "weekly")


class Trigger(Base):
    """A tenant-scoped automation rule (RLS, workspace-scoped)."""

    __tablename__ = "triggers"
    __table_args__ = (
        Index("ix_triggers_workspace_id", "workspace_id"),
        Index("ix_triggers_ws_type", "workspace_id", "trigger_type"),
        Index("ix_triggers_ws_enabled", "workspace_id", "enabled"),
        Index("ix_triggers_ws_next_run", "workspace_id", "trigger_type", "next_run_at"),
    )

    id = Column(String(36), primary_key=True, default=_uuid)
    workspace_id = Column(String(64), nullable=False)
    name = Column(String(200), nullable=False)
    enabled = Column(Boolean, nullable=False, default=True)
    trigger_type = Column(String(32), nullable=False)
    trigger_config = Column(JSON, nullable=False, default=dict)
    condition = Column(Text, default="")
    actions = Column(JSON, nullable=False, default=list)
    scope_workbook_ids = Column(JSON, default=list)
    stop_on_error = Column(Boolean, nullable=False, default=False)
    max_spend_usd_per_day = Column(Float, nullable=True)
    max_actions_per_day = Column(Integer, nullable=True)
    next_run_at = Column(DateTime(timezone=True), nullable=True)
    schedule_anchor = Column(DateTime(timezone=True), nullable=True)
    last_fired_at = Column(DateTime(timezone=True), nullable=True)
    created_by = Column(String(64), nullable=True)
    created_at = Column(DateTime(timezone=True), server_default=func.now())
    updated_at = Column(DateTime(timezone=True), server_default=func.now(), onupdate=func.now())

    def to_api(self) -> dict:
        return {
            "id": self.id,
            "workspace_id": self.workspace_id,
            "name": self.name,
            "enabled": bool(self.enabled),
            "trigger_type": self.trigger_type,
            "trigger_config": self.trigger_config or {},
            "condition": self.condition or "",
            "actions": self.actions or [],
            "scope_workbook_ids": self.scope_workbook_ids or [],
            "stop_on_error": bool(self.stop_on_error),
            "max_spend_usd_per_day": self.max_spend_usd_per_day,
            "max_actions_per_day": self.max_actions_per_day,
            "next_run_at": self.next_run_at.isoformat() if self.next_run_at else None,
            "schedule_anchor": self.schedule_anchor.isoformat() if self.schedule_anchor else None,
            "last_fired_at": self.last_fired_at.isoformat() if self.last_fired_at else None,
            "created_by": self.created_by,
            "created_at": self.created_at.isoformat() if self.created_at else None,
            "updated_at": self.updated_at.isoformat() if self.updated_at else None,
        }


class TriggerRun(Base):
    """One row per evaluation of a rule (RLS)."""

    __tablename__ = "trigger_runs"
    __table_args__ = (
        Index("ix_trigger_runs_ws", "workspace_id"),
        Index("ix_trigger_runs_ws_trigger_started", "workspace_id", "trigger_id", "started_at"),
    )

    id = Column(String(36), primary_key=True, default=_uuid)
    workspace_id = Column(String(64), nullable=False)
    trigger_id = Column(String(36), nullable=False)
    trigger_name_snapshot = Column(String(200), nullable=True)
    job_id = Column(Integer, nullable=True)
    status = Column(String(20), default="running")
    fire_source = Column(String(32), nullable=True)
    fire_key = Column(String(200), nullable=True)
    sweep_cursor = Column(String(200), nullable=True)
    matched_rows = Column(Integer, default=0)
    actions_attempted = Column(Integer, default=0)
    actions_succeeded = Column(Integer, default=0)
    actions_skipped = Column(Integer, default=0)
    actions_failed = Column(Integer, default=0)
    total_charged_usd = Column(Float, default=0.0)
    error = Column(Text, nullable=True)
    started_at = Column(DateTime(timezone=True), server_default=func.now())
    finished_at = Column(DateTime(timezone=True), nullable=True)

    def to_api(self) -> dict:
        return {
            "id": self.id,
            "trigger_id": self.trigger_id,
            "trigger_name_snapshot": self.trigger_name_snapshot,
            "job_id": self.job_id,
            "status": self.status,
            "fire_source": self.fire_source,
            "fire_key": self.fire_key,
            "matched_rows": self.matched_rows or 0,
            "actions_attempted": self.actions_attempted or 0,
            "actions_succeeded": self.actions_succeeded or 0,
            "actions_skipped": self.actions_skipped or 0,
            "actions_failed": self.actions_failed or 0,
            "total_charged_usd": round(self.total_charged_usd or 0.0, 4),
            "error": self.error,
            "started_at": self.started_at.isoformat() if self.started_at else None,
            "finished_at": self.finished_at.isoformat() if self.finished_at else None,
        }


class TriggerActionResult(Base):
    """One row per (run, row, action). Carries the idempotency key (RLS)."""

    __tablename__ = "trigger_action_results"
    __table_args__ = (
        UniqueConstraint("workspace_id", "idempotency_key", name="uq_trigger_action_idem"),
        Index("ix_tar_ws_trigger_created", "workspace_id", "trigger_id", "created_at"),
        Index("ix_tar_ws_created", "workspace_id", "created_at"),
    )

    id = Column(Integer, primary_key=True, autoincrement=True)
    workspace_id = Column(String(64), nullable=False)
    run_id = Column(String(36), nullable=False)
    trigger_id = Column(String(36), nullable=False)
    workbook_id = Column(String(36), nullable=False)
    row_id = Column(String(36), nullable=False)
    lead_id = Column(Integer, nullable=True)
    action_index = Column(Integer, nullable=False)
    action_type = Column(String(32), nullable=False)
    idempotency_key = Column(String(255), nullable=False)
    status = Column(String(20), default="in_flight")
    skip_reason = Column(String(40), nullable=True)
    charged_usd = Column(Float, default=0.0)
    result_summary = Column(Text, nullable=True)
    error = Column(Text, nullable=True)
    created_at = Column(DateTime(timezone=True), server_default=func.now())
    updated_at = Column(DateTime(timezone=True), server_default=func.now(), onupdate=func.now())

    def to_api(self) -> dict:
        return {
            "id": self.id,
            "run_id": self.run_id,
            "trigger_id": self.trigger_id,
            "workbook_id": self.workbook_id,
            "row_id": self.row_id,
            "lead_id": self.lead_id,
            "action_index": self.action_index,
            "action_type": self.action_type,
            "idempotency_key": self.idempotency_key,
            "status": self.status,
            "skip_reason": self.skip_reason,
            "charged_usd": round(self.charged_usd or 0.0, 4),
            "result_summary": self.result_summary,
            "error": self.error,
            "created_at": self.created_at.isoformat() if self.created_at else None,
        }


class TriggerCapReservation(Base):
    """Atomic spend-cap reservation (RLS). Reserve before debit, settle/release after."""

    __tablename__ = "trigger_cap_reservations"
    __table_args__ = (
        UniqueConstraint("workspace_id", "idempotency_key", name="uq_cap_reservation_idem"),
        Index("ix_capres_ws_trig_day", "workspace_id", "trigger_id", "day_utc", "state"),
        Index("ix_capres_ws_day", "workspace_id", "day_utc", "state"),
    )

    id = Column(Integer, primary_key=True, autoincrement=True)
    workspace_id = Column(String(64), nullable=False)
    trigger_id = Column(String(36), nullable=False)
    day_utc = Column(String(10), nullable=False)
    reserved_usd = Column(Float, nullable=False)
    reserved_actions = Column(Integer, nullable=False, default=1)
    idempotency_key = Column(String(255), nullable=False)
    state = Column(String(12), nullable=False, default="held")  # held / settled / released
    created_at = Column(DateTime(timezone=True), server_default=func.now())


class ScheduledTrigger(Base):
    """Non-RLS mirror of (workspace_id, trigger_id, next_run_at, enabled).

    DELIBERATELY NOT RLS: the scheduler cold-start bootstrap reads this with no
    workspace GUC set. It contains ONLY ids + a timestamp + an enabled flag — no
    lead/row content — so reading it across tenants leaks nothing of substance.
    Written transactionally on rule create / resume / pause / delete.
    """

    __tablename__ = "scheduled_triggers"
    __table_args__ = (
        Index("ix_scheduled_triggers_due", "enabled", "next_run_at"),
    )

    trigger_id = Column(String(36), primary_key=True)
    workspace_id = Column(String(64), nullable=False)
    next_run_at = Column(DateTime(timezone=True), nullable=True)
    enabled = Column(Boolean, nullable=False, default=True)
    updated_at = Column(DateTime(timezone=True), server_default=func.now(), onupdate=func.now())
