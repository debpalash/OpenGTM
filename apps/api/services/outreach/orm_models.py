"""Outreach ORM models — the RLS-hardened tenant tables (spec §3).

Mirrors :mod:`apps.api.services.leadgen.orm_models` + the automations recipe.
Four RLS-protected tenant tables (every row carries a NOT NULL ``workspace_id``
as the first column + composite ``(workspace_id, …)`` indexes), plus a single
NON-RLS mirror table (:class:`OutreachSchedule`) modelled on
``scheduled_triggers`` that drives the autonomous ticker.

The RLS policies + GRANTs live in the Alembic migration (``<rev>_outreach_rls``);
this module only describes the table shapes so ``Base.metadata`` is complete for
``create_all`` on SQLite / tests.
"""

from __future__ import annotations

from sqlalchemy import (
    Boolean,
    Column,
    DateTime,
    Float,
    Index,
    Integer,
    JSON,
    String,
    Text,
    UniqueConstraint,
    func,
)

from apps.api.database import Base


class OutreachDraft(Base):
    """A persisted, evidence-grounded message that has not been sent.

    Drafts are deliberately separate from sequences, enrollments, and sends.
    Creating one cannot enqueue SMTP work, and there is no draft-to-send API.
    """

    __tablename__ = "outreach_drafts"
    __table_args__ = (
        UniqueConstraint(
            "workspace_id", "action_idempotency_key", name="uq_outreach_draft_ws_action"
        ),
        Index("ix_outreach_draft_ws_created", "workspace_id", "created_at"),
        Index("ix_outreach_draft_ws_person", "workspace_id", "person_id"),
    )

    id = Column(String(36), primary_key=True)
    workspace_id = Column(String(64), nullable=False)
    action_idempotency_key = Column(String(255), nullable=False)
    conversation_id = Column(String(64), nullable=False)
    source_action_id = Column(String(255), nullable=False)
    person_id = Column(String(80), nullable=False)
    person_name = Column(String(200), nullable=False)
    company = Column(String(200), nullable=False)
    title = Column(String(240), default="")
    to_email = Column(String(320), nullable=False)
    contact_status = Column(String(20), nullable=False)
    risky_approved = Column(Boolean, nullable=False, default=False, server_default="false")
    generic_inbox = Column(Boolean, nullable=False, default=False, server_default="false")
    is_role_address = Column(Boolean, nullable=False, default=False, server_default="false")
    subject = Column(String(300), nullable=False)
    body_text = Column(Text, nullable=False)
    sentence_evidence = Column(JSON, nullable=False, default=list)
    source_snapshot = Column(JSON, nullable=False, default=dict)
    state = Column(String(20), nullable=False, default="draft", server_default="draft")
    sent_at = Column(DateTime(timezone=True), nullable=True)
    created_at = Column(DateTime(timezone=True), server_default=func.now())
    updated_at = Column(DateTime(timezone=True), server_default=func.now(), onupdate=func.now())


class OutreachSequence(Base):
    """A named, ordered outreach sequence (RLS). Replaces legacy ``sequences``."""

    __tablename__ = "outreach_sequences"
    __table_args__ = (
        Index("ix_outreach_seq_ws", "workspace_id"),
        Index("ix_outreach_seq_ws_status", "workspace_id", "status"),
    )

    id = Column(String(36), primary_key=True)  # uuid
    workspace_id = Column(String(64), nullable=False)
    name = Column(String(200), nullable=False)
    description = Column(Text, default="")
    steps = Column(JSON, nullable=False, default=list)
    status = Column(String(20), default="draft")  # draft/active/paused/completed
    daily_limit = Column(Integer, default=50)
    send_window_start = Column(Integer, default=9)  # hour 0-23
    send_window_end = Column(Integer, default=18)  # hour 0-23
    send_window_tz = Column(String(40), default="UTC")  # IANA tz name
    consent_basis = Column(String(40), nullable=False, default="")  # lawful-basis attestation
    # Circuit-breaker bookkeeping (LOCKED SCOPE decision 2).
    bounce_count = Column(Integer, default=0)
    complaint_count = Column(Integer, default=0)
    auto_paused = Column(Boolean, default=False)
    created_at = Column(DateTime(timezone=True), server_default=func.now())
    updated_at = Column(DateTime(timezone=True), server_default=func.now(), onupdate=func.now())


class OutreachEnrollment(Base):
    """A lead enrolled in a sequence (RLS). Replaces legacy ``sequence_leads``."""

    __tablename__ = "outreach_enrollments"
    __table_args__ = (
        UniqueConstraint(
            "workspace_id", "sequence_id", "lead_id", name="uq_enroll_ws_seq_lead"
        ),
        Index("ix_enroll_ws_seq_status", "workspace_id", "sequence_id", "status"),
        Index("ix_enroll_ws_due", "workspace_id", "status", "next_send_at"),
    )

    id = Column(Integer, primary_key=True, autoincrement=True)
    workspace_id = Column(String(64), nullable=False)
    sequence_id = Column(String(36), nullable=False)
    lead_id = Column(Integer, nullable=False)
    to_email_snapshot = Column(String, nullable=False)  # normalized email at enroll
    consent_source = Column(String(120), default="")
    consent_at = Column(DateTime(timezone=True), nullable=True)
    current_step = Column(Integer, default=0)
    status = Column(String(20), default="pending")  # StepStatus
    next_send_at = Column(DateTime(timezone=True), nullable=True)
    sent_count = Column(Integer, default=0)
    soft_bounce_count = Column(Integer, default=0)
    last_sent_at = Column(DateTime(timezone=True), nullable=True)
    error = Column(Text, default="")


class OutreachSend(Base):
    """The send log + at-most-once idempotency ledger (RLS). Replaces ``send_log``."""

    __tablename__ = "outreach_sends"
    __table_args__ = (
        UniqueConstraint(
            "workspace_id", "idempotency_key", name="uq_outreach_send_idem"
        ),
        Index("ix_send_ws_seq_created", "workspace_id", "sequence_id", "created_at"),
        Index("ix_send_ws_status", "workspace_id", "status"),
        Index("ix_send_ws_msgid", "workspace_id", "message_id"),
        Index("ix_send_ws_sent_at", "workspace_id", "sent_at"),
    )

    id = Column(Integer, primary_key=True, autoincrement=True)
    workspace_id = Column(String(64), nullable=False)
    sequence_id = Column(String(36), nullable=True)
    enrollment_id = Column(Integer, nullable=True)
    lead_id = Column(Integer, nullable=True)
    step_number = Column(Integer, default=0)
    to_email = Column(String, nullable=False)
    subject = Column(String, default="")
    status = Column(String(20), default="in_flight")  # in_flight/sent/failed/bounced/skipped
    skip_reason = Column(String(40), nullable=True)
    message_id = Column(String, default="")
    idempotency_key = Column(String(255), nullable=False)
    charged_usd = Column(Float, default=0.0)
    migrated = Column(Boolean, default=False)
    error = Column(Text, default="")
    sent_at = Column(DateTime(timezone=True), nullable=True)
    opened_at = Column(DateTime(timezone=True), nullable=True)
    replied_at = Column(DateTime(timezone=True), nullable=True)
    bounced_at = Column(DateTime(timezone=True), nullable=True)
    created_at = Column(DateTime(timezone=True), server_default=func.now())
    updated_at = Column(DateTime(timezone=True), server_default=func.now(), onupdate=func.now())


class OutreachSuppression(Base):
    """Per-workspace suppression list (RLS). NEW."""

    __tablename__ = "outreach_suppressions"
    __table_args__ = (
        UniqueConstraint("workspace_id", "email", name="uq_suppress_ws_email"),
        Index("ix_suppress_ws_email", "workspace_id", "email"),
    )

    id = Column(Integer, primary_key=True, autoincrement=True)
    workspace_id = Column(String(64), nullable=False)
    email = Column(String, nullable=False)  # canonical normalized form
    reason = Column(String(20), default="manual")  # unsubscribe/bounce/complaint/manual
    source = Column(String(40), default="")  # sequence_id / trigger_id / 'webhook'
    locked = Column(Boolean, default=False)  # unsubscribe/complaint → cannot remove via API
    created_at = Column(DateTime(timezone=True), server_default=func.now())


class OutreachInboundMessage(Base):
    """Inbound DSN/complaint dedup ledger (RLS). NEW (bounce-ingestion spec).

    One row per inbound message we have *seen* in a workspace mailbox. The insert
    is the idempotency gate for ``apply_bounce`` — the breaker/suppression effect
    runs only when this row is newly created, so a re-fetch (retry / restart /
    mailbox UID churn) never double-counts. Two unique gates:

      * ``(workspace_id, uidvalidity, imap_uid)`` — IMAP server identity.
      * ``(workspace_id, source_message_id)`` — survives UID churn (the inbound
        message's own Message-ID; the webhook path reuses this with the event's
        message_id so a webhook replay is also a no-op).

    Stores only the recipient + a bounded diagnostic — never the raw message body
    (PII minimization).
    """

    __tablename__ = "outreach_inbound_messages"
    __table_args__ = (
        UniqueConstraint(
            "workspace_id", "uidvalidity", "imap_uid", name="uq_inbound_ws_uid"
        ),
        UniqueConstraint(
            "workspace_id", "source_message_id", name="uq_inbound_ws_srcmid"
        ),
        Index("ix_inbound_ws_created", "workspace_id", "created_at"),
    )

    id = Column(Integer, primary_key=True, autoincrement=True)
    workspace_id = Column(String(64), nullable=False)
    imap_uid = Column(String(64), nullable=False, default="")  # 'webhook' for the ESP path
    uidvalidity = Column(String(64), nullable=False, default="")
    source_message_id = Column(String, nullable=False, default="")  # the DSN's own Message-ID
    matched_send_id = Column(Integer, nullable=True)
    kind = Column(String(20), default="unknown")  # hard/soft/complaint/unmatched/unknown
    recipient = Column(String, default="")
    diagnostic = Column(Text, default="")
    processed_at = Column(DateTime(timezone=True), nullable=True)
    created_at = Column(DateTime(timezone=True), server_default=func.now())


class OutreachInboundSchedule(Base):
    """Non-RLS mirror driving the per-workspace inbound IMAP poll (bounce spec).

    DELIBERATELY NOT RLS — read at cold-start WITHOUT a workspace GUC, exactly
    like :class:`OutreachSchedule`. Carries only the workspace id + scheduling /
    IMAP-cursor bookkeeping, no recipient PII.
    """

    __tablename__ = "outreach_inbound_schedules"
    __table_args__ = (
        Index("ix_inbound_sched_due", "enabled", "next_poll_at"),
    )

    workspace_id = Column(String(64), primary_key=True)
    next_poll_at = Column(DateTime(timezone=True), nullable=True)
    enabled = Column(Boolean, nullable=False, default=False, server_default="false")
    consecutive_failures = Column(Integer, nullable=False, default=0, server_default="0")
    uidvalidity = Column(String(64), nullable=True)
    last_uid = Column(String(64), nullable=True)
    updated_at = Column(DateTime(timezone=True), server_default=func.now(), onupdate=func.now())


class OutreachSchedule(Base):
    """Non-RLS mirror driving the autonomous ticker (spec §3.5).

    DELIBERATELY NOT RLS — read at cold-start WITHOUT a workspace GUC, exactly
    like ``scheduled_triggers``. Carries only sequence/workspace ids + a
    timestamp, no recipient PII.
    """

    __tablename__ = "outreach_schedules"
    __table_args__ = (
        Index("ix_outreach_sched_due", "enabled", "next_tick_at"),
    )

    sequence_id = Column(String(36), primary_key=True)
    workspace_id = Column(String(64), nullable=False)
    next_tick_at = Column(DateTime(timezone=True), nullable=True)
    enabled = Column(Boolean, nullable=False, default=False)
    updated_at = Column(DateTime(timezone=True), server_default=func.now(), onupdate=func.now())
