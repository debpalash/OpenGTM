"""Inbound IMAP-poll ingestion for async bounces/complaints (bounce spec).

BYO-SMTP means async DSNs (RFC 3464) and FBL/ARF complaints come back as ordinary
emails to the workspace's own mailbox. This module polls that mailbox per
workspace on the existing durable-queue / self-scheduling ticker pattern (mirrors
``poller.engine`` and ``outreach.sending``), parses each message
(``dsn_parse``), maps it back to the originating ``OutreachSend`` by our
server-generated Message-ID, and applies the effect via the shared idempotent
``store.apply_bounce`` — gated by the RLS inbound ledger so a re-fetch is a no-op.

Tenancy: per-workspace IMAP creds (WI-6); all writes inside ``workspace_scope``;
the mapping is workspace-scoped (no cross-tenant lookup). Default OFF behind
``OUTREACH_INBOUND_POLL_ENABLED`` + per-workspace IMAP config.

IMAP is wrapped behind a tiny client so tests inject a fake; a real server is
NEVER contacted from the test suite.
"""

from __future__ import annotations

import logging
from datetime import datetime, timedelta, timezone
from typing import List, Optional, Protocol

from sqlalchemy import text
from sqlalchemy.exc import IntegrityError

from apps.api.core.config import settings
from apps.api.database import IS_SQLITE, SessionLocal
from apps.api.services.outreach import dsn_parse
from apps.api.services.outreach.orm_models import OutreachInboundSchedule
from apps.api.services.outreach.sender import IMAPConfig, get_imap_config
from apps.api.services.outreach.store import get_outreach_store

logger = logging.getLogger("outreach.inbound")

JOB_TYPE = "outreach_inbound_poll"


def _utcnow() -> datetime:
    return datetime.now(timezone.utc)


def _enabled() -> bool:
    return bool(getattr(settings, "AUTOMATIONS_ENABLED", False)) and bool(
        getattr(settings, "OUTREACH_INBOUND_POLL_ENABLED", False)
    )


def _is_pg(db) -> bool:
    try:
        return db.get_bind().dialect.name == "postgresql"
    except Exception:
        return not IS_SQLITE


# ── IMAP client (injectable for tests) ───────────────────────────────────────

class ImapClient(Protocol):
    def connect(self) -> None: ...
    def select(self, folder: str) -> str: ...  # returns UIDVALIDITY
    def search_unseen(self, since: datetime) -> List[str]: ...
    def fetch(self, uid: str) -> bytes: ...
    def mark_seen(self, uid: str) -> None: ...
    def logout(self) -> None: ...


class RealImapClient:
    """imaplib-backed IMAP client. Only constructed in production / real runs."""

    def __init__(self, cfg: IMAPConfig):
        self.cfg = cfg
        self._c = None

    def connect(self) -> None:
        import imaplib

        if self.cfg.use_ssl:
            self._c = imaplib.IMAP4_SSL(self.cfg.host, self.cfg.port)
        else:
            self._c = imaplib.IMAP4(self.cfg.host, self.cfg.port)
        self._c.login(self.cfg.user, self.cfg.password)

    def select(self, folder: str) -> str:
        self._c.select(folder, readonly=False)
        typ, data = self._c.response("UIDVALIDITY")
        if data and data[0]:
            return data[0].decode() if isinstance(data[0], bytes) else str(data[0])
        return ""

    def search_unseen(self, since: datetime) -> List[str]:
        crit = since.strftime("%d-%b-%Y")
        typ, data = self._c.uid("search", None, "UNSEEN", "SINCE", crit)
        if typ != "OK" or not data or not data[0]:
            return []
        return [u.decode() if isinstance(u, bytes) else str(u) for u in data[0].split()]

    def fetch(self, uid: str) -> bytes:
        typ, data = self._c.uid("fetch", uid, "(RFC822)")
        if typ != "OK" or not data or not data[0]:
            return b""
        first = data[0]
        return first[1] if isinstance(first, tuple) and len(first) > 1 else b""

    def mark_seen(self, uid: str) -> None:
        self._c.uid("store", uid, "+FLAGS", "(\\Seen)")

    def logout(self) -> None:
        try:
            if self._c is not None:
                self._c.logout()
        except Exception:
            pass


def _make_client(cfg: IMAPConfig) -> ImapClient:
    """Factory — tests monkeypatch this to inject a fake client."""
    return RealImapClient(cfg)


# ── single-flight enqueue (mirrors poller.engine._enqueue_poll_if_absent) ─────

def inbound_fire_key(ws_id: str, next_at: datetime) -> str:
    return f"inbound:{ws_id}:{next_at.isoformat()}"


def _enqueue_inbound_if_absent(db, *, ws_id: str, next_run_at: datetime) -> bool:
    from apps.api.models import Job

    fire_key = inbound_fire_key(ws_id, next_run_at)
    if _is_pg(db):
        db.execute(
            text("SELECT pg_advisory_xact_lock(hashtext(:k))"),
            {"k": f"{JOB_TYPE}:{fire_key}"},
        )
    existing = (
        db.query(Job)
        .filter(
            Job.type == JOB_TYPE,
            Job.fire_key == fire_key,
            Job.status.in_(("pending", "processing")),
        )
        .first()
    )
    if existing is not None:
        return False
    job = Job(
        type=JOB_TYPE,
        payload={"workspace_id": ws_id, "fire_key": fire_key},
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


# ── reschedule + backoff (mirrors poller.engine._reschedule_watch) ────────────

def _reschedule(ws_id: str, *, failed: bool, uidvalidity: Optional[str] = None,
                last_uid: Optional[str] = None) -> None:
    """Update the non-RLS mirror + (when still enabled) enqueue the next poll.

    Backoff on consecutive IMAP failures; hard-disable past the cap so one bad
    mailbox never blocks other workspaces or wedges the worker.
    """
    interval = int(settings.OUTREACH_INBOUND_POLL_INTERVAL)
    with SessionLocal() as db, db.begin():
        row = (
            db.query(OutreachInboundSchedule)
            .filter(OutreachInboundSchedule.workspace_id == ws_id)
            .first()
        )
        if row is None:
            row = OutreachInboundSchedule(workspace_id=ws_id)
            db.add(row)
        if uidvalidity is not None:
            row.uidvalidity = uidvalidity
        if last_uid is not None:
            row.last_uid = last_uid

        if failed:
            row.consecutive_failures = (row.consecutive_failures or 0) + 1
            cap = int(settings.OUTREACH_INBOUND_MAX_CONSECUTIVE_FAILURES)
            if row.consecutive_failures >= cap:
                row.enabled = False
                row.next_poll_at = None
                logger.warning("inbound poll auto-disabled ws=%s after %d failures",
                               ws_id, row.consecutive_failures)
                return
            backoff = min(interval * (2 ** (row.consecutive_failures - 1)), 86400)
            next_at = _utcnow() + timedelta(seconds=backoff)
        else:
            row.consecutive_failures = 0
            next_at = _utcnow() + timedelta(seconds=interval)

        row.next_poll_at = next_at
        row.enabled = True
        _enqueue_inbound_if_absent(db, ws_id=ws_id, next_run_at=next_at)


def _disable(ws_id: str) -> None:
    with SessionLocal() as db, db.begin():
        row = (
            db.query(OutreachInboundSchedule)
            .filter(OutreachInboundSchedule.workspace_id == ws_id)
            .first()
        )
        if row is None:
            row = OutreachInboundSchedule(workspace_id=ws_id)
            db.add(row)
        row.enabled = False
        row.next_poll_at = None


# ── per-message processing (idempotent via the ledger gate) ───────────────────

def _process_one(store, ws_id: str, uidvalidity: str, uid: str, raw: bytes) -> None:
    src_mid = dsn_parse.source_message_id(raw)
    records = dsn_parse.parse_message(raw)

    # Hard Message-ID match only (anti-spoof v1): find the first record whose
    # original Message-ID maps to one of OUR server-generated sends.
    chosen = None
    matched = None
    for rec in records:
        if rec.original_message_id:
            send = store.get_send_by_message_id(rec.original_message_id)
            if send is not None:
                chosen, matched = rec, send
                break

    if chosen is None:
        # No hard match. Recipient-only fallback is recorded for visibility but
        # NOT acted on in v1 (a spoofed DSN can't trip a competitor's breaker).
        rec0 = records[0] if records else None
        fb_send = None
        if rec0 and rec0.recipient:
            fb_send = store.get_recent_send_to(rec0.recipient)
        store.record_inbound_processed(
            imap_uid=uid,
            uidvalidity=uidvalidity,
            source_message_id=src_mid,
            matched_send_id=(fb_send["id"] if fb_send else None),
            kind=("unmatched" if records else "unknown"),
            recipient=(rec0.recipient if rec0 else ""),
            diagnostic=(rec0.diagnostic if rec0 else "no bounce record"),
        )
        return

    # Hard match → ledger insert is the idempotency gate. Apply ONLY when newly
    # created, so a re-fetch / retry / restart never double-counts the breaker.
    created = store.record_inbound_processed(
        imap_uid=uid,
        uidvalidity=uidvalidity,
        source_message_id=src_mid,
        matched_send_id=matched["id"],
        kind=chosen.kind,
        recipient=chosen.recipient or matched["to_email"],
        diagnostic=chosen.diagnostic,
    )
    if not created:
        return  # already processed

    store.apply_bounce(
        send_id=matched["id"],
        sequence_id=matched["sequence_id"],
        enrollment_id=matched["enrollment_id"],
        to_email=matched["to_email"],
        kind=chosen.kind,
        diagnostic=chosen.diagnostic,
        source="imap",
    )


def _run_poll(store, ws_id: str, cfg: IMAPConfig, client: ImapClient) -> str:
    """Fetch + process UNSEEN messages. Returns the mailbox UIDVALIDITY.

    Raises on connection/auth failure (caller treats it as a failed tick and
    backs off). Marking ``\\Seen`` is best-effort AFTER the ledger commit.
    """
    client.connect()
    try:
        uidvalidity = client.select(cfg.folder)
        since = _utcnow() - timedelta(days=max(1, int(settings.OUTREACH_INBOUND_LOOKBACK_DAYS)))
        uids = client.search_unseen(since)[: int(settings.OUTREACH_INBOUND_MAX_FETCH)]
        for uid in uids:
            raw = client.fetch(uid)
            if not raw:
                continue
            _process_one(store, ws_id, uidvalidity, uid, raw)
            # least-invasive post-processing: flag \Seen only (never move/delete).
            try:
                client.mark_seen(uid)
            except Exception as e:  # noqa: BLE001 — ledger already prevents reprocessing
                logger.warning("inbound mark_seen failed ws=%s uid=%s: %s", ws_id, uid, e)
        return uidvalidity
    finally:
        client.logout()


# ── the durable-queue handler ─────────────────────────────────────────────────

async def handle_inbound_poll(job_id: int, payload: dict) -> None:
    """Process one ``outreach_inbound_poll`` job for one workspace.

    Runs in the worker's per-job thread (queue_service invokes via asyncio.run in
    a thread), so the blocking IMAP I/O does not stall the main loop.
    """
    if not _enabled():
        return
    ws_id = payload.get("workspace_id")
    if not ws_id:
        logger.warning("inbound poll missing workspace_id: %s", payload)
        return

    cfg = get_imap_config(ws_id)
    if not (cfg.host and cfg.user and cfg.password):
        # Creds removed / never set → stop scheduling for this workspace.
        _disable(ws_id)
        return

    from apps.api.core.tenancy import workspace_scope

    with workspace_scope(ws_id):
        store = get_outreach_store(ws_id)
        uidvalidity: Optional[str] = None
        failed = False
        try:
            client = _make_client(cfg)
            uidvalidity = _run_poll(store, ws_id, cfg, client)
        except Exception as e:  # noqa: BLE001 — never block other workspaces
            failed = True
            logger.warning("inbound poll failed ws=%s: %s", ws_id, e)
        _reschedule(ws_id, failed=failed, uidvalidity=uidvalidity)


# ── cold-start bootstrap (non-RLS mirror, GUC-less) ───────────────────────────

def bootstrap_inbound_schedules() -> int:
    """Cold-start: read the NON-RLS inbound mirror (no workspace GUC), then per
    due workspace re-enter workspace_scope to enqueue. Survives restarts.

    No-op unless the feature is enabled. Also seeds a mirror row for any
    IMAP-configured workspace that has an enabled outreach schedule but no inbound
    schedule yet (so enabling the flag activates ingestion on next cold start)."""
    if not _enabled():
        return 0

    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.outreach.orm_models import OutreachSchedule
    from apps.api.services.outreach.sender import is_imap_configured

    now = _utcnow()
    enqueued = 0
    with SessionLocal() as db:
        rows = db.query(OutreachInboundSchedule).all()
        existing_ws = {r.workspace_id for r in rows}
        due_ws = [
            r.workspace_id for r in rows
            if r.enabled and (r.next_poll_at is None or r.next_poll_at <= now)
        ]
        # Seed inbound schedules for IMAP-configured workspaces that have outreach
        # activity but no inbound mirror row yet.
        seed_ws = {
            r.workspace_id
            for r in db.query(OutreachSchedule).filter(OutreachSchedule.enabled.is_(True)).all()
            if r.workspace_id not in existing_ws
        }

    for ws_id in seed_ws:
        if is_imap_configured(ws_id):
            due_ws.append(ws_id)

    for ws_id in dict.fromkeys(due_ws):  # dedup, preserve order
        try:
            if not is_imap_configured(ws_id):
                _disable(ws_id)
                continue
            with workspace_scope(ws_id):
                _reschedule(ws_id, failed=False)
                enqueued += 1
        except Exception as e:  # noqa: BLE001
            logger.warning("inbound bootstrap failed ws=%s: %s", ws_id, e)
    logger.info("bootstrap_inbound_schedules enqueued %d inbound poll(s)", enqueued)
    return enqueued
