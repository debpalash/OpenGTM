"""Trigger Engine — unit tests (SQLite, no Postgres required).

Maps to the spec acceptance criteria (AC-#) and the §11 test plan. PG-gated RLS
tests live in tests/test_automations_rls.py.

All external effects are mocked: webhook HTTP via monkeypatched executor, CRM via
monkeypatched output.execute_output_column, provider runs via monkeypatched
run_workbook_enrichment.
"""

import asyncio
import importlib

import pytest
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker

from apps.api.core.config import settings
from apps.api.database import Base
# register tables
from apps.api.services.automations import models as auto_models  # noqa: F401
from apps.api.services.billing import models as billing_models  # noqa: F401
from apps.api.services.workbook import models as wb_models  # noqa: F401
from apps.api.services.workbook import activity_models as act_models  # noqa: F401
from apps.api.services.automations.models import (
    Trigger, TriggerRun, TriggerActionResult, TriggerCapReservation, ScheduledTrigger,
)
from apps.api.services.workbook.models import Workbook, WorkbookRow
from apps.api.services.billing.models import WorkspaceCredit, CreditLedgerEntry

WS = "ws_unit"
WS2 = "ws_other"


@pytest.fixture()
def engine():
    from sqlalchemy.pool import StaticPool
    from apps.api.models import Job
    eng = create_engine(
        "sqlite://", connect_args={"check_same_thread": False}, poolclass=StaticPool,
    )
    Base.metadata.create_all(eng, tables=[
        Trigger.__table__, TriggerRun.__table__, TriggerActionResult.__table__,
        TriggerCapReservation.__table__, ScheduledTrigger.__table__,
        Workbook.__table__, WorkbookRow.__table__,
        WorkspaceCredit.__table__, CreditLedgerEntry.__table__,
        act_models.WorkbookActivity.__table__, Job.__table__,
    ])
    return eng


@pytest.fixture()
def Session(engine, monkeypatch):
    SL = sessionmaker(bind=engine, autoflush=False)
    # The engine + emitters open their OWN SessionLocal() — point those at the
    # in-memory test DB so the whole flow shares one database.
    import apps.api.database as database
    import apps.api.services.automations.engine as eng_mod
    import apps.api.services.automations.events as ev_mod
    monkeypatch.setattr(database, "SessionLocal", SL, raising=False)
    monkeypatch.setattr(eng_mod, "SessionLocal", SL, raising=False)
    return SL


@pytest.fixture()
def db(Session):
    s = Session()
    try:
        yield s
    finally:
        s.close()


@pytest.fixture()
def automations_on(monkeypatch):
    monkeypatch.setattr(settings, "AUTOMATIONS_ENABLED", True, raising=False)


@pytest.fixture()
def paid_provider(monkeypatch):
    """All catalog paid providers are BYOK (cost 0 to the platform). Register a
    synthetic platform-billed (non-BYOK) provider so the spend path is exercised."""
    from apps.api.services.workbook import vendor_catalog as vc
    monkeypatch.setitem(vc.VENDORS, "platform_paid",
                        vc.Vendor("platform_paid", base_cost=0.04, byok=False))
    return "platform_paid"


def _mk_workbook(db, ws=WS, columns=None):
    wb = Workbook(name="WB", workspace_id=ws, columns_config=columns or [
        {"id": "email", "name": "email", "type": "waterfall", "target_field": "email"},
        {"id": "score", "name": "score", "type": "lead_field", "lead_field": "score"},
    ])
    db.add(wb)
    db.commit()
    db.refresh(wb)
    return wb


def _mk_row(db, wb, data, enrichments=None, lead_id=None):
    r = WorkbookRow(workbook_id=wb.id, workspace_id=wb.workspace_id, position=0, data=data,
                    enrichments=enrichments or {}, lead_id=lead_id)
    db.add(r)
    db.commit()
    db.refresh(r)
    return r


def _mk_trigger(db, **kw):
    defaults = dict(
        workspace_id=WS, name="rule", enabled=True, trigger_type="on_row_added",
        trigger_config={}, condition="", actions=[], scope_workbook_ids=[],
        stop_on_error=False,
    )
    defaults.update(kw)
    t = Trigger(**defaults)
    db.add(t)
    db.commit()
    db.refresh(t)
    return t


# ── AC-3 / AC-14: hardened condition evaluator ──────────────────────────────

def test_safe_condition_basic_and_adversarial():
    from apps.api.services.automations.safe_conditions import safe_evaluate_condition as ev
    cols = [{"id": "email", "name": "email"}, {"id": "score", "name": "score"}]

    def cells(**kw):
        return {k: {"value": v} for k, v in kw.items()}

    assert ev("{score} > 70", cells(score="85"), cols).passed is True
    assert ev("{score} > 70", cells(score="10"), cols).passed is False
    assert ev('{email} contains "@"', cells(email="a@b"), cols).passed is True
    assert ev('{score} > 70 AND {email} != ""', cells(score="85", email="a@b"), cols).passed is True
    assert ev('{score} > 70 AND {email} != ""', cells(score="85", email=""), cols).passed is False

    # adversarial: value containing `" AND "` must NOT flip the predicate
    r = ev('{email} == "safe"', cells(email='x" AND "1"=="1'), cols)
    assert r.passed is False

    # unparseable → fail closed with reason
    bad = ev("not valid !!!", cells(email="x"), cols)
    assert bad.passed is False and bad.error

    # unresolved field → fail closed
    assert ev('{nope} == "1"', cells(email="x"), cols).passed is False


def test_validate_condition_rejects_garbage():
    from apps.api.services.automations.safe_conditions import validate_condition, ConditionError
    validate_condition('{score} > 70 AND {email} != ""')  # ok
    with pytest.raises(ConditionError):
        validate_condition("{a} >< 1")


# ── AC-7: re_enrich cost projection uses the real waterfall ─────────────────

def test_project_action_cost_reenrich_matches_billing(paid_provider):
    from apps.api.services.automations import actions as actmod
    from apps.api.services.billing import service as billing

    cols = [{"id": "email", "type": "waterfall", "target_field": "email",
             "waterfall": [paid_provider, "apollo_io"]}]
    action = {"type": "re_enrich", "config": {"column_ids": ["email"]}}
    cost = actmod.project_action_cost(action, cols)
    expected = billing.projected_platform_cost(1, {"email": [paid_provider, "apollo_io"]})
    assert cost == expected and cost > 0

    # webhook/crm project zero
    assert actmod.project_action_cost({"type": "webhook", "config": {}}, cols) == 0.0
    assert actmod.project_action_cost({"type": "push_crm", "config": {}}, cols) == 0.0


def test_project_action_cost_falls_back_to_default_waterfalls(monkeypatch):
    """re_enrich with no explicit waterfall builds providers from DEFAULT_WATERFALLS."""
    from apps.api.services.automations import actions as actmod
    from apps.api.services.workbook import enrichment as enr
    # ensure the default-waterfall path is taken (column has no waterfall/provider)
    cols = [{"id": "revenue_estimate", "type": "waterfall", "target_field": "revenue_estimate"}]
    action = {"type": "re_enrich", "config": {"column_ids": ["revenue_estimate"]}}
    # cost may be 0 if all default providers are BYOK; assert it equals the billing
    # projection built from DEFAULT_WATERFALLS (the contract — same inputs).
    from apps.api.services.billing import service as billing
    by_col = {"revenue_estimate": enr.DEFAULT_WATERFALLS.get("revenue_estimate", [])}
    assert actmod.project_action_cost(action, cols) == billing.projected_platform_cost(1, by_col)


# ── AC-6: cap reservations arithmetic ───────────────────────────────────────

def test_caps_reserve_settle_release(db):
    from apps.api.services.automations import caps
    t = _mk_trigger(db, max_actions_per_day=2, max_spend_usd_per_day=1.0)

    r1 = caps.try_reserve(db, t, WS, 0.4, "idem1")
    assert r1.ok
    caps.settle(db, r1)
    r2 = caps.try_reserve(db, t, WS, 0.4, "idem2")
    assert r2.ok
    caps.settle(db, r2)
    db.commit()
    assert caps.actions_today(db, t.id, WS) == 2

    # 3rd action exceeds max_actions_per_day=2 → refused
    r3 = caps.try_reserve(db, t, WS, 0.1, "idem3")
    assert not r3.ok and r3.reason == "cap"


def test_caps_spend_cap(db):
    from apps.api.services.automations import caps
    t = _mk_trigger(db, max_spend_usd_per_day=0.5)
    r1 = caps.try_reserve(db, t, WS, 0.4, "s1")
    assert r1.ok
    caps.settle(db, r1)
    db.commit()
    r2 = caps.try_reserve(db, t, WS, 0.2, "s2")  # 0.4+0.2 > 0.5
    assert not r2.ok


def test_caps_global_cap(db, monkeypatch):
    from apps.api.services.automations import caps
    monkeypatch.setattr(settings, "AUTOMATIONS_GLOBAL_DAILY_USD", 0.5, raising=False)
    t = _mk_trigger(db)  # no per-rule cap
    r1 = caps.try_reserve(db, t, WS, 0.4, "g1")
    caps.settle(db, r1)
    db.commit()
    r2 = caps.try_reserve(db, t, WS, 0.2, "g2")
    assert not r2.ok and r2.reason == "cap"


def test_caps_idempotent_reservation(db):
    from apps.api.services.automations import caps
    t = _mk_trigger(db, max_actions_per_day=1)
    r1 = caps.try_reserve(db, t, WS, 0.1, "dup")
    assert r1.ok
    # same idem → returns existing, does not double-count
    r2 = caps.try_reserve(db, t, WS, 0.1, "dup")
    assert r2.ok and r2.id == r1.id


# ── engine end-to-end (mocked side effects) ─────────────────────────────────

def _patch_execute_action(monkeypatch, fn):
    from apps.api.services.automations import engine, actions
    monkeypatch.setattr(actions, "execute_action", fn)


async def _run_eval(db_factory, trigger, payload, dry_run=False):
    from apps.api.services.automations import engine
    p = dict(payload)
    p["dry_run"] = dry_run
    await engine.handle_trigger_eval(1, p)


def test_engine_disabled_is_noop(db, Session, monkeypatch):
    monkeypatch.setattr(settings, "AUTOMATIONS_ENABLED", False, raising=False)
    wb = _mk_workbook(db)
    row = _mk_row(db, wb, {"email": "a@b", "score": 90})
    t = _mk_trigger(db, scope_workbook_ids=[wb.id], actions=[{"type": "webhook", "config": {"url": "https://x.test"}}])
    asyncio.run(_run_eval(Session, t, {
        "trigger_id": t.id, "workspace_id": WS, "fire_source": "manual",
        "targets": [{"workbook_id": wb.id, "row_id": str(row.id)}],
    }))
    assert db.query(TriggerRun).count() == 0


def test_engine_ordered_execution_and_partial(db, Session, monkeypatch, automations_on):
    """AC-2/AC-8: ordered exec, non-fatal failure → run partial."""
    from apps.api.services.automations import actions as actmod
    wb = _mk_workbook(db)
    row = _mk_row(db, wb, {"email": "a@b", "score": 90})
    calls = []

    async def fake_exec(ws_id, action, wb_id, row_id, lead_id, lead_data, cols, idem=None):
        calls.append(action["type"])
        if action["type"] == "webhook":
            return actmod.ActionResult("failed", error="boom")
        return actmod.ActionResult("success", summary="ok")

    monkeypatch.setattr(actmod, "execute_action", fake_exec)
    # patch the symbol the engine imports lazily too
    t = _mk_trigger(db, condition='{score} > 50', scope_workbook_ids=[wb.id], actions=[
        {"type": "push_crm", "config": {"type": "hubspot"}},
        {"type": "webhook", "config": {"url": "https://x.test"}},
    ])
    asyncio.run(_run_eval(Session, t, {
        "trigger_id": t.id, "workspace_id": WS, "fire_source": "manual",
        "fire_key": "fk1",
        "targets": [{"workbook_id": wb.id, "row_id": str(row.id)}],
    }))
    run = db.query(TriggerRun).filter(TriggerRun.trigger_id == t.id).first()
    assert run.status == "partial"
    assert run.actions_succeeded == 1 and run.actions_failed == 1
    assert calls == ["push_crm", "webhook"]  # stored order preserved


def test_engine_condition_filters_rows(db, Session, monkeypatch, automations_on):
    """AC-3: non-matching rows produce no actions."""
    from apps.api.services.automations import actions as actmod
    wb = _mk_workbook(db)
    row = _mk_row(db, wb, {"email": "a@b", "score": 10})

    async def fake_exec(*a, **k):
        return actmod.ActionResult("success")

    monkeypatch.setattr(actmod, "execute_action", fake_exec)
    t = _mk_trigger(db, condition='{score} > 50', scope_workbook_ids=[wb.id],
                    actions=[{"type": "webhook", "config": {"url": "https://x.test"}}])
    asyncio.run(_run_eval(Session, t, {
        "trigger_id": t.id, "workspace_id": WS, "fire_source": "manual", "fire_key": "fk",
        "targets": [{"workbook_id": wb.id, "row_id": str(row.id)}],
    }))
    run = db.query(TriggerRun).filter(TriggerRun.trigger_id == t.id).first()
    assert run.matched_rows == 0 and run.actions_attempted == 0


def test_engine_idempotency_no_double_send(db, Session, monkeypatch, automations_on):
    """AC-5: same fire_key twice → second run only replay-skips."""
    from apps.api.services.automations import actions as actmod
    wb = _mk_workbook(db)
    row = _mk_row(db, wb, {"email": "a@b", "score": 90})
    sends = {"n": 0}

    async def fake_exec(*a, **k):
        sends["n"] += 1
        return actmod.ActionResult("success", summary="sent")

    monkeypatch.setattr(actmod, "execute_action", fake_exec)
    t = _mk_trigger(db, condition='{score} > 50', scope_workbook_ids=[wb.id],
                    actions=[{"type": "webhook", "config": {"url": "https://x.test"}}])
    payload = {"trigger_id": t.id, "workspace_id": WS, "fire_source": "row_changed",
               "fire_key": "samekey",
               "targets": [{"workbook_id": wb.id, "row_id": str(row.id)}]}
    asyncio.run(_run_eval(Session, t, payload))
    asyncio.run(_run_eval(Session, t, payload))  # re-fire same key
    assert sends["n"] == 1  # NOT re-sent
    runs = db.query(TriggerRun).filter(TriggerRun.trigger_id == t.id).all()
    assert len(runs) == 2
    # exactly one action_result terminal success row for that idem
    results = db.query(TriggerActionResult).all()
    successes = [r for r in results if r.status == "success"]
    assert len(successes) == 1


def test_engine_dry_run_charges_nothing(db, Session, monkeypatch, automations_on, paid_provider):
    """AC-11: dry-run writes no action_results, no ledger, no reservations."""
    from apps.api.services.automations import actions as actmod
    from apps.api.services.billing import service as billing
    monkeypatch.setattr(settings, "BILLING_ENABLED", True, raising=False)
    billing.credit(db, WS, 10.0, reason="seed", idempotency_key="seed1")

    wb = _mk_workbook(db, columns=[{"id": "email", "type": "waterfall",
                                    "target_field": "email", "waterfall": [paid_provider]}])
    row = _mk_row(db, wb, {"email": ""})
    calls = {"n": 0}

    async def fake_exec(*a, **k):
        calls["n"] += 1
        return actmod.ActionResult("success")

    monkeypatch.setattr(actmod, "execute_action", fake_exec)
    t = _mk_trigger(db, condition="", scope_workbook_ids=[wb.id],
                    actions=[{"type": "re_enrich", "config": {"column_ids": ["email"]}}])
    asyncio.run(_run_eval(Session, t, {
        "trigger_id": t.id, "workspace_id": WS, "fire_source": "manual", "fire_key": "dry",
        "targets": [{"workbook_id": wb.id, "row_id": str(row.id)}],
    }, dry_run=True))
    assert calls["n"] == 1  # action invoked for preview-style execution path
    assert db.query(TriggerActionResult).count() == 0
    assert db.query(TriggerCapReservation).count() == 0
    # ledger only has the seed credit, no debit
    assert billing.get_balance(db, WS) == 10.0


def test_engine_spend_cap_skips(db, Session, monkeypatch, automations_on, paid_provider):
    """AC-6: per-rule cap exceeded → action skipped (reason cap), run partial."""
    from apps.api.services.automations import actions as actmod
    from apps.api.services.billing import service as billing
    monkeypatch.setattr(settings, "BILLING_ENABLED", True, raising=False)
    billing.credit(db, WS, 10.0, reason="seed", idempotency_key="seedcap")

    wb = _mk_workbook(db, columns=[{"id": "email", "type": "waterfall",
                                    "target_field": "email", "waterfall": [paid_provider]}])
    r1 = _mk_row(db, wb, {"email": ""})

    async def fake_exec(*a, **k):
        return actmod.ActionResult("success")

    monkeypatch.setattr(actmod, "execute_action", fake_exec)
    # cap so low even one re_enrich (0.04) exceeds it
    t = _mk_trigger(db, condition="", scope_workbook_ids=[wb.id], max_spend_usd_per_day=0.01,
                    actions=[{"type": "re_enrich", "config": {"column_ids": ["email"]}}])
    asyncio.run(_run_eval(Session, t, {
        "trigger_id": t.id, "workspace_id": WS, "fire_source": "manual", "fire_key": "capk",
        "targets": [{"workbook_id": wb.id, "row_id": str(r1.id)}],
    }))
    res = db.query(TriggerActionResult).first()
    assert res.status == "skipped" and res.skip_reason == "cap"
    assert billing.get_balance(db, WS) == 10.0  # nothing charged


def test_engine_insufficient_credits_skips_and_run_survives(db, Session, monkeypatch, automations_on, paid_provider):
    """AC-8: insufficient credits → action skipped(credits), run row survives."""
    from apps.api.services.automations import actions as actmod
    from apps.api.services.billing import service as billing
    monkeypatch.setattr(settings, "BILLING_ENABLED", True, raising=False)
    # zero balance

    wb = _mk_workbook(db, columns=[{"id": "email", "type": "waterfall",
                                    "target_field": "email", "waterfall": [paid_provider]}])
    r1 = _mk_row(db, wb, {"email": ""})

    async def fake_exec(*a, **k):
        return actmod.ActionResult("success")

    monkeypatch.setattr(actmod, "execute_action", fake_exec)
    t = _mk_trigger(db, condition="", scope_workbook_ids=[wb.id],
                    actions=[{"type": "re_enrich", "config": {"column_ids": ["email"]}}])
    asyncio.run(_run_eval(Session, t, {
        "trigger_id": t.id, "workspace_id": WS, "fire_source": "manual", "fire_key": "nocred",
        "targets": [{"workbook_id": wb.id, "row_id": str(r1.id)}],
    }))
    run = db.query(TriggerRun).filter(TriggerRun.trigger_id == t.id).first()
    assert run is not None  # survived the internal rollback
    res = db.query(TriggerActionResult).first()
    assert res.status == "skipped" and res.skip_reason == "credits"


def test_engine_paid_debits_once(db, Session, monkeypatch, automations_on, paid_provider):
    """AC-7: paid re_enrich debits the ledger exactly once via run:<idem>."""
    from apps.api.services.automations import actions as actmod
    from apps.api.services.billing import service as billing
    monkeypatch.setattr(settings, "BILLING_ENABLED", True, raising=False)
    billing.credit(db, WS, 10.0, reason="seed", idempotency_key="seedpaid")

    wb = _mk_workbook(db, columns=[{"id": "email", "type": "waterfall",
                                    "target_field": "email", "waterfall": [paid_provider]}])
    r1 = _mk_row(db, wb, {"email": ""})

    async def fake_exec(*a, **k):
        return actmod.ActionResult("success", summary="enriched")

    monkeypatch.setattr(actmod, "execute_action", fake_exec)
    t = _mk_trigger(db, condition="", scope_workbook_ids=[wb.id],
                    actions=[{"type": "re_enrich", "config": {"column_ids": ["email"]}}])
    payload = {"trigger_id": t.id, "workspace_id": WS, "fire_source": "manual", "fire_key": "paidk",
               "targets": [{"workbook_id": wb.id, "row_id": str(r1.id)}]}
    asyncio.run(_run_eval(Session, t, payload))
    bal_after = billing.get_balance(db, WS)
    assert bal_after == round(10.0 - 0.04, 4)
    # retry same fire_key → no further debit
    asyncio.run(_run_eval(Session, t, payload))
    assert billing.get_balance(db, WS) == bal_after


# ── events emitters ─────────────────────────────────────────────────────────

def test_on_rows_changed_transition_firekey_and_pause(db, Session, monkeypatch, automations_on):
    """AC-4/AC-14: oscillation yields distinct fire_keys; paused rule → no enqueue."""
    from apps.api.services.automations import events
    enqueued = []

    def fake_add_job(self, db_, jtype, payload, priority=1):
        enqueued.append(payload)

        class _J:  # noqa
            id = len(enqueued)
        return _J()

    from apps.api.services.queue_service import QueueService
    monkeypatch.setattr(QueueService, "add_job", fake_add_job)

    wb = _mk_workbook(db)
    t = _mk_trigger(db, trigger_type="on_row_changed",
                    trigger_config={"watch_fields": ["email"]}, scope_workbook_ids=[wb.id],
                    actions=[{"type": "webhook", "config": {"url": "https://x.test"}}])

    # empty -> A
    events.on_rows_changed(WS, wb.id, [{"row_id": "5", "field": "email", "old": "", "new": "A", "cell_version": 1}], db=db)
    # A -> empty
    events.on_rows_changed(WS, wb.id, [{"row_id": "5", "field": "email", "old": "A", "new": "", "cell_version": 2}], db=db)
    # empty -> A again (genuine re-transition, distinct cell_version)
    events.on_rows_changed(WS, wb.id, [{"row_id": "5", "field": "email", "old": "", "new": "A", "cell_version": 3}], db=db)
    keys = [p["fire_key"] for p in enqueued]
    assert len(keys) == 3 and len(set(keys)) == 3  # all distinct

    # non-watched field → no enqueue
    before = len(enqueued)
    events.on_rows_changed(WS, wb.id, [{"row_id": "5", "field": "phone", "old": "", "new": "1", "cell_version": 4}], db=db)
    assert len(enqueued) == before

    # paused rule → no enqueue
    t.enabled = False
    db.commit()
    events.on_rows_changed(WS, wb.id, [{"row_id": "5", "field": "email", "old": "x", "new": "y", "cell_version": 5}], db=db)
    assert len(enqueued) == before


def test_emit_row_added(db, monkeypatch, automations_on):
    from apps.api.services.automations import events
    enqueued = []

    def fake_add_job(self, db_, jtype, payload, priority=1):
        enqueued.append(payload)

        class _J:
            id = 1
        return _J()

    from apps.api.services.queue_service import QueueService
    monkeypatch.setattr(QueueService, "add_job", fake_add_job)

    wb = _mk_workbook(db)
    _mk_trigger(db, trigger_type="on_row_added", scope_workbook_ids=[wb.id],
                actions=[{"type": "webhook", "config": {"url": "https://x.test"}}])
    n = events.emit_row_added(WS, wb.id, [10, 11], db=db)
    assert n == 1 and len(enqueued) == 1
    assert enqueued[0]["fire_source"] == "row_added"


# ── AC-12: handler registration + timeout ───────────────────────────────────

def test_handler_registered_in_main_and_worker_and_timeout():
    from apps.api.services.queue_service import JOB_TIMEOUTS
    assert JOB_TIMEOUTS.get("trigger_eval") == 600
    import apps.api.worker as worker
    import apps.api.main as main  # noqa: F401
    src_worker = importlib.import_module("apps.api.worker")
    # both register trigger_eval (textual check of the wiring is the contract)
    import inspect
    assert "trigger_eval" in inspect.getsource(src_worker._register_handlers)
    assert "trigger_eval" in inspect.getsource(main.lifespan)


# ── scheduling helpers (AC-9/AC-17) ─────────────────────────────────────────

def test_compute_next_run_wall_clock_aligned():
    from datetime import datetime, timezone, timedelta
    from apps.api.services.automations.engine import compute_next_run
    anchor = datetime(2026, 1, 1, 9, 0, tzinfo=timezone.utc)
    now = datetime(2026, 1, 3, 10, 0, tzinfo=timezone.utc)
    nxt = compute_next_run(anchor, "daily", now=now)
    # next daily run after now, aligned to 09:00 → 2026-01-04 09:00
    assert nxt == datetime(2026, 1, 4, 9, 0, tzinfo=timezone.utc)


def test_schedule_single_flight(db, automations_on, monkeypatch):
    """AC-17: a duplicate enqueue with the same fire_key is suppressed."""
    from apps.api.services.automations import engine
    from apps.api.models import Job
    Base.metadata.create_all(db.bind, tables=[Job.__table__])
    from datetime import datetime, timezone
    t = _mk_trigger(db, trigger_type="on_schedule", trigger_config={"interval": "daily"},
                    schedule_anchor=datetime.now(timezone.utc))
    next_run = engine.compute_next_run(t.schedule_anchor, "daily")
    assert engine._enqueue_schedule_if_absent(db, t, next_run) is True
    # second call with same next_run → no new job (single-flight)
    assert engine._enqueue_schedule_if_absent(db, t, next_run) is False
    n = db.query(Job).filter(Job.type == "trigger_eval").count()
    assert n == 1
