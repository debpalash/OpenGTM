"""Async bounce/complaint ingestion — behavioral tests (SQLite default).

Covers the bounce-ingestion spec acceptance criteria:
  * dsn_parse unit (RFC3464 hard/soft, ARF complaint, heuristic, non-DSN).
  * Message-ID prerequisite — send_email emits + returns a non-empty server id.
  * End-to-end via a FAKE IMAP client (no real server EVER): match → bounced +
    suppressed + enrollment advanced + breaker fed; complaint; soft accumulation.
  * Idempotency — same message processed twice → one effect (ledger-gated).
  * Spoof-safety — DSN with no matching server Message-ID → ledgered, not acted on.
  * Unknown format → ledgered, no breaker action.
  * IMAP failure → backoff + consecutive_failures; hard-disable at the cap.
  * Webhook refactor — still works, now marks the send row + is idempotent.

SMTP/IMAP are ALWAYS mocked.
"""

import asyncio

import pytest
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.core.config import settings
from apps.api.database import Base
from apps.api.models import Job
from apps.api.services.outreach import orm_models as om
from apps.api.services.outreach.orm_models import (
    OutreachInboundMessage,
    OutreachInboundSchedule,
    OutreachSend,
)

WS = "ws_inbound_a"


def _run(coro):
    return asyncio.new_event_loop().run_until_complete(coro)


# ════════════════════════════ Fixtures ══════════════════════════════════════

@pytest.fixture()
def engine():
    eng = create_engine(
        "sqlite://", connect_args={"check_same_thread": False}, poolclass=StaticPool,
    )
    Base.metadata.create_all(eng, tables=[
        om.OutreachSequence.__table__, om.OutreachEnrollment.__table__,
        om.OutreachSend.__table__, om.OutreachSuppression.__table__,
        om.OutreachSchedule.__table__, om.OutreachInboundMessage.__table__,
        om.OutreachInboundSchedule.__table__, Job.__table__,
    ])
    return eng


@pytest.fixture()
def SL(engine, monkeypatch):
    sl = sessionmaker(bind=engine, autoflush=False)
    import apps.api.database as database
    import apps.api.services.outreach.store as store_mod
    import apps.api.services.outreach.inbound as inbound_mod
    import apps.api.services.queue_service as qs_mod
    monkeypatch.setattr(database, "SessionLocal", sl, raising=False)
    monkeypatch.setattr(store_mod, "SessionLocal", sl, raising=False)
    monkeypatch.setattr(inbound_mod, "SessionLocal", sl, raising=False)
    monkeypatch.setattr(qs_mod, "SessionLocal", sl, raising=False)
    monkeypatch.setattr(settings, "AUTOMATIONS_ENABLED", True, raising=False)
    monkeypatch.setattr(settings, "OUTREACH_INBOUND_POLL_ENABLED", True, raising=False)
    return sl


def _mk_active_seq(store):
    seq = store.create_sequence(
        "S", steps=[{"step_number": 0, "subject": "Hi", "body_html": "<p>Hi</p>", "delay_hours": 0},
                    {"step_number": 1, "subject": "Bump", "body_html": "<p>Bump</p>", "delay_hours": 24}],
        send_window_start=0, send_window_end=24, consent_basis="legit")
    store.set_sequence_status(seq["id"], "active")
    return seq


def _seed_sent(store, seq_id, eid, message_id, to_email="bob@example.com", lead_id=1, idem="idem-1"):
    """Insert a 'sent' OutreachSend row with a known server Message-ID."""
    with store._session() as s, s.begin():
        s.add(OutreachSend(
            workspace_id=store.workspace_id, sequence_id=seq_id, enrollment_id=eid,
            lead_id=lead_id, step_number=0, to_email=to_email, subject="Hi",
            status="sent", message_id=message_id, idempotency_key=idem,
        ))


# ── DSN/ARF fixture builders ─────────────────────────────────────────────────

def _dsn(original_mid, status="5.1.1", action="failed", recipient="bob@example.com",
         dsn_mid="<dsn-1@mx.example.com>"):
    return (
        f"From: MAILER-DAEMON@mx.example.com\n"
        f"To: sender@acme.com\n"
        f"Subject: Mail delivery failed\n"
        f"Message-ID: {dsn_mid}\n"
        f"Content-Type: multipart/report; report-type=delivery-status; boundary=\"B\"\n\n"
        f"--B\nContent-Type: text/plain\n\ndelivery failed\n"
        f"--B\nContent-Type: message/delivery-status\n\n"
        f"Reporting-MTA: dns; mx.example.com\n\n"
        f"Final-Recipient: rfc822; {recipient}\n"
        f"Action: {action}\nStatus: {status}\n"
        f"Diagnostic-Code: smtp; 550 {status} user unknown\n\n"
        f"--B\nContent-Type: message/rfc822\n\n"
        f"Message-ID: {original_mid}\nFrom: sender@acme.com\nTo: {recipient}\nSubject: Hi\n\nhello\n"
        f"--B--\n"
    ).encode()


def _arf(original_mid, recipient="bob@example.com", arf_mid="<arf-1@isp.com>"):
    return (
        f"From: feedback@isp.com\nTo: sender@acme.com\nSubject: complaint\n"
        f"Message-ID: {arf_mid}\n"
        f"Content-Type: multipart/report; report-type=feedback-report; boundary=\"B\"\n\n"
        f"--B\nContent-Type: text/plain\n\ncomplaint\n"
        f"--B\nContent-Type: message/feedback-report\n\n"
        f"Feedback-Type: abuse\nVersion: 1\nOriginal-Rcpt-To: {recipient}\n\n"
        f"--B\nContent-Type: message/rfc822\n\n"
        f"Message-ID: {original_mid}\nFrom: sender@acme.com\nTo: {recipient}\nSubject: Hi\n\nhello\n"
        f"--B--\n"
    ).encode()


# ── Fake IMAP client ─────────────────────────────────────────────────────────

class FakeImap:
    def __init__(self, messages, uidvalidity="100", fail_on_connect=False):
        # messages: dict[uid_str] -> raw bytes
        self.messages = messages
        self.uidvalidity = uidvalidity
        self.fail_on_connect = fail_on_connect
        self.seen = []
        self.connected = False
        self.logged_out = False

    def connect(self):
        if self.fail_on_connect:
            raise OSError("IMAP auth failed")
        self.connected = True

    def select(self, folder):
        return self.uidvalidity

    def search_unseen(self, since):
        return [u for u in self.messages if u not in self.seen]

    def fetch(self, uid):
        return self.messages.get(uid, b"")

    def mark_seen(self, uid):
        self.seen.append(uid)

    def logout(self):
        self.logged_out = True


def _install_imap(monkeypatch, fake):
    import apps.api.services.outreach.inbound as inbound_mod
    from apps.api.services.outreach.sender import IMAPConfig
    monkeypatch.setattr(inbound_mod, "get_imap_config",
                        lambda ws=None: IMAPConfig(host="imap.x", port=993, user="me@x.com", password="pw"))
    monkeypatch.setattr(inbound_mod, "_make_client", lambda cfg: fake)


def _store():
    from apps.api.services.outreach.store import PgOutreachStore
    return PgOutreachStore(WS)


# ════════════════════════════ dsn_parse unit ════════════════════════════════

def test_parse_rfc3464_hard():
    from apps.api.services.outreach import dsn_parse
    recs = dsn_parse.parse_message(_dsn("<orig-1@acme.com>", status="5.1.1"))
    assert len(recs) == 1
    assert recs[0].kind == "hard"
    assert recs[0].original_message_id == "<orig-1@acme.com>"
    assert recs[0].recipient == "bob@example.com"


def test_parse_rfc3464_soft():
    from apps.api.services.outreach import dsn_parse
    recs = dsn_parse.parse_message(_dsn("<orig-2@acme.com>", status="4.2.2", action="delayed"))
    assert recs[0].kind == "soft"


def test_parse_arf_complaint():
    from apps.api.services.outreach import dsn_parse
    recs = dsn_parse.parse_message(_arf("<orig-3@acme.com>"))
    assert recs[0].kind == "complaint"
    assert recs[0].original_message_id == "<orig-3@acme.com>"


def test_parse_heuristic_plaintext():
    from apps.api.services.outreach import dsn_parse
    raw = (
        "From: postmaster@x.com\nTo: sender@acme.com\nSubject: Undeliverable\n"
        "Content-Type: text/plain\n\n"
        "Your message to bob@example.com could not be delivered.\n"
        "550 5.1.1 user unknown\nMessage-ID: <orig-h@acme.com>\n"
    ).encode()
    recs = dsn_parse.parse_message(raw)
    assert recs and recs[0].kind == "hard"
    assert recs[0].original_message_id == "<orig-h@acme.com>"


def test_parse_non_dsn_returns_empty():
    from apps.api.services.outreach import dsn_parse
    raw = ("From: bob@example.com\nTo: sender@acme.com\nSubject: Re: Hi\n"
           "Content-Type: text/plain\n\nThanks, sounds good!\n").encode()
    assert dsn_parse.parse_message(raw) == []


# ════════════════ Message-ID prerequisite (AC1) ═════════════════════════════

def test_send_email_emits_server_message_id(monkeypatch):
    """send_email must set + return a non-empty server Message-ID (was always '')."""
    import apps.api.services.outreach.sender as sender_mod

    captured = {}

    class _FakeSMTP:
        def __init__(self, host, port):
            pass
        def __enter__(self):
            return self
        def __exit__(self, *a):
            return False
        def starttls(self):
            pass
        def login(self, u, p):
            pass
        def send_message(self, msg):
            captured["msg"] = msg

    import smtplib
    monkeypatch.setattr(smtplib, "SMTP", _FakeSMTP)

    cfg = sender_mod.SMTPConfig(host="smtp.x", port=587, email="me@acme.com", password="pw")
    res = _run(sender_mod.send_email(
        to_email="bob@example.com", subject="Hi", body_html="<p>Hi</p>",
        body_text="Hi", config=cfg,
    ))
    assert res.success
    assert res.message_id and res.message_id.startswith("<") and "@acme.com>" in res.message_id
    assert captured["msg"]["Message-ID"] == res.message_id


# ════════════════ End-to-end via fake IMAP (AC3/4) ═══════════════════════════

def test_imap_hard_bounce_end_to_end(SL, monkeypatch):
    store = _store()
    seq = _mk_active_seq(store)
    eid = store.enroll(seq["id"], 1, "bob@example.com")
    mid = "<orig-e2e@acme.com>"
    _seed_sent(store, seq["id"], eid, mid)

    fake = FakeImap({"11": _dsn(mid, status="5.1.1")})
    _install_imap(monkeypatch, fake)

    _run(_handle())

    send = store.get_send_by_message_id(mid)
    assert send["status"] == "bounced"
    assert store.is_suppressed("bob@example.com")
    enr = store.get_enrollment(eid)
    assert enr.status == "bounced"
    assert store.get_sequence(seq["id"])["bounce_count"] == 1
    assert fake.seen == ["11"]  # marked \Seen
    assert store.inbound_message_count() == 1


def test_imap_complaint(SL, monkeypatch):
    store = _store()
    seq = _mk_active_seq(store)
    eid = store.enroll(seq["id"], 1, "bob@example.com")
    mid = "<orig-cmpl@acme.com>"
    _seed_sent(store, seq["id"], eid, mid)

    _install_imap(monkeypatch, FakeImap({"21": _arf(mid)}))
    _run(_handle())

    assert store.get_send_by_message_id(mid)["status"] == "bounced"
    assert store.get_sequence(seq["id"])["complaint_count"] == 1
    # Complaint suppression is LOCKED.
    sup = store.list_suppressions()
    assert sup and sup[0]["reason"] == "complaint" and sup[0]["locked"] is True


# ════════════════ Idempotency (AC7) ══════════════════════════════════════════

def test_imap_idempotent_double_poll(SL, monkeypatch):
    store = _store()
    seq = _mk_active_seq(store)
    eid = store.enroll(seq["id"], 1, "bob@example.com")
    mid = "<orig-idem@acme.com>"
    _seed_sent(store, seq["id"], eid, mid)

    msg = _dsn(mid, status="5.1.1")
    # Two separate polls of the SAME message (re-fetch UNSEEN both times).
    fake1 = FakeImap({"31": msg})
    _install_imap(monkeypatch, fake1)
    _run(_handle())
    fake2 = FakeImap({"31": msg})  # same uid + same source Message-ID
    _install_imap(monkeypatch, fake2)
    _run(_handle())

    assert store.get_sequence(seq["id"])["bounce_count"] == 1  # exactly once
    assert store.inbound_message_count() == 1


# ════════════════ Spoof-safety / unmatched (AC8) ═════════════════════════════

def test_imap_unmatched_message_id_not_acted(SL, monkeypatch):
    store = _store()
    seq = _mk_active_seq(store)
    eid = store.enroll(seq["id"], 1, "bob@example.com")
    _seed_sent(store, seq["id"], eid, "<orig-real@acme.com>")

    # DSN references a Message-ID we never sent → must NOT act (anti-spoof).
    _install_imap(monkeypatch, FakeImap({"41": _dsn("<spoofed-not-ours@evil.com>")}))
    _run(_handle())

    assert store.get_sequence(seq["id"])["bounce_count"] == 0
    assert not store.is_suppressed("bob@example.com")
    # Ledgered as unmatched.
    with SL() as s:
        row = s.query(OutreachInboundMessage).first()
    assert row is not None and row.kind == "unmatched"


def test_imap_unknown_format_no_breaker(SL, monkeypatch):
    store = _store()
    seq = _mk_active_seq(store)
    store.enroll(seq["id"], 1, "bob@example.com")

    reply = ("From: bob@example.com\nTo: sender@acme.com\nSubject: Re: Hi\n"
             "Content-Type: text/plain\n\nThanks!\n").encode()
    _install_imap(monkeypatch, FakeImap({"51": reply}))
    _run(_handle())

    assert store.get_sequence(seq["id"])["bounce_count"] == 0
    with SL() as s:
        row = s.query(OutreachInboundMessage).first()
    assert row is not None and row.kind == "unknown"


# ════════════════ Soft-bounce accumulation (AC6) ═════════════════════════════

def test_imap_soft_bounce_accumulates(SL, monkeypatch):
    monkeypatch.setattr(settings, "OUTREACH_SOFT_BOUNCE_MAX", 2, raising=False)
    store = _store()
    seq = _mk_active_seq(store)
    eid = store.enroll(seq["id"], 1, "bob@example.com")
    mid = "<orig-soft@acme.com>"
    _seed_sent(store, seq["id"], eid, mid)

    # First soft bounce → counter 1, not suppressed.
    _install_imap(monkeypatch, FakeImap({"61": _dsn(mid, status="4.2.2", action="delayed",
                                                    dsn_mid="<dsn-soft-1@mx>")}))
    _run(_handle())
    assert not store.is_suppressed("bob@example.com")
    assert store.get_enrollment(eid).soft_bounce_count == 1

    # Second soft bounce (distinct message) → reaches max → suppressed.
    _install_imap(monkeypatch, FakeImap({"62": _dsn(mid, status="4.2.2", action="delayed",
                                                    dsn_mid="<dsn-soft-2@mx>")}))
    _run(_handle())
    assert store.is_suppressed("bob@example.com")


# ════════════════ IMAP failure → backoff + auto-disable (AC9) ════════════════

def test_imap_failure_backoff_and_disable(SL, monkeypatch):
    monkeypatch.setattr(settings, "OUTREACH_INBOUND_MAX_CONSECUTIVE_FAILURES", 3, raising=False)
    _store()
    fake = FakeImap({}, fail_on_connect=True)
    _install_imap(monkeypatch, fake)

    _run(_handle())
    sched = _store().get_inbound_schedule()
    assert sched.consecutive_failures == 1 and sched.enabled is True
    assert sched.next_poll_at is not None  # backoff reschedule

    _run(_handle())
    _run(_handle())  # third failure hits the cap
    sched = _store().get_inbound_schedule()
    assert sched.consecutive_failures == 3
    assert sched.enabled is False and sched.next_poll_at is None


def test_disabled_when_imap_not_configured(SL, monkeypatch):
    import apps.api.services.outreach.inbound as inbound_mod
    from apps.api.services.outreach.sender import IMAPConfig
    monkeypatch.setattr(inbound_mod, "get_imap_config", lambda ws=None: IMAPConfig())
    _run(inbound_mod.handle_inbound_poll(1, {"workspace_id": WS}))
    sched = _store().get_inbound_schedule()
    assert sched is not None and sched.enabled is False


def test_feature_flag_off_is_noop(SL, monkeypatch):
    monkeypatch.setattr(settings, "OUTREACH_INBOUND_POLL_ENABLED", False, raising=False)
    import apps.api.services.outreach.inbound as inbound_mod
    _install_imap(monkeypatch, FakeImap({"1": _dsn("<x@acme.com>")}))
    _run(inbound_mod.handle_inbound_poll(1, {"workspace_id": WS}))
    assert _store().get_inbound_schedule() is None  # nothing scheduled / touched


# ════════════════ Webhook refactor regression (AC12) ════════════════════════

def test_webhook_marks_send_and_idempotent(SL, monkeypatch):
    """The refactored bounce_webhook marks the send row + is idempotent."""
    from apps.api.routers import outreach as outreach_router
    # The cross-tenant lookup imports SessionLocal from apps.api.database at call
    # time; the per-ws writes go through the (already-patched) store SessionLocal.
    import apps.api.database as database
    monkeypatch.setattr(database, "SessionLocal", SL, raising=False)
    monkeypatch.setattr(settings, "OUTREACH_BOUNCE_WEBHOOK_SECRET", "shh", raising=False)

    store = _store()
    seq = _mk_active_seq(store)
    eid = store.enroll(seq["id"], 1, "bob@example.com")
    mid = "<orig-webhook@acme.com>"
    _seed_sent(store, seq["id"], eid, mid)

    class _Req:
        headers = {"X-Webhook-Secret": "shh"}

    event = outreach_router.BounceEvent(message_id=mid, event="bounce", hard=True)
    r1 = outreach_router.bounce_webhook(event, _Req())
    assert r1["status"] == "ok"
    assert store.get_send_by_message_id(mid)["status"] == "bounced"  # now marks the row
    assert store.get_sequence(seq["id"])["bounce_count"] == 1

    # Replay → idempotent (no double count).
    r2 = outreach_router.bounce_webhook(event, _Req())
    assert r2["status"] == "duplicate"
    assert store.get_sequence(seq["id"])["bounce_count"] == 1


# ── handler wrapper ──────────────────────────────────────────────────────────

async def _handle():
    from apps.api.services.outreach.inbound import handle_inbound_poll
    await handle_inbound_poll(1, {"workspace_id": WS})
