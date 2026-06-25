"""Outreach RLS-hardening — unit tests (SQLite, no Postgres). Spec §12.1.

All SMTP is mocked. Maps to U1-U17 in the spec test plan + the LOCKED-SCOPE
circuit breaker. PG-gated RLS tests live in tests/test_outreach_rls.py.
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
from apps.api.services.billing.models import WorkspaceCredit, CreditLedgerEntry
from apps.api.services.automations.models import TriggerCapReservation

WS = "ws_out_a"
WS2 = "ws_out_b"


def _run(coro):
    return asyncio.get_event_loop().run_until_complete(coro)


@pytest.fixture()
def engine():
    eng = create_engine(
        "sqlite://", connect_args={"check_same_thread": False}, poolclass=StaticPool,
    )
    Base.metadata.create_all(eng, tables=[
        om.OutreachSequence.__table__, om.OutreachEnrollment.__table__,
        om.OutreachSend.__table__, om.OutreachSuppression.__table__,
        om.OutreachSchedule.__table__,
        WorkspaceCredit.__table__, CreditLedgerEntry.__table__,
        TriggerCapReservation.__table__, Job.__table__,
    ])
    return eng


@pytest.fixture()
def patch_db(engine, monkeypatch):
    SL = sessionmaker(bind=engine, autoflush=False)
    import apps.api.database as database
    import apps.api.services.outreach.store as store_mod
    import apps.api.services.outreach.sending as sending_mod
    monkeypatch.setattr(database, "SessionLocal", SL, raising=False)
    monkeypatch.setattr(store_mod, "SessionLocal", SL, raising=False)
    monkeypatch.setattr(sending_mod, "SessionLocal", SL, raising=False)
    return SL


@pytest.fixture()
def store(patch_db):
    from apps.api.services.outreach.store import PgOutreachStore
    return PgOutreachStore(WS)


def _mk_seq(store, steps=None, **kw):
    return store.create_sequence(
        "Seq", steps=steps or [{"step_number": 0, "subject": "Hi {{company}}",
                                "body_html": "<p>Hello {{company}}</p>", "delay_hours": 0}],
        consent_basis="legitimate_interest", **kw)


# ── U1: store CRUD scopes by workspace_id; cross-ws read empty ───────────────

def test_u1_sequence_crud_workspace_scoped(patch_db):
    from apps.api.services.outreach.store import PgOutreachStore
    a, b = PgOutreachStore(WS), PgOutreachStore(WS2)
    seq = _mk_seq(a)
    assert a.get_sequence(seq["id"]) is not None
    assert b.get_sequence(seq["id"]) is None       # cross-ws read empty
    assert [s["id"] for s in b.list_sequences()] == []
    assert a.sequence_exists(seq["id"]) and not b.sequence_exists(seq["id"])


# ── U2: enrollment dedup — second enroll no-op ──────────────────────────────

def test_u2_enroll_dedup_and_snapshot(store):
    seq = _mk_seq(store)
    eid = store.enroll(seq["id"], 7, "Foo@Bar.com", consent_source="manual")
    assert eid is not None
    enr = store.get_enrollment(eid)
    assert enr.to_email_snapshot == "foo@bar.com"   # normalized snapshot
    assert store.enroll(seq["id"], 7, "foo@bar.com") is None  # dedup → no-op


def test_u2b_enroll_rejects_empty_email(store):
    seq = _mk_seq(store)
    with pytest.raises(ValueError):
        store.enroll(seq["id"], 7, "")


# ── U6: project_action_cost ─────────────────────────────────────────────────

def test_u6_project_action_cost(monkeypatch):
    from apps.api.services.automations import actions as actmod
    monkeypatch.setattr(settings, "OUTREACH_SEND_COST_USD", 0.05, raising=False)
    # Engine-level cost is 0 (debit-on-success in handle_send, not on enqueue).
    assert actmod.project_action_cost({"type": "send_email"}, []) == 0.0
    assert actmod.project_action_cost({"type": "sequencer"}, []) == 0.0


# ── U10: empty workspace_id raises ──────────────────────────────────────────

def test_u10_empty_ws_raises():
    from apps.api.services.outreach.store import PgOutreachStore
    from apps.api.core.tenancy import workspace_scope
    with pytest.raises(ValueError):
        PgOutreachStore("")
    with pytest.raises(ValueError):
        with workspace_scope(""):
            pass


# ── U11: SMTP header injection rejected/stripped ────────────────────────────

def test_u11_header_injection():
    from apps.api.services.outreach.sender import sanitize_header_value, validate_recipient
    cleaned = sanitize_header_value("Subject\r\nBcc: evil@x.com")
    assert "\n" not in cleaned and "\r" not in cleaned
    # control chars are stripped (no injection possible).
    assert sanitize_header_value("Hi\rthere") == "Hithere"
    assert validate_recipient("a@b.com")
    assert not validate_recipient("a@b.com\r\nBcc: x@y.com")
    assert not validate_recipient("a@b.com, c@d.com")


# ── U12: email normalization w/ Gmail dot+plus ──────────────────────────────

def test_u12_normalization_gmail(store):
    store.add_suppression("username@gmail.com", reason="unsubscribe", locked=True)
    assert store.is_suppressed("User.Name+promo@gmail.com")   # gmail collapse
    assert store.is_suppressed("u.s.e.r.n.a.m.e@googlemail.com") is False  # diff domain stored
    # Non-gmail matched only on exact canonical form.
    store.add_suppression("a.b@outlook.com", reason="manual")
    assert store.is_suppressed("A.B@outlook.com")
    assert not store.is_suppressed("ab@outlook.com")


# ── U15: un-suppress locked vs removable ────────────────────────────────────

def test_u15_unsuppress_gating(store):
    store.add_suppression("locked@x.com", reason="unsubscribe", locked=True)
    store.add_suppression("free@x.com", reason="bounce", locked=False)
    assert store.remove_suppression("locked@x.com") == "locked"
    assert store.remove_suppression("free@x.com") == "ok"
    assert store.remove_suppression("missing@x.com") == "not_found"


# ── U5: unsubscribe token sign/verify ───────────────────────────────────────

def test_u5_unsubscribe_token():
    from apps.api.services.outreach.tokens import (
        make_unsubscribe_token, verify_unsubscribe_token,
    )
    tok = make_unsubscribe_token(WS, "x@y.com", "seq1")
    p = verify_unsubscribe_token(tok)
    assert p and p["ws"] == WS and p["email"] == "x@y.com"
    assert verify_unsubscribe_token(tok[:-3] + "AAA") is None       # tamper
    expired = make_unsubscribe_token(WS, "x@y.com", ttl_days=-1)
    assert verify_unsubscribe_token(expired) is None                # expired rejected
    assert verify_unsubscribe_token(expired, allow_expired=True) is not None


def test_u5b_token_key_via_master_key(monkeypatch):
    """The token HMAC key resolves via _load_master_key (fails closed on insecure
    default in non-dev)."""
    import apps.api.services.outreach.tokens as tokmod
    called = {}
    real = tokmod._signing_key

    def spy():
        called["yes"] = True
        return real()
    monkeypatch.setattr(tokmod, "_signing_key", spy)
    tokmod.make_unsubscribe_token(WS, "x@y.com")
    assert called.get("yes")


# ── U4 + footer + List-Unsubscribe in BOTH parts ────────────────────────────

def test_u4_build_message_footer_and_unsub(patch_db, monkeypatch):
    import apps.api.services.outreach.sending as sending_mod
    monkeypatch.setattr(sending_mod, "_footer", lambda ws: "Acme Inc, 1 Main St, NY")
    html, text, headers = sending_mod.build_message(
        WS, "x@y.com", "Subj", "<p>Body</p>", "seq1")
    assert "List-Unsubscribe" in headers
    assert "One-Click" in headers["List-Unsubscribe-Post"]
    assert "Acme Inc, 1 Main St, NY" in html and "Acme Inc, 1 Main St, NY" in text
    assert "Unsubscribe" in html and "Unsubscribe:" in text   # link in BOTH parts


def test_u4b_build_message_blocks_without_footer(patch_db, monkeypatch):
    import apps.api.services.outreach.sending as sending_mod
    monkeypatch.setattr(sending_mod, "_footer", lambda ws: "")
    with pytest.raises(ValueError):
        sending_mod.build_message(WS, "x@y.com", "S", "<p>B</p>", "")


# ── U16: send-window enforcement honors tz ──────────────────────────────────

def test_u16_send_window():
    from apps.api.services.outreach.sending import _in_send_window
    from datetime import datetime, timezone
    seq = {"send_window_start": 9, "send_window_end": 18, "send_window_tz": "UTC"}
    assert _in_send_window(seq, datetime(2026, 6, 25, 12, tzinfo=timezone.utc))
    assert not _in_send_window(seq, datetime(2026, 6, 25, 3, tzinfo=timezone.utc))
    # No window configured → always allowed.
    assert _in_send_window({"send_window_start": 0, "send_window_end": 0})


# ── Circuit breaker (LOCKED SCOPE decision 2) ───────────────────────────────

def test_circuit_breaker_autopause(store, monkeypatch):
    monkeypatch.setattr(settings, "OUTREACH_CIRCUIT_MIN_SENDS", 10, raising=False)
    monkeypatch.setattr(settings, "OUTREACH_BOUNCE_PAUSE_RATE", 0.05, raising=False)
    seq = _mk_seq(store)
    sid = seq["id"]
    # Seed 20 sent rows.
    from apps.api.services.outreach.orm_models import OutreachSend
    from datetime import datetime, timezone
    with store._session() as s, s.begin():
        for i in range(20):
            s.add(OutreachSend(workspace_id=WS, sequence_id=sid, to_email=f"{i}@x.com",
                               status="sent", idempotency_key=f"k{i}",
                               sent_at=datetime.now(timezone.utc)))
    # 1 bounce / 20 = 5% → not > 5% → no pause.
    assert store.record_bounce_and_maybe_pause(sid) is False
    # 2nd bounce → 2/20 = 10% > 5% → auto-pause.
    tripped = store.record_bounce_and_maybe_pause(sid)
    assert tripped is True
    assert store.get_sequence(sid)["auto_paused"] is True
    assert store.get_sequence(sid)["status"] == "paused"


def test_circuit_breaker_min_sends_guard(store):
    seq = _mk_seq(store)
    # Below min sends → never pauses even at 100% bounce.
    assert store.record_bounce_and_maybe_pause(seq["id"]) is False
    assert store.get_sequence(seq["id"])["auto_paused"] is False
