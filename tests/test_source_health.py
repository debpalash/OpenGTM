"""Source health-check tests (sourcing P2/P3) — offline, deterministic.

Covers the acceptance criteria:
  - transition state machine: degrade@3, auto_disable@6, recover@2
  - systemic-outage guard: a high-zero-ratio run applies NO zero transitions
  - gate is OBSERVE-ONLY by default: ENFORCE off → nothing is ever excluded
    (a chronically-0 / auto_disabled source still runs); ENFORCE on → excluded
  - manual override: operator enable clears auto_disabled + pins manual_override
  - scheduling: bootstrap enqueues exactly one job (single-flight); handler
    self-reschedules; bounded probe volume
  - probe yield: junk-filtered count, max-over-canary
Network is never hit — _ddg_search / _run_probe are monkeypatched.
"""

import asyncio

import pytest
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.core.config import settings
from apps.api.services.leadgen import source_health as sh
from apps.api.services.leadgen.source_health import (
    SourceHealth,
    CANARY_QUERIES,
    _apply_transition,
    is_health_disabled,
    health_disabled_names,
    manual_enable,
    reset_health,
    health_summary,
    handle_source_health_check,
    bootstrap_source_health,
)


# ── fixtures ──────────────────────────────────────────────────────────────
@pytest.fixture
def Session(monkeypatch):
    """In-memory SQLite shared across sessions, with source_health + jobs tables.
    SessionLocal is monkeypatched so the module's handler/gate/scheduler use it."""
    engine = create_engine(
        "sqlite://",
        connect_args={"check_same_thread": False},
        poolclass=StaticPool,
    )
    from apps.api.models import Job
    SourceHealth.__table__.create(bind=engine)
    Job.__table__.create(bind=engine)
    Maker = sessionmaker(bind=engine)
    monkeypatch.setattr(sh, "SessionLocal", Maker)
    return Maker


@pytest.fixture
def enforce_on(monkeypatch):
    monkeypatch.setattr(settings, "SOURCE_HEALTH_ENFORCE", True)


@pytest.fixture
def enforce_off(monkeypatch):
    monkeypatch.setattr(settings, "SOURCE_HEALTH_ENFORCE", False)


@pytest.fixture
def probe_on(monkeypatch):
    monkeypatch.setattr(settings, "SOURCE_HEALTH_ENABLED", True)


def _row(**kw):
    base = dict(name="s", state="healthy", consecutive_zero=0,
                consecutive_nonzero=0, probes=0, manual_override=False)
    base.update(kw)
    return SourceHealth(**base)


# ── transition state machine (pure) ───────────────────────────────────────
def test_degrade_at_threshold():
    r = _row()
    for _ in range(2):
        _apply_transition(r, 0, outage=False)
    assert r.state == "healthy"           # 2 zeros < degrade(3)
    _apply_transition(r, 0, outage=False)
    assert r.state == "degraded"          # 3rd zero
    assert r.consecutive_zero == 3


def test_auto_disable_at_threshold():
    r = _row()
    for _ in range(5):
        _apply_transition(r, 0, outage=False)
    assert r.state == "degraded"
    _apply_transition(r, 0, outage=False)  # 6th zero
    assert r.state == "auto_disabled"
    assert r.disabled_reason == "zero_yield"
    assert r.disabled_at is not None
    assert r.consecutive_zero == 6


def test_recover_after_two_nonzero():
    r = _row(state="auto_disabled", consecutive_zero=6, disabled_reason="zero_yield")
    _apply_transition(r, 5, outage=False)
    assert r.state == "auto_disabled"      # 1 nonzero < recover(2)
    assert r.consecutive_zero == 0
    _apply_transition(r, 3, outage=False)
    assert r.state == "healthy"            # 2nd nonzero recovers
    assert r.disabled_at is None and r.disabled_reason is None
    assert r.last_ok_at is not None


def test_degraded_recovers_immediately_on_nonzero():
    r = _row(state="degraded", consecutive_zero=3)
    _apply_transition(r, 1, outage=False)
    assert r.state == "healthy"


def test_ewma_and_last_yield_recorded():
    r = _row()
    _apply_transition(r, 4, outage=False)
    assert r.last_yield == 4 and r.ewma_yield == 4.0 and r.probes == 1
    _apply_transition(r, 0, outage=False)
    assert r.last_yield == 0
    assert 0.0 < r.ewma_yield < 4.0        # EWMA decays toward 0
    assert r.probes == 2


# ── systemic-outage guard ─────────────────────────────────────────────────
def test_outage_suppresses_zero_transitions():
    r = _row()
    # Even many zeros during an outage never advance the disable counter.
    for _ in range(10):
        _apply_transition(r, 0, outage=True)
    assert r.state == "healthy"
    assert r.consecutive_zero == 0
    assert r.last_outage_at is not None


def test_outage_nonzero_still_recovers():
    r = _row(state="auto_disabled", consecutive_zero=6)
    _apply_transition(r, 2, outage=True)
    _apply_transition(r, 2, outage=True)
    assert r.state == "healthy"            # recovery advances even in an outage


def test_manual_override_never_auto_disables():
    r = _row(manual_override=True)
    for _ in range(20):
        _apply_transition(r, 0, outage=False)
    assert r.state == "degraded"           # may degrade, never auto_disabled
    assert r.disabled_reason is None


# ── gate: OBSERVE-ONLY by default ─────────────────────────────────────────
def test_gate_observe_only_off_never_excludes(Session, enforce_off):
    """AC#1: ENFORCE off → a chronically-0 (auto_disabled) source still runs."""
    with Session() as db:
        db.add(_row(name="dead", state="auto_disabled", consecutive_zero=9,
                    disabled_reason="zero_yield"))
        db.commit()
    assert health_disabled_names() == set()   # short-circuit, no exclusion
    assert is_health_disabled("dead") is False


def test_gate_enforce_on_excludes_auto_disabled(Session, enforce_on):
    with Session() as db:
        db.add(_row(name="dead", state="auto_disabled"))
        db.add(_row(name="alive", state="healthy"))
        db.commit()
    assert health_disabled_names() == {"dead"}
    assert is_health_disabled("dead") is True
    assert is_health_disabled("alive") is False


def test_gate_missing_row_fails_open(Session, enforce_on):
    assert is_health_disabled("never_probed") is False  # missing → healthy


# ── manual override via operator enable ───────────────────────────────────
def test_manual_enable_clears_auto_disabled_and_pins(Session, enforce_on):
    with Session() as db:
        db.add(_row(name="dead", state="auto_disabled", consecutive_zero=9,
                    disabled_reason="zero_yield"))
        db.commit()
    out = manual_enable("dead")
    assert out["state"] == "healthy"
    assert out["manual_override"] is True
    assert out["disabled_reason"] is None
    assert health_disabled_names() == set()   # no longer excluded


def test_reset_health(Session):
    with Session() as db:
        db.add(_row(name="x", state="auto_disabled", consecutive_zero=9,
                    manual_override=True, disabled_reason="zero_yield"))
        db.commit()
    out = reset_health("x")
    assert out["state"] == "healthy"
    assert out["manual_override"] is False
    assert out["consecutive_zero"] == 0


# ── probe yield (junk-filtered, max-over-canary) ──────────────────────────
def test_run_probe_max_over_canary_and_junk_filter(monkeypatch):
    from apps.api.services.leadgen import job_runner

    async def fake_ddg(query, max_results=8):
        # The Bangalore canary returns 3 usable + 1 junk; New York returns 1.
        if "Bangalore" in query:
            return [
                {"href": "https://a.com/co1"},
                {"href": "https://b.com/co2"},
                {"href": "https://c.com/co3"},
                {"href": "https://x.com/banner.pdf"},  # junk → filtered
            ]
        return [{"href": "https://d.com/co"}]

    monkeypatch.setattr(job_runner, "_ddg_search", fake_ddg)
    source = {"name": "demo", "query_templates": ["site:demo.com {q} {city}"]}
    y = asyncio.run(sh._run_probe(source))
    assert y == 3   # max over canaries, junk dropped


# ── handler: probe → persist → outage guard → self-reschedule ─────────────
def _patch_handler_probe(monkeypatch, yields_by_name):
    sources = [{"name": n, "query_templates": ["site:x {q}"]} for n in yields_by_name]
    monkeypatch.setattr(sh, "_all_probe_sources", lambda: sources)

    async def fake_probe(source):
        return yields_by_name[source["name"]]

    monkeypatch.setattr(sh, "_run_probe", fake_probe)


def test_handler_records_every_source_and_reschedules(Session, probe_on, monkeypatch):
    yields = {"a": 5, "b": 0, "c": 7}
    _patch_handler_probe(monkeypatch, yields)
    asyncio.run(handle_source_health_check(1, {}))
    with Session() as db:
        rows = {r.name: r for r in db.query(SourceHealth).all()}
        assert set(rows) == {"a", "b", "c"}        # AC#2: a row per source
        assert rows["a"].last_yield == 5 and rows["a"].state == "healthy"
        assert rows["b"].last_yield == 0 and rows["b"].consecutive_zero == 1
        from apps.api.models import Job
        pending = db.query(Job).filter(Job.type == "source_health_check").count()
        assert pending == 1                        # AC#8: self-rescheduled


def test_handler_outage_guard_no_disable(Session, probe_on, monkeypatch):
    """AC#5: a run with zero_ratio >= 0.6 records NO zero transitions."""
    # Pre-seed one source already at the brink of disable.
    with Session() as db:
        db.add(_row(name="a", consecutive_zero=5))
        db.commit()
    # 4 of 5 sources zero → ratio 0.8 >= 0.6 → outage.
    yields = {"a": 0, "b": 0, "c": 0, "d": 0, "e": 9}
    _patch_handler_probe(monkeypatch, yields)
    asyncio.run(handle_source_health_check(2, {}))
    with Session() as db:
        rows = {r.name: r for r in db.query(SourceHealth).all()}
        assert rows["a"].state == "healthy"        # NOT disabled despite 6th zero
        assert rows["a"].consecutive_zero == 5     # counter untouched by outage
        assert rows["a"].last_outage_at is not None


def test_handler_disabled_when_master_flag_off(Session, monkeypatch):
    monkeypatch.setattr(settings, "SOURCE_HEALTH_ENABLED", False)
    _patch_handler_probe(monkeypatch, {"a": 0})
    asyncio.run(handle_source_health_check(3, {}))
    with Session() as db:
        assert db.query(SourceHealth).count() == 0          # no recording
        from apps.api.models import Job
        assert db.query(Job).count() == 0                   # no reschedule


# ── scheduling / bootstrap (single-flight) ────────────────────────────────
def test_bootstrap_single_flight(Session, probe_on):
    assert bootstrap_source_health() is True
    assert bootstrap_source_health() is False    # second call no-op
    with Session() as db:
        from apps.api.models import Job
        assert db.query(Job).filter(Job.type == "source_health_check").count() == 1


def test_bootstrap_noop_when_disabled(Session, monkeypatch):
    monkeypatch.setattr(settings, "SOURCE_HEALTH_ENABLED", False)
    assert bootstrap_source_health() is False
    with Session() as db:
        from apps.api.models import Job
        assert db.query(Job).count() == 0


# ── bounded probe volume + summary ────────────────────────────────────────
def test_canary_set_is_bounded():
    assert 1 <= len(CANARY_QUERIES) <= 4         # AC#10: small, fixed fan-out


def test_health_summary_shape(Session, enforce_off):
    with Session() as db:
        db.add(_row(name="a", state="healthy", last_yield=3))
        db.add(_row(name="b", state="auto_disabled"))
        db.commit()
    s = health_summary()
    assert s["total"] == 2
    assert s["enforce"] is False
    assert s["by_state"]["healthy"] == 1
    names = {x["name"] for x in s["sources"]}
    assert names == {"a", "b"}
