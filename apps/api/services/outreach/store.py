"""Tenant-scoped outreach store (spec §7) — mirrors :class:`PgLeadStore`.

Every read filters ``workspace_id`` (belt) and RLS enforces it again at the DB
(suspenders, PG only). Every write force-stamps ``workspace_id``. On SQLite /
``PG_LEAD_STORE=False`` the SAME class runs against the single-file ORM tables
with app-layer ``workspace_id`` filtering (RLS is PG-only); the only difference
is the backend selection in :func:`get_outreach_store`, which mirrors
``use_pg_store()`` including ``assert_rls_role`` so a misconfigured superuser
role fails fast in BOTH the request and worker paths.

The ``outreach_schedules`` mirror is non-RLS and is read WITHOUT a workspace GUC
at ticker cold-start (see ``sending.bootstrap_outreach_schedules``).
"""

from __future__ import annotations

import logging
import uuid
from datetime import datetime, timedelta, timezone
from typing import Any, Dict, List, Optional

from sqlalchemy import func
from sqlalchemy.exc import IntegrityError

from apps.api.core.config import settings
from apps.api.database import IS_SQLITE, SessionLocal
from apps.api.services.outreach.normalize import normalize_email, suppression_match_keys
from apps.api.services.outreach.orm_models import (
    OutreachEnrollment,
    OutreachInboundMessage,
    OutreachInboundSchedule,
    OutreachSchedule,
    OutreachSend,
    OutreachSequence,
    OutreachSuppression,
)

logger = logging.getLogger("outreach.store")


def _utcnow() -> datetime:
    return datetime.now(timezone.utc)


def use_pg_store() -> bool:
    """True when the shared RLS-protected Postgres store should be used.

    Mirrors ``leadgen.store.use_pg_store`` exactly, including the role check so
    ``assert_rls_role`` fires wherever the store is opened (request OR worker).
    """
    if IS_SQLITE or not settings.PG_LEAD_STORE:
        return False
    from apps.api.services.leadgen.store import assert_rls_role

    return assert_rls_role(strict=settings.PG_RLS_REQUIRE_SAFE_ROLE)


def get_outreach_store(workspace_id: str) -> "PgOutreachStore":
    """Return the tenant-scoped outreach store for ``workspace_id``.

    Publishes ``workspace_id`` into the contextvar so the RLS session hook sets
    the GUC. On PG it also runs ``assert_rls_role`` (via ``use_pg_store``);
    today there is one store class for both dialects, so the call is mainly the
    fail-fast role check + contextvar publish.
    """
    from apps.api.core.tenancy import current_workspace_var

    if workspace_id:
        current_workspace_var.set(workspace_id)
    use_pg_store()  # fail-fast role check on PG; no-op on SQLite
    return PgOutreachStore(workspace_id)


class PgOutreachStore:
    """Tenant-scoped outreach store over the shared outreach_* tables."""

    def __init__(self, workspace_id: str):
        if not workspace_id:
            raise ValueError("PgOutreachStore requires a non-empty workspace_id")
        self.workspace_id = workspace_id

    # ── session helper (mirrors PgLeadStore._session) ──
    def _session(self):
        from apps.api.core.tenancy import current_workspace_var

        current_workspace_var.set(self.workspace_id)
        return SessionLocal()

    # ════════════════════════════ Sequences ════════════════════════════════

    def create_sequence(
        self,
        name: str,
        description: str = "",
        steps: Optional[List[dict]] = None,
        daily_limit: int = 50,
        send_window_start: int = 9,
        send_window_end: int = 18,
        send_window_tz: str = "UTC",
        consent_basis: str = "",
    ) -> dict:
        seq_id = str(uuid.uuid4())
        with self._session() as s, s.begin():
            row = OutreachSequence(
                id=seq_id,
                workspace_id=self.workspace_id,
                name=name,
                description=description or "",
                steps=steps or [],
                status="draft",
                daily_limit=daily_limit,
                send_window_start=send_window_start,
                send_window_end=send_window_end,
                send_window_tz=send_window_tz or "UTC",
                consent_basis=consent_basis or "",
            )
            s.add(row)
            s.flush()
            return self._seq_to_dict(row)

    def list_sequences(self) -> List[dict]:
        with self._session() as s:
            rows = (
                s.query(OutreachSequence)
                .filter(OutreachSequence.workspace_id == self.workspace_id)
                .order_by(OutreachSequence.created_at.desc())
                .all()
            )
            return [self._seq_to_dict(r) for r in rows]

    def get_sequence(self, seq_id: str) -> Optional[dict]:
        with self._session() as s:
            row = self._get_seq_row(s, seq_id)
            return self._seq_to_dict(row) if row else None

    def _get_seq_row(self, s, seq_id: str) -> Optional[OutreachSequence]:
        return (
            s.query(OutreachSequence)
            .filter(
                OutreachSequence.workspace_id == self.workspace_id,
                OutreachSequence.id == seq_id,
            )
            .first()
        )

    def sequence_exists(self, seq_id: str) -> bool:
        """Validate a sequence exists IN THIS WORKSPACE (spec §7 anti-orphan)."""
        with self._session() as s:
            return self._get_seq_row(s, seq_id) is not None

    _UPDATABLE_SEQ_FIELDS = {
        "name", "description", "steps", "status", "daily_limit",
        "send_window_start", "send_window_end", "send_window_tz", "consent_basis",
    }

    def update_sequence(self, seq_id: str, updates: Dict[str, Any]) -> Optional[dict]:
        fields = {k: v for k, v in updates.items() if k in self._UPDATABLE_SEQ_FIELDS}
        with self._session() as s, s.begin():
            row = self._get_seq_row(s, seq_id)
            if not row:
                return None
            for k, v in fields.items():
                setattr(row, k, v)
            s.flush()
            return self._seq_to_dict(row)

    def set_sequence_status(self, seq_id: str, status: str) -> Optional[dict]:
        return self.update_sequence(seq_id, {"status": status})

    def delete_sequence(self, seq_id: str) -> bool:
        with self._session() as s, s.begin():
            row = self._get_seq_row(s, seq_id)
            if not row:
                return False
            s.query(OutreachSend).filter(
                OutreachSend.workspace_id == self.workspace_id,
                OutreachSend.sequence_id == seq_id,
            ).delete(synchronize_session=False)
            s.query(OutreachEnrollment).filter(
                OutreachEnrollment.workspace_id == self.workspace_id,
                OutreachEnrollment.sequence_id == seq_id,
            ).delete(synchronize_session=False)
            s.delete(row)
            # Remove the (non-RLS) schedule mirror row.
            s.query(OutreachSchedule).filter(
                OutreachSchedule.sequence_id == seq_id
            ).delete(synchronize_session=False)
            return True

    # ════════════════════════════ Enrollments ══════════════════════════════

    def enroll(
        self,
        seq_id: str,
        lead_id: int,
        to_email_snapshot: str,
        consent_source: str = "",
        consent_at: Optional[datetime] = None,
    ) -> Optional[int]:
        """Insert one enrollment (deduped). Returns the id, or None if a dup.

        Caller MUST validate the sequence exists in-ws (``sequence_exists``) and
        provide a non-empty normalized ``to_email_snapshot`` (the router/executor
        do this and reject ``no_email``).
        """
        email = normalize_email(to_email_snapshot)
        if not email:
            raise ValueError("no_email")
        with self._session() as s:
            try:
                with s.begin():
                    row = OutreachEnrollment(
                        workspace_id=self.workspace_id,
                        sequence_id=seq_id,
                        lead_id=lead_id,
                        to_email_snapshot=email,
                        consent_source=consent_source or "",
                        consent_at=consent_at,
                        current_step=0,
                        status="pending",
                        next_send_at=_utcnow(),
                    )
                    s.add(row)
                    s.flush()
                    return row.id
            except IntegrityError:
                s.rollback()
                return None  # already enrolled (uq_enroll_ws_seq_lead)

    def get_enrollment(self, enrollment_id: int) -> Optional[OutreachEnrollment]:
        with self._session() as s:
            return self._get_enrollment_row(s, enrollment_id)

    def _get_enrollment_row(self, s, enrollment_id: int) -> Optional[OutreachEnrollment]:
        return (
            s.query(OutreachEnrollment)
            .filter(
                OutreachEnrollment.workspace_id == self.workspace_id,
                OutreachEnrollment.id == enrollment_id,
            )
            .first()
        )

    def due_enrollments(self, seq_id: str, limit: int = 200) -> List[dict]:
        """Enrollments with a due step (pending/scheduled, next_send_at<=now)."""
        now = _utcnow()
        with self._session() as s:
            rows = (
                s.query(OutreachEnrollment)
                .filter(
                    OutreachEnrollment.workspace_id == self.workspace_id,
                    OutreachEnrollment.sequence_id == seq_id,
                    OutreachEnrollment.status.in_(("pending", "scheduled")),
                    OutreachEnrollment.next_send_at <= now,
                )
                .order_by(OutreachEnrollment.next_send_at.asc())
                .limit(limit)
                .all()
            )
            return [self._enroll_to_dict(r) for r in rows]

    def reschedule_enrollment(self, enrollment_id: int, next_send_at: datetime) -> None:
        with self._session() as s, s.begin():
            row = self._get_enrollment_row(s, enrollment_id)
            if row:
                row.next_send_at = next_send_at
                if row.status == "pending":
                    row.status = "scheduled"

    def advance_enrollment(
        self,
        enrollment_id: int,
        *,
        status: str,
        next_send_at: Optional[datetime] = None,
        current_step: Optional[int] = None,
        increment_sent: bool = False,
        error: str = "",
    ) -> None:
        with self._session() as s, s.begin():
            row = self._get_enrollment_row(s, enrollment_id)
            if not row:
                return
            row.status = status
            if next_send_at is not None:
                row.next_send_at = next_send_at
            if current_step is not None:
                row.current_step = current_step
            if increment_sent:
                row.sent_count = (row.sent_count or 0) + 1
                row.last_sent_at = _utcnow()
            if error:
                row.error = error

    def increment_soft_bounce(self, enrollment_id: int) -> int:
        with self._session() as s, s.begin():
            row = self._get_enrollment_row(s, enrollment_id)
            if not row:
                return 0
            row.soft_bounce_count = (row.soft_bounce_count or 0) + 1
            return row.soft_bounce_count

    # ════════════════════════════ Sends ════════════════════════════════════

    def get_send_by_idem(self, idempotency_key: str) -> Optional[OutreachSend]:
        with self._session() as s:
            return (
                s.query(OutreachSend)
                .filter(
                    OutreachSend.workspace_id == self.workspace_id,
                    OutreachSend.idempotency_key == idempotency_key,
                )
                .first()
            )

    def get_send_by_message_id(self, message_id: str) -> Optional[dict]:
        """Most-recent send in THIS workspace with this server Message-ID.

        Workspace-scoped (no cross-tenant lookup): the mailbox belongs to this
        workspace. A hard Message-ID match is the v1 anti-spoof basis.
        """
        mid = (message_id or "").strip()
        if not mid:
            return None
        with self._session() as s:
            row = (
                s.query(OutreachSend)
                .filter(
                    OutreachSend.workspace_id == self.workspace_id,
                    OutreachSend.message_id == mid,
                )
                .order_by(OutreachSend.id.desc())
                .first()
            )
            return self._send_to_dict(row) if row else None

    def get_recent_send_to(self, email: str) -> Optional[dict]:
        """Most-recent send to ``email`` in this workspace (non-terminal preferred).

        Recipient-only fallback used for LEDGER annotation only — v1 never feeds
        the breaker/suppression off a recipient-only match (anti-spoof decision).
        """
        canonical = normalize_email(email)
        if not canonical:
            return None
        with self._session() as s:
            base = s.query(OutreachSend).filter(
                OutreachSend.workspace_id == self.workspace_id,
                OutreachSend.to_email == canonical,
            )
            row = (
                base.filter(OutreachSend.status.in_(("sent", "in_flight")))
                .order_by(OutreachSend.id.desc())
                .first()
            )
            if row is None:
                row = base.order_by(OutreachSend.id.desc()).first()
            return self._send_to_dict(row) if row else None

    def mark_send_bounced(self, send_id: int, error: str = "") -> bool:
        """Set status='bounced' + bounced_at on a send row (set-idempotent)."""
        with self._session() as s, s.begin():
            row = (
                s.query(OutreachSend)
                .filter(
                    OutreachSend.workspace_id == self.workspace_id,
                    OutreachSend.id == send_id,
                )
                .first()
            )
            if row is None:
                return False
            row.status = "bounced"
            if row.bounced_at is None:
                row.bounced_at = _utcnow()
            if error:
                row.error = error[:500]
            return True

    def list_sends(self, seq_id: Optional[str] = None, limit: int = 200) -> List[dict]:
        with self._session() as s:
            q = s.query(OutreachSend).filter(
                OutreachSend.workspace_id == self.workspace_id
            )
            if seq_id:
                q = q.filter(OutreachSend.sequence_id == seq_id)
            rows = q.order_by(OutreachSend.created_at.desc()).limit(limit).all()
            return [self._send_to_dict(r) for r in rows]

    def sent_count_last_hour(self) -> int:
        cutoff = _utcnow() - timedelta(hours=1)
        with self._session() as s:
            return (
                s.query(OutreachSend)
                .filter(
                    OutreachSend.workspace_id == self.workspace_id,
                    OutreachSend.status == "sent",
                    OutreachSend.sent_at >= cutoff,
                )
                .count()
            )

    def sent_count_today(self, seq_id: str) -> int:
        cutoff = _utcnow() - timedelta(days=1)
        with self._session() as s:
            return (
                s.query(OutreachSend)
                .filter(
                    OutreachSend.workspace_id == self.workspace_id,
                    OutreachSend.sequence_id == seq_id,
                    OutreachSend.status == "sent",
                    OutreachSend.sent_at >= cutoff,
                )
                .count()
            )

    def get_sequence_stats(self, seq_id: str) -> Dict[str, int]:
        with self._session() as s:
            stats: Dict[str, int] = {}
            rows = (
                s.query(OutreachEnrollment.status, func.count())
                .filter(
                    OutreachEnrollment.workspace_id == self.workspace_id,
                    OutreachEnrollment.sequence_id == seq_id,
                )
                .group_by(OutreachEnrollment.status)
                .all()
            )
            by_status = {k: v for k, v in rows}
            for st in ("pending", "scheduled", "sent", "opened", "replied",
                       "bounced", "failed", "skipped", "suppressed", "completed"):
                stats[st] = by_status.get(st, 0)
            stats["total"] = sum(by_status.values())
            stats["emails_sent"] = (
                s.query(OutreachSend)
                .filter(
                    OutreachSend.workspace_id == self.workspace_id,
                    OutreachSend.sequence_id == seq_id,
                    OutreachSend.status == "sent",
                )
                .count()
            )
            seq = self._get_seq_row(s, seq_id)
            if seq:
                stats["bounce_count"] = seq.bounce_count or 0
                stats["complaint_count"] = seq.complaint_count or 0
                stats["auto_paused"] = 1 if seq.auto_paused else 0
            return stats

    # ════════════════════════════ Suppressions ═════════════════════════════

    def is_suppressed(self, email: str) -> bool:
        keys = suppression_match_keys(email)
        if not keys:
            return False
        with self._session() as s:
            return (
                s.query(OutreachSuppression)
                .filter(
                    OutreachSuppression.workspace_id == self.workspace_id,
                    OutreachSuppression.email.in_(keys),
                )
                .first()
                is not None
            )

    def add_suppression(
        self, email: str, reason: str = "manual", source: str = "", locked: bool = False
    ) -> bool:
        """Idempotent insert (ON CONFLICT DO NOTHING via catch). True if newly added."""
        canonical = normalize_email(email)
        if not canonical:
            return False
        with self._session() as s:
            try:
                with s.begin():
                    s.add(
                        OutreachSuppression(
                            workspace_id=self.workspace_id,
                            email=canonical,
                            reason=reason,
                            source=source or "",
                            locked=locked,
                        )
                    )
                return True
            except IntegrityError:
                s.rollback()
                return False  # already suppressed

    def list_suppressions(self, limit: int = 500) -> List[dict]:
        with self._session() as s:
            rows = (
                s.query(OutreachSuppression)
                .filter(OutreachSuppression.workspace_id == self.workspace_id)
                .order_by(OutreachSuppression.created_at.desc())
                .limit(limit)
                .all()
            )
            return [
                {
                    "id": r.id,
                    "email": r.email,
                    "reason": r.reason,
                    "source": r.source,
                    "locked": bool(r.locked),
                    "created_at": r.created_at.isoformat() if r.created_at else None,
                }
                for r in rows
            ]

    def remove_suppression(self, email: str) -> str:
        """Remove a suppression. Returns 'ok' / 'locked' / 'not_found'."""
        canonical = normalize_email(email)
        with self._session() as s, s.begin():
            row = (
                s.query(OutreachSuppression)
                .filter(
                    OutreachSuppression.workspace_id == self.workspace_id,
                    OutreachSuppression.email == canonical,
                )
                .first()
            )
            if not row:
                return "not_found"
            if row.locked:
                return "locked"
            s.delete(row)
            return "ok"

    # ════════════════════════ Circuit breaker (LOCKED §2) ══════════════════

    def record_bounce_and_maybe_pause(
        self, seq_id: Optional[str], *, complaint: bool = False
    ) -> bool:
        """Increment per-sequence bounce/complaint; auto-pause past thresholds.

        Returns True when this call tripped the breaker (sequence auto-paused).
        """
        if not seq_id:
            return False
        with self._session() as s, s.begin():
            seq = self._get_seq_row(s, seq_id)
            if not seq:
                return False
            if complaint:
                seq.complaint_count = (seq.complaint_count or 0) + 1
            else:
                seq.bounce_count = (seq.bounce_count or 0) + 1
            sent = (
                s.query(OutreachSend)
                .filter(
                    OutreachSend.workspace_id == self.workspace_id,
                    OutreachSend.sequence_id == seq_id,
                    OutreachSend.status.in_(("sent", "bounced")),
                )
                .count()
            )
            if seq.auto_paused or sent < settings.OUTREACH_CIRCUIT_MIN_SENDS:
                return False
            bounce_rate = (seq.bounce_count or 0) / sent if sent else 0.0
            complaint_rate = (seq.complaint_count or 0) / sent if sent else 0.0
            if (
                bounce_rate > settings.OUTREACH_BOUNCE_PAUSE_RATE
                or complaint_rate > settings.OUTREACH_COMPLAINT_PAUSE_RATE
            ):
                seq.auto_paused = True
                seq.status = "paused"
                return True
            return False

    # ════════════════════ Shared bounce/complaint application ═══════════════

    def apply_bounce(
        self,
        *,
        send_id: Optional[int],
        sequence_id: Optional[str],
        enrollment_id: Optional[int],
        to_email: str,
        kind: str,
        diagnostic: str = "",
        source: str = "",
    ) -> None:
        """The single idempotent-on-effect bounce/complaint applier (spec §design 5).

        BOTH the IMAP poller and the ``bounce_webhook`` route through this. It is
        safe to call once per inbound message — the CALLER (ledger insert) is the
        idempotency gate, because the breaker counter is NOT itself idempotent.

          * ``hard``      → mark send bounced + ``bounce`` suppression + advance
                            enrollment ``bounced`` + bounce breaker.
          * ``complaint`` → mark send bounced + LOCKED ``complaint`` suppression +
                            advance enrollment ``bounced`` + complaint breaker.
          * ``soft``      → increment the per-enrollment soft counter; suppress +
                            terminal only at ``OUTREACH_SOFT_BOUNCE_MAX`` (no
                            breaker, matches the webhook soft path).
        """
        src = source or "inbound"
        if kind == "complaint":
            if send_id is not None:
                self.mark_send_bounced(send_id, diagnostic or "complaint")
            self.add_suppression(to_email, reason="complaint", source=src, locked=True)
            if enrollment_id is not None:
                self.advance_enrollment(enrollment_id, status="bounced", error="complaint")
            self.record_bounce_and_maybe_pause(sequence_id, complaint=True)
        elif kind == "soft":
            if enrollment_id is not None:
                count = self.increment_soft_bounce(enrollment_id)
                if count >= settings.OUTREACH_SOFT_BOUNCE_MAX:
                    self.add_suppression(to_email, reason="bounce", source=src, locked=False)
                    if send_id is not None:
                        self.mark_send_bounced(send_id, diagnostic or "soft_bounce_max")
                    self.advance_enrollment(enrollment_id, status="bounced", error="soft_bounce_max")
        else:  # hard (default)
            if send_id is not None:
                self.mark_send_bounced(send_id, diagnostic or "hard_bounce")
            self.add_suppression(to_email, reason="bounce", source=src, locked=False)
            if enrollment_id is not None:
                self.advance_enrollment(enrollment_id, status="bounced", error="hard_bounce")
            self.record_bounce_and_maybe_pause(sequence_id, complaint=False)

    # ════════════════════ Inbound dedup ledger (RLS) ═══════════════════════

    def record_inbound_processed(
        self,
        *,
        imap_uid: str,
        uidvalidity: str,
        source_message_id: str,
        matched_send_id: Optional[int],
        kind: str,
        recipient: str = "",
        diagnostic: str = "",
    ) -> bool:
        """Idempotent insert into the inbound ledger.

        Returns True when a NEW row was created (caller may then ``apply_bounce``),
        False when this message was already processed (unique conflict on either
        ``(ws, uidvalidity, imap_uid)`` or ``(ws, source_message_id)``).
        """
        # Never let an empty source_message_id collide across distinct messages —
        # synthesize a per-uid key so the secondary unique stays meaningful.
        src_mid = (source_message_id or "").strip() or f"uid:{uidvalidity}:{imap_uid}"
        with self._session() as s:
            try:
                with s.begin():
                    s.add(OutreachInboundMessage(
                        workspace_id=self.workspace_id,
                        imap_uid=str(imap_uid),
                        uidvalidity=str(uidvalidity or ""),
                        source_message_id=src_mid,
                        matched_send_id=matched_send_id,
                        kind=kind,
                        recipient=(recipient or "")[:320],
                        diagnostic=(diagnostic or "")[:500],
                        processed_at=_utcnow(),
                    ))
                return True
            except IntegrityError:
                s.rollback()
                return False  # already processed

    def inbound_message_count(self) -> int:
        with self._session() as s:
            return (
                s.query(OutreachInboundMessage)
                .filter(OutreachInboundMessage.workspace_id == self.workspace_id)
                .count()
            )

    # ════════════════════ Inbound schedule mirror (non-RLS) ════════════════

    def get_inbound_schedule(self) -> Optional[OutreachInboundSchedule]:
        with self._session() as s:
            return (
                s.query(OutreachInboundSchedule)
                .filter(OutreachInboundSchedule.workspace_id == self.workspace_id)
                .first()
            )

    def upsert_inbound_schedule(
        self,
        *,
        next_poll_at: Optional[datetime],
        enabled: bool,
        consecutive_failures: Optional[int] = None,
        uidvalidity: Optional[str] = None,
        last_uid: Optional[str] = None,
    ) -> None:
        with self._session() as s, s.begin():
            row = (
                s.query(OutreachInboundSchedule)
                .filter(OutreachInboundSchedule.workspace_id == self.workspace_id)
                .first()
            )
            if row is None:
                row = OutreachInboundSchedule(workspace_id=self.workspace_id)
                s.add(row)
            row.next_poll_at = next_poll_at
            row.enabled = enabled
            if consecutive_failures is not None:
                row.consecutive_failures = consecutive_failures
            if uidvalidity is not None:
                row.uidvalidity = uidvalidity
            if last_uid is not None:
                row.last_uid = last_uid

    def disable_inbound_schedule(self) -> None:
        with self._session() as s, s.begin():
            row = (
                s.query(OutreachInboundSchedule)
                .filter(OutreachInboundSchedule.workspace_id == self.workspace_id)
                .first()
            )
            if row:
                row.enabled = False
                row.next_poll_at = None

    # ════════════════════════ Schedule mirror (non-RLS) ════════════════════

    def upsert_schedule(self, seq_id: str, next_tick_at: datetime, enabled: bool) -> None:
        # The mirror is non-RLS; still go through a normal session.
        with self._session() as s, s.begin():
            row = (
                s.query(OutreachSchedule)
                .filter(OutreachSchedule.sequence_id == seq_id)
                .first()
            )
            if row is None:
                row = OutreachSchedule(
                    sequence_id=seq_id, workspace_id=self.workspace_id
                )
                s.add(row)
            row.workspace_id = self.workspace_id
            row.next_tick_at = next_tick_at
            row.enabled = enabled

    def disable_schedule(self, seq_id: str) -> None:
        with self._session() as s, s.begin():
            row = (
                s.query(OutreachSchedule)
                .filter(OutreachSchedule.sequence_id == seq_id)
                .first()
            )
            if row:
                row.enabled = False

    # ════════════════════════════ mappers ══════════════════════════════════

    @staticmethod
    def _seq_to_dict(row: OutreachSequence) -> dict:
        return {
            "id": row.id,
            "workspace_id": row.workspace_id,
            "name": row.name,
            "description": row.description or "",
            "steps": row.steps or [],
            "status": row.status,
            "daily_limit": row.daily_limit,
            "send_window_start": row.send_window_start,
            "send_window_end": row.send_window_end,
            "send_window_tz": row.send_window_tz,
            "consent_basis": row.consent_basis or "",
            "bounce_count": row.bounce_count or 0,
            "complaint_count": row.complaint_count or 0,
            "auto_paused": bool(row.auto_paused),
            "created_at": row.created_at.isoformat() if row.created_at else None,
            "updated_at": row.updated_at.isoformat() if row.updated_at else None,
        }

    @staticmethod
    def _enroll_to_dict(row: OutreachEnrollment) -> dict:
        return {
            "id": row.id,
            "sequence_id": row.sequence_id,
            "lead_id": row.lead_id,
            "to_email_snapshot": row.to_email_snapshot,
            "current_step": row.current_step,
            "status": row.status,
            "next_send_at": row.next_send_at.isoformat() if row.next_send_at else None,
            "sent_count": row.sent_count,
            "soft_bounce_count": row.soft_bounce_count or 0,
        }

    @staticmethod
    def _send_to_dict(row: OutreachSend) -> dict:
        return {
            "id": row.id,
            "sequence_id": row.sequence_id,
            "enrollment_id": row.enrollment_id,
            "lead_id": row.lead_id,
            "step_number": row.step_number,
            "to_email": row.to_email,
            "subject": row.subject,
            "status": row.status,
            "skip_reason": row.skip_reason,
            "message_id": row.message_id,
            "charged_usd": round(row.charged_usd or 0.0, 4),
            "migrated": bool(row.migrated),
            "error": row.error,
            "sent_at": row.sent_at.isoformat() if row.sent_at else None,
            "created_at": row.created_at.isoformat() if row.created_at else None,
        }
