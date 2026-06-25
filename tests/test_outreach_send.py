"""Outreach send-handler + ticker + trigger-engine — behavioral tests (SQLite).

SMTP is ALWAYS mocked. Covers the safety-critical handle_send ordering (§6.2.1):
at-most-once via committed in_flight, debit-on-success, suppression block,
window/rate reschedule (not terminal), hard-bounce classify, ticker single-flight,
trigger-engine sequencer/send_email end-to-end.
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
from apps.api.services.outreach.orm_models import OutreachSend
from apps.api.services.billing.models import WorkspaceCredit, CreditLedgerEntry
from apps.api.services.automations.models import TriggerCapReservation

WS = "ws_send_a"
WS2 = "ws_send_b"


def _run(coro):
    return asyncio.new_event_loop().run_until_complete(coro)


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
def SL(engine, monkeypatch):
    sl = sessionmaker(bind=engine, autoflush=False)
    import apps.api.database as database
    import apps.api.services.outreach.store as store_mod
    import apps.api.services.outreach.sending as sending_mod
    import apps.api.services.queue_service as qs_mod
    monkeypatch.setattr(database, "SessionLocal", sl, raising=False)
    monkeypatch.setattr(store_mod, "SessionLocal", sl, raising=False)
    monkeypatch.setattr(sending_mod, "SessionLocal", sl, raising=False)
    monkeypatch.setattr(qs_mod, "SessionLocal", sl, raising=False)
    # Master switch on for handle_send.
    monkeypatch.setattr(settings, "AUTOMATIONS_ENABLED", True, raising=False)
    return sl


@pytest.fixture()
def env(SL, monkeypatch):
    """Common mocks: SMTP config present, footer present, SMTP sender captured."""
    from apps.api.services.outreach.sender import SMTPConfig
    import apps.api.services.outreach.sending as sending_mod

    cfg = SMTPConfig(host="smtp.x", port=587, email="me@x.com", password="pw", max_per_hour=100)
    monkeypatch.setattr(sending_mod, "_footer", lambda ws: "Acme, 1 Main St, NY")

    # get_smtp_config is imported inside _do_send from sender module → patch there.
    import apps.api.services.outreach.sender as sender_mod
    monkeypatch.setattr(sender_mod, "get_smtp_config", lambda ws=None: cfg)

    calls = {"n": 0, "to": []}

    async def fake_send(**kw):
        calls["n"] += 1
        calls["to"].append(kw.get("to_email"))
        return sender_mod.SendResult(success=True, message_id="<mid-1>")

    monkeypatch.setattr(sender_mod, "send_email", fake_send)
    return calls


def _mk_active_seq(store):
    seq = store.create_sequence(
        "S", steps=[{"step_number": 0, "subject": "Hi", "body_html": "<p>Hi</p>", "delay_hours": 0},
                    {"step_number": 1, "subject": "Bump", "body_html": "<p>Bump</p>", "delay_hours": 24}],
        # All-day window so send-path tests are deterministic regardless of the
        # wall-clock hour they run at (the default 9-18 UTC window otherwise
        # reschedules the send outside business hours). Tests that exercise the
        # window gate monkeypatch _in_send_window explicitly.
        send_window_start=0, send_window_end=24,
        consent_basis="legit")
    store.set_sequence_status(seq["id"], "active")
    return seq


# ── debit-on-success: success → exactly one debit (I3) ──────────────────────

def test_debit_on_success(SL, env, monkeypatch):
    from apps.api.services.outreach.store import PgOutreachStore
    import apps.api.services.billing.service as billing
    monkeypatch.setattr(settings, "OUTREACH_SEND_COST_USD", 0.10, raising=False)
    monkeypatch.setattr(billing, "billing_enabled", lambda: True)
    store = PgOutreachStore(WS)
    # Fund the workspace.
    with SL() as s, s.begin():
        s.add(WorkspaceCredit(workspace_id=WS, balance_usd=5.0))
    seq = _mk_active_seq(store)
    eid = store.enroll(seq["id"], 1, "to@x.com")
    idem = f"seq:{WS}:{seq['id']}:{eid}:0"
    payload = {"workspace_id": WS, "sequence_id": seq["id"], "enrollment_id": eid,
               "lead_id": 1, "step_number": 0, "to_email": "to@x.com", "idempotency_key": idem}
    _run(_handle(WS, payload))
    assert env["n"] == 1
    row = store.get_send_by_idem(idem)
    assert row.status == "sent" and round(row.charged_usd, 2) == 0.10
    # Exactly one ledger debit.
    with SL() as s:
        n = s.query(CreditLedgerEntry).filter(CreditLedgerEntry.reason == "outreach_send").count()
    assert n == 1


# ── failed SMTP → no charge (debit-on-success) ──────────────────────────────

def test_failed_smtp_no_charge(SL, env, monkeypatch):
    from apps.api.services.outreach.store import PgOutreachStore
    import apps.api.services.outreach.sender as sender_mod
    import apps.api.services.billing.service as billing
    monkeypatch.setattr(settings, "OUTREACH_SEND_COST_USD", 0.10, raising=False)
    monkeypatch.setattr(billing, "billing_enabled", lambda: True)

    async def fail_send(**kw):
        return sender_mod.SendResult(success=False, error="Connection refused")
    monkeypatch.setattr(sender_mod, "send_email", fail_send)

    store = PgOutreachStore(WS)
    with SL() as s, s.begin():
        s.add(WorkspaceCredit(workspace_id=WS, balance_usd=5.0))
    seq = _mk_active_seq(store)
    eid = store.enroll(seq["id"], 1, "to@x.com")
    idem = f"seq:{WS}:{seq['id']}:{eid}:0"
    payload = {"workspace_id": WS, "sequence_id": seq["id"], "enrollment_id": eid,
               "lead_id": 1, "step_number": 0, "to_email": "to@x.com", "idempotency_key": idem}
    # Debit-on-success: the spec debits inside handle_send BEFORE send, but the
    # locked decision is debit-on-success. A transient failure re-raises; the
    # ledger must NOT show a permanent charge for a failed send.
    with pytest.raises(RuntimeError):
        _run(_handle(WS, payload))
    row = store.get_send_by_idem(idem)
    assert row.status == "failed"
    # No debit was committed for a send that failed.
    with SL() as s:
        bal = s.query(WorkspaceCredit).filter_by(workspace_id=WS).first().balance_usd
    assert bal == 5.0


# ── at-most-once: in_flight marker → no double-send on retry (I4) ────────────

def test_at_most_once_replay(SL, env):
    from apps.api.services.outreach.store import PgOutreachStore
    store = PgOutreachStore(WS)
    seq = _mk_active_seq(store)
    eid = store.enroll(seq["id"], 1, "to@x.com")
    idem = f"seq:{WS}:{seq['id']}:{eid}:0"
    payload = {"workspace_id": WS, "sequence_id": seq["id"], "enrollment_id": eid,
               "lead_id": 1, "step_number": 0, "to_email": "to@x.com", "idempotency_key": idem}
    _run(_handle(WS, payload))           # first send
    assert env["n"] == 1
    _run(_handle(WS, payload))           # retry with same idem → terminal → skip
    assert env["n"] == 1                 # zero extra SMTP calls


def test_at_most_once_inflight_crash_replay(SL, env):
    """A committed in_flight marker (crash after SMTP, before terminal) → replay
    flips to skipped/replay, zero extra SMTP."""
    from apps.api.services.outreach.store import PgOutreachStore
    store = PgOutreachStore(WS)
    seq = _mk_active_seq(store)
    eid = store.enroll(seq["id"], 1, "to@x.com")
    idem = f"seq:{WS}:{seq['id']}:{eid}:0"
    # Hand-place an in_flight row (simulates crash after marker commit).
    with SL() as s, s.begin():
        s.add(OutreachSend(workspace_id=WS, sequence_id=seq["id"], enrollment_id=eid,
                           to_email="to@x.com", status="in_flight", idempotency_key=idem))
    payload = {"workspace_id": WS, "sequence_id": seq["id"], "enrollment_id": eid,
               "lead_id": 1, "step_number": 0, "to_email": "to@x.com", "idempotency_key": idem}
    _run(_handle(WS, payload))
    assert env["n"] == 0
    row = store.get_send_by_idem(idem)
    assert row.status == "skipped" and row.skip_reason == "replay"


# ── suppression blocks send ─────────────────────────────────────────────────

def test_suppression_blocks_send(SL, env):
    from apps.api.services.outreach.store import PgOutreachStore
    store = PgOutreachStore(WS)
    seq = _mk_active_seq(store)
    eid = store.enroll(seq["id"], 1, "to@x.com")
    store.add_suppression("to@x.com", reason="unsubscribe", locked=True)
    idem = f"seq:{WS}:{seq['id']}:{eid}:0"
    payload = {"workspace_id": WS, "sequence_id": seq["id"], "enrollment_id": eid,
               "lead_id": 1, "step_number": 0, "to_email": "to@x.com", "idempotency_key": idem}
    _run(_handle(WS, payload))
    assert env["n"] == 0
    row = store.get_send_by_idem(idem)
    assert row.status == "skipped" and row.skip_reason == "suppressed"
    assert store.get_enrollment(eid).status == "suppressed"


# ── U13: window/rate skip does NOT write terminal row + reschedules ─────────

def test_u13_window_reschedule_not_terminal(SL, env, monkeypatch):
    from apps.api.services.outreach.store import PgOutreachStore
    import apps.api.services.outreach.sending as sending_mod
    monkeypatch.setattr(sending_mod, "_in_send_window", lambda seq, now=None: False)
    store = PgOutreachStore(WS)
    seq = _mk_active_seq(store)
    eid = store.enroll(seq["id"], 1, "to@x.com")
    idem = f"seq:{WS}:{seq['id']}:{eid}:0"
    payload = {"workspace_id": WS, "sequence_id": seq["id"], "enrollment_id": eid,
               "lead_id": 1, "step_number": 0, "to_email": "to@x.com", "idempotency_key": idem}
    _run(_handle(WS, payload))
    assert env["n"] == 0
    assert store.get_send_by_idem(idem) is None        # NO terminal row
    enr = store.get_enrollment(eid)
    assert enr.status == "scheduled" and enr.next_send_at is not None  # rescheduled


def test_u13_rate_reschedule_not_terminal(SL, env, monkeypatch):
    from apps.api.services.outreach.store import PgOutreachStore
    monkeypatch.setattr(PgOutreachStore, "sent_count_last_hour", lambda self: 10_000)
    store = PgOutreachStore(WS)
    seq = _mk_active_seq(store)
    eid = store.enroll(seq["id"], 1, "to@x.com")
    idem = f"seq:{WS}:{seq['id']}:{eid}:0"
    payload = {"workspace_id": WS, "sequence_id": seq["id"], "enrollment_id": eid,
               "lead_id": 1, "step_number": 0, "to_email": "to@x.com", "idempotency_key": idem}
    _run(_handle(WS, payload))
    assert env["n"] == 0
    assert store.get_send_by_idem(idem) is None
    assert store.get_enrollment(eid).status == "scheduled"


# ── U14: in_flight written when unmetered (cost=0, billing off) ─────────────

def test_u14_inflight_unmetered(SL, env, monkeypatch):
    from apps.api.services.outreach.store import PgOutreachStore
    monkeypatch.setattr(settings, "OUTREACH_SEND_COST_USD", 0.0, raising=False)
    store = PgOutreachStore(WS)
    seq = _mk_active_seq(store)
    eid = store.enroll(seq["id"], 1, "to@x.com")
    idem = f"seq:{WS}:{seq['id']}:{eid}:0"
    payload = {"workspace_id": WS, "sequence_id": seq["id"], "enrollment_id": eid,
               "lead_id": 1, "step_number": 0, "to_email": "to@x.com", "idempotency_key": idem}
    _run(_handle(WS, payload))
    row = store.get_send_by_idem(idem)
    assert row is not None and row.status == "sent"      # marker → terminal even free
    _run(_handle(WS, payload))
    assert env["n"] == 1                                  # replay short-circuits


# ── hard bounce (sync 5xx) → suppress + terminal bounced (I12) ──────────────

def test_hard_bounce_suppresses(SL, env, monkeypatch):
    from apps.api.services.outreach.store import PgOutreachStore
    import apps.api.services.outreach.sender as sender_mod

    async def bounce_send(**kw):
        return sender_mod.SendResult(success=False, error="550 mailbox unavailable")
    monkeypatch.setattr(sender_mod, "send_email", bounce_send)
    store = PgOutreachStore(WS)
    seq = _mk_active_seq(store)
    eid = store.enroll(seq["id"], 1, "to@x.com")
    idem = f"seq:{WS}:{seq['id']}:{eid}:0"
    payload = {"workspace_id": WS, "sequence_id": seq["id"], "enrollment_id": eid,
               "lead_id": 1, "step_number": 0, "to_email": "to@x.com", "idempotency_key": idem}
    _run(_handle(WS, payload))
    row = store.get_send_by_idem(idem)
    assert row.status == "bounced"
    assert store.is_suppressed("to@x.com")
    assert store.get_enrollment(eid).status == "bounced"


# ── advance to next step on success ─────────────────────────────────────────

def test_advance_next_step(SL, env):
    from apps.api.services.outreach.store import PgOutreachStore
    store = PgOutreachStore(WS)
    seq = _mk_active_seq(store)
    eid = store.enroll(seq["id"], 1, "to@x.com")
    idem = f"seq:{WS}:{seq['id']}:{eid}:0"
    payload = {"workspace_id": WS, "sequence_id": seq["id"], "enrollment_id": eid,
               "lead_id": 1, "step_number": 0, "to_email": "to@x.com", "idempotency_key": idem}
    _run(_handle(WS, payload))
    enr = store.get_enrollment(eid)
    assert enr.status == "scheduled" and enr.current_step == 1   # advanced to step 2


# ── ticker single-flight (I14 partial) ──────────────────────────────────────

def test_ticker_single_flight(SL, env, monkeypatch):
    from apps.api.services.outreach.store import PgOutreachStore
    from apps.api.services.outreach.sending import enqueue_due_sends
    store = PgOutreachStore(WS)
    seq = _mk_active_seq(store)
    store.enroll(seq["id"], 1, "to@x.com")
    n1 = enqueue_due_sends(store, WS, seq["id"], 200)
    n2 = enqueue_due_sends(store, WS, seq["id"], 200)   # same step → single-flight
    assert n1 == 1 and n2 == 0
    with SL() as s:
        jobs = s.query(Job).filter(Job.type == "send").all()
    assert len(jobs) == 1


def _mk_trigger_lead_data():
    return {"company": "Acme", "email": "lead@acme.com"}


# ── trigger-engine sequencer end-to-end (I8/I11) ────────────────────────────

def test_trigger_sequencer_e2e(SL, env):
    from apps.api.services.automations.actions import execute_action
    from apps.api.services.outreach.store import PgOutreachStore
    store = PgOutreachStore(WS)
    seq = _mk_active_seq(store)
    res = _run(execute_action(
        WS, {"type": "sequencer", "config": {"sequence_id": seq["id"]}},
        "wb", "row1", 42, _mk_trigger_lead_data(), [], idem="trig:WS:t:wb:row1:0:fk"))
    assert res.status == "success"
    # enrolled in the workspace
    stats = store.get_sequence_stats(seq["id"])
    assert stats["total"] == 1


def test_trigger_send_email_enqueues(SL, env, monkeypatch):
    from apps.api.services.automations.actions import execute_action
    from apps.api.services.outreach.sender import is_smtp_configured
    import apps.api.services.outreach.sender as sender_mod
    monkeypatch.setattr(sender_mod, "is_smtp_configured", lambda ws=None: True)
    from apps.api.services.outreach.store import PgOutreachStore
    store = PgOutreachStore(WS)
    seq = _mk_active_seq(store)
    res = _run(execute_action(
        WS, {"type": "send_email", "config": {"sequence_id": seq["id"], "step_number": 0}},
        "wb", "row1", 42, _mk_trigger_lead_data(), [], idem="trig:WS:t:wb:row1:0:fk"))
    assert res.status == "success"
    with SL() as s:
        jobs = s.query(Job).filter(Job.type == "send").all()
    assert len(jobs) == 1
    # The job uses the canonical seq: key (cross-producer dedup §8.2).
    assert jobs[0].payload["idempotency_key"].startswith("seq:")


# ── handle_send wrapper (imports workspace_scope) ──────────────────────────

async def _handle(ws, payload):
    """Call the inner _do_send under workspace_scope (handle_send wraps the same;
    we exercise the inner path to avoid get_outreach_store's role check on PG)."""
    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.outreach.sending import _do_send
    from apps.api.services.outreach.store import PgOutreachStore
    with workspace_scope(ws):
        store = PgOutreachStore(ws)
        await _do_send(store, ws, payload["idempotency_key"], payload)
