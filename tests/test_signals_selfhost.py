"""Self-host ``on_signal`` — scanner → unified ORM store → emit (SQLite, offline).

Maps to the follow-up acceptance criteria:

  * AC-1  on_signal fires exactly once from a scanner-detected signal on SQLite.
  * AC-2  re-scanning unchanged state enqueues ZERO additional trigger_eval
          (deterministic signal id → no dup row → no dup fire).
  * AC-3  every scanner signal carries the correct workspace_id and a signal can
          never map a lead into another workspace's workbook rows (isolation).
  * AC-6  the feed (signal store.get_signals) returns scanner-written signals.
  * AC-7  one workspace failing the scan does not abort the others.

All provider I/O (JobSpy) is mocked; the queue is mocked to capture enqueues.
The PG-gated regression that PgLeadStore.add_signal still fires lives in
tests/test_intent_poller_rls.py (delegation is exercised there).
"""

import asyncio
from types import SimpleNamespace

import pytest
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.core.config import settings
from apps.api.database import Base
from apps.api.services.automations.models import (
    Trigger, TriggerRun, TriggerActionResult, TriggerCapReservation, ScheduledTrigger,
)
from apps.api.services.leadgen.orm_models import SignalRow
from apps.api.services.workbook.models import Workbook, WorkbookRow

WS_A = "ws_alpha"
WS_B = "ws_beta"
LEAD_ID = 4242


# ── fakes ─────────────────────────────────────────────────────────────────────

class _FakeLead:
    def __init__(self, lead_id, company, score=40, tier="warm"):
        self.id = lead_id
        self.company = company
        self.score = score
        self.score_tier = tier


class _FakeLeadStore:
    """Minimal lead-store stand-in (LeadDB/PgLeadStore surface used by the scan)."""

    def __init__(self, leads):
        self._leads = {l.id: l for l in leads}
        self.updates = []

    def get_leads(self, score_tier=None, limit=500, **kw):
        out = [l for l in self._leads.values() if not score_tier or l.score_tier == score_tier]
        return out[:limit]

    def get_lead(self, lead_id):
        return self._leads.get(lead_id)

    def update_lead_fields(self, lead_id, fields):
        self.updates.append((lead_id, fields))
        l = self._leads.get(lead_id)
        if l:
            for k, v in fields.items():
                setattr(l, k, v)

    def close(self):
        pass


class _FakeWS:
    def __init__(self, ws_id, slug):
        self.id = ws_id
        self.slug = slug


def _fake_enrich_factory(total_jobs):
    async def _enrich(self, lead):
        return SimpleNamespace(
            success=True,
            data={"total_jobs": total_jobs, "top_titles": ["Account Executive", "SDR"]},
        )
    return _enrich


# ── fixtures ──────────────────────────────────────────────────────────────────

@pytest.fixture()
def session_factory():
    engine = create_engine(
        "sqlite:///:memory:", connect_args={"check_same_thread": False}, poolclass=StaticPool,
    )
    Base.metadata.create_all(engine, tables=[
        SignalRow.__table__,
        Trigger.__table__, TriggerRun.__table__, TriggerActionResult.__table__,
        TriggerCapReservation.__table__, ScheduledTrigger.__table__,
        Workbook.__table__, WorkbookRow.__table__,
    ])
    return sessionmaker(bind=engine, autoflush=False, autocommit=False)


@pytest.fixture()
def wire(monkeypatch, session_factory):
    """Wire the scan: AUTOMATIONS_ENABLED on, ORM store bound to the test engine,
    queue captured, workspace list + lead store + JobSpy provider mocked.

    Returns a small control object the tests drive."""
    monkeypatch.setattr(settings, "AUTOMATIONS_ENABLED", True, raising=False)

    # Bind the shared signal store + automations emit to the test engine.
    import apps.api.services.signals.store as sig_store_mod
    monkeypatch.setattr(sig_store_mod, "SessionLocal", session_factory)

    # Capture trigger_eval enqueues (avoid needing the jobs table).
    from apps.api.services.queue_service import QueueService
    enqueued = []

    def _fake_add_job(self, db, jtype, payload, priority=1):
        enqueued.append((jtype, payload))
        return SimpleNamespace(id=len(enqueued))

    monkeypatch.setattr(QueueService, "add_job", _fake_add_job)

    ctl = SimpleNamespace(
        enqueued=enqueued,
        session_factory=session_factory,
        workspaces=[_FakeWS(WS_A, "alpha")],
        lead_stores={WS_A: _FakeLeadStore([_FakeLead(LEAD_ID, "Acme Corp")])},
        total_jobs=12,
    )

    import apps.api.services.workspace.manager as ws_mgr
    monkeypatch.setattr(ws_mgr, "list_workspaces", lambda: list(ctl.workspaces))

    import apps.api.services.leadgen.store as lead_store_mod

    def _fake_get_lead_store(ws_id, slug):
        return ctl.lead_stores[ws_id]

    monkeypatch.setattr(lead_store_mod, "get_lead_store", _fake_get_lead_store)

    from apps.api.services.leadgen.enrichment.providers.jobspy_signals import JobSpySignalProvider
    monkeypatch.setattr(
        JobSpySignalProvider, "enrich", _fake_enrich_factory(ctl.total_jobs), raising=True
    )

    return ctl


def _seed_on_signal_rule(session_factory, ws_id, signal_types, scope_workbook_ids=None):
    s = session_factory()
    rule = Trigger(
        workspace_id=ws_id, name="hiring rule", enabled=True,
        trigger_type="on_signal", trigger_config={"signal_types": signal_types},
        actions=[{"type": "webhook", "config": {"url": "https://hooks.example.com/x"}}],
        scope_workbook_ids=scope_workbook_ids or [],
    )
    s.add(rule)
    s.commit()
    rid = rule.id
    s.close()
    return rid


def _seed_workbook_row(session_factory, ws_id, lead_id):
    s = session_factory()
    wb = Workbook(name="wb", workspace_id=ws_id, columns_config=[])
    s.add(wb)
    s.commit()
    wid = wb.id
    s.add(WorkbookRow(workbook_id=wid, workspace_id=ws_id, position=0, lead_id=lead_id, data={}))
    s.commit()
    s.close()
    return wid


def _trigger_evals(enqueued):
    return [p for (t, p) in enqueued if t == "trigger_eval"]


# ── AC-1 / AC-2 ───────────────────────────────────────────────────────────────

def test_scan_fires_on_signal_once_then_dedup(wire):
    """AC-1: a scanner-detected hiring signal fires exactly one trigger_eval.
    AC-2: a second identical scan fires zero more (deterministic id dedup)."""
    from apps.api.services.signals.monitor import run_signal_scan

    _seed_on_signal_rule(wire.session_factory, WS_A, ["hiring"])
    _seed_workbook_row(wire.session_factory, WS_A, LEAD_ID)

    result = asyncio.run(run_signal_scan())
    assert result["signals_found"] == 1, result
    fires = _trigger_evals(wire.enqueued)
    assert len(fires) == 1, fires
    pk = fires[0]["fire_key"]
    assert pk.startswith("signal:")
    assert fires[0]["fire_source"] == "signal"
    # one signal row persisted, stamped with the workspace
    s = wire.session_factory()
    rows = s.query(SignalRow).all()
    assert len(rows) == 1
    assert rows[0].workspace_id == WS_A and rows[0].signal_type == "hiring"
    s.close()

    # AC-2: re-scan unchanged state → no new row, no new fire.
    wire.enqueued.clear()
    asyncio.run(run_signal_scan())
    assert _trigger_evals(wire.enqueued) == []
    s = wire.session_factory()
    assert s.query(SignalRow).count() == 1
    s.close()


def test_no_fire_when_signal_type_unmatched(wire):
    """Rule wants 'funding' only → a 'hiring' scan signal fires nothing."""
    from apps.api.services.signals.monitor import run_signal_scan

    _seed_on_signal_rule(wire.session_factory, WS_A, ["funding"])
    _seed_workbook_row(wire.session_factory, WS_A, LEAD_ID)

    asyncio.run(run_signal_scan())
    assert _trigger_evals(wire.enqueued) == []


# ── AC-3 isolation ────────────────────────────────────────────────────────────

def test_workspace_isolation_no_cross_tenant_fire(wire):
    """AC-3: the lead's workbook row lives in WS_B; the rule + scan are WS_A.
    The signal is stamped WS_A and the WS_A emit (filtered by Workbook.workspace_id
    == WS_A) finds no row → zero fires. The belt filter is the only isolation on
    SQLite (RLS inert) and it holds."""
    from apps.api.services.signals.monitor import run_signal_scan

    _seed_on_signal_rule(wire.session_factory, WS_A, ["hiring"])
    # workbook row for the SAME lead_id but owned by WS_B
    _seed_workbook_row(wire.session_factory, WS_B, LEAD_ID)

    asyncio.run(run_signal_scan())
    assert _trigger_evals(wire.enqueued) == []
    # signal still written, stamped WS_A
    s = wire.session_factory()
    rows = s.query(SignalRow).all()
    assert len(rows) == 1 and rows[0].workspace_id == WS_A
    s.close()


# ── AC-6 feed ─────────────────────────────────────────────────────────────────

def test_feed_returns_scanner_signal(wire):
    """AC-6: the unified store feed returns the scanner-written signal."""
    from apps.api.services.signals.monitor import run_signal_scan
    from apps.api.services.signals.store import get_signal_store

    _seed_workbook_row(wire.session_factory, WS_A, LEAD_ID)
    asyncio.run(run_signal_scan())

    feed = get_signal_store(WS_A).get_signals(signal_type="hiring")
    assert len(feed) == 1
    assert feed[0]["company"] == "Acme Corp"
    assert feed[0]["workspace_id"] == WS_A
    counts = get_signal_store(WS_A).get_signal_counts()
    assert counts["hiring"] == 1 and counts["total"] == 1


# ── AC-7 one workspace failing does not abort others ──────────────────────────

def test_one_workspace_failure_does_not_abort_others(wire, monkeypatch):
    """AC-7: WS_A's lead store raises; WS_B still scanned and fires."""
    from apps.api.services.signals.monitor import run_signal_scan

    class _Boom(_FakeLeadStore):
        def get_leads(self, *a, **k):
            raise RuntimeError("provider down")

    wire.workspaces = [_FakeWS(WS_A, "alpha"), _FakeWS(WS_B, "beta")]
    wire.lead_stores = {
        WS_A: _Boom([]),
        WS_B: _FakeLeadStore([_FakeLead(LEAD_ID, "Beta Inc")]),
    }
    _seed_on_signal_rule(wire.session_factory, WS_B, ["hiring"])
    _seed_workbook_row(wire.session_factory, WS_B, LEAD_ID)

    result = asyncio.run(run_signal_scan())
    assert result["workspaces"] == 1 and result["workspaces_failed"] == 1
    fires = _trigger_evals(wire.enqueued)
    assert len(fires) == 1


# ── emit no-op when AUTOMATIONS disabled (AC-4 tail) ──────────────────────────

def test_no_emit_when_automations_disabled(wire, monkeypatch):
    """Signals still written, but no trigger_eval when the master flag is off."""
    from apps.api.services.signals.monitor import run_signal_scan

    monkeypatch.setattr(settings, "AUTOMATIONS_ENABLED", False, raising=False)
    _seed_on_signal_rule(wire.session_factory, WS_A, ["hiring"])
    _seed_workbook_row(wire.session_factory, WS_A, LEAD_ID)

    asyncio.run(run_signal_scan())
    assert _trigger_evals(wire.enqueued) == []
    s = wire.session_factory()
    assert s.query(SignalRow).count() == 1  # write path unaffected
    s.close()
