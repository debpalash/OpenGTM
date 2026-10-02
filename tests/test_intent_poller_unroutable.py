"""Detected events that cannot be routed to a lead are held, not consumed.

An ordinary company watch needs a lead to deliver to. Before, a watch whose target
matched no lead still advanced its cursor after detecting an event, so the event
was seen as already handled on every later poll and was lost for good.
"""

import asyncio

import pytest
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.core.config import settings
from apps.api.database import Base
from apps.api.models import Job
from apps.api.services.leadgen.orm_models import LeadRow
from apps.api.services.poller import engine as eng
from apps.api.services.poller.models import PollBudgetLedger, WatchSchedule, WatchSubscription
from apps.api.services.poller.sources import DetectedEvent

WS = "ws-unroutable"
BASELINE = {"bootstrapped": True, "hiring_band": "none"}


class _Store:
    """Stands in for the signal store: records what would be delivered."""

    def __init__(self):
        self.signals = []

    def add_signal(self, signal):
        self.signals.append(signal)


def _surge():
    return [DetectedEvent(natural_event_id="fixture-event", signal_type="hiring_surge",
                          title="Synthetic hiring event")]


@pytest.fixture
def Session(monkeypatch):
    engine = create_engine("sqlite:///:memory:", connect_args={"check_same_thread": False},
                           poolclass=StaticPool)
    Base.metadata.create_all(engine, tables=[
        WatchSubscription.__table__, WatchSchedule.__table__, PollBudgetLedger.__table__,
        Job.__table__, LeadRow.__table__,
    ])
    factory = sessionmaker(bind=engine, autoflush=False, autocommit=False)
    monkeypatch.setattr(eng, "SessionLocal", factory)
    monkeypatch.setattr(eng, "_debit_source", lambda *a, **k: True)
    return factory


def _watch(Session, kind="hiring", **kw):
    with Session() as db:
        db.add(WatchSubscription(
            id="w1", workspace_id=WS, kind=kind, target="Synthetic Company", interval="daily",
            cursor=dict(BASELINE), enabled=True, **kw))
        db.commit()


def _state(Session):
    with Session() as db:
        w = db.get(WatchSubscription, "w1")
        return dict(w.cursor), w.last_error, w.consecutive_failures, w.enabled


def _fetch(monkeypatch, events, patch):
    """A fixed answer. A hiring watch also polls a tech source, which reports nothing."""
    def fetch(watch, want_hiring=False, want_tech=False, backfill=False):
        return (events, dict(patch)) if want_hiring else ([], {})
    monkeypatch.setattr(eng.sources, "fetch_hiring_and_tech", fetch)


def _detector(monkeypatch, band="growing", extra=None):
    """Like the real hiring detector: a surge is reported only when the band
    differs from the one the cursor already holds."""
    def fetch(watch, want_hiring=False, want_tech=False, backfill=False):
        if not want_hiring:
            return [], {}
        changed = (watch.cursor or {}).get("hiring_band") != band
        return (_surge() if changed else []), {"hiring_band": band, **(extra or {})}
    monkeypatch.setattr(eng.sources, "fetch_hiring_and_tech", fetch)


def _poll(store, lead_id, src="hiring"):
    return eng._poll_one_source(store, "w1", WS, src, "fire-1", lead_id, False)


def test_event_detected_before_a_lead_exists_is_delivered_once_one_does(Session, monkeypatch):
    _watch(Session)
    store = _Store()
    _detector(monkeypatch)

    # No lead matches the target: nothing is delivered and nothing is consumed.
    assert _poll(store, None) == (0, 0, False, True)
    cursor, last_error, _, _ = _state(Session)
    assert store.signals == []
    assert cursor["hiring_band"] == "none"
    assert last_error == eng.UNROUTABLE_EVENTS

    # A lead now matches, so the same event is detected again and delivered.
    assert _poll(store, 5) == (1, 0, False, False)
    cursor, _, _, _ = _state(Session)
    assert [s.lead_id for s in store.signals] == [5]
    assert cursor["hiring_band"] == "growing"


def test_a_quiet_poll_without_a_lead_still_advances_the_cursor(Session, monkeypatch):
    _watch(Session)
    store = _Store()
    _fetch(monkeypatch, [], {"hiring_band": "growing"})

    assert _poll(store, None) == (0, 0, False, False)
    cursor, last_error, _, _ = _state(Session)
    assert cursor["hiring_band"] == "growing" and last_error is None


def test_sources_that_route_their_own_events_do_not_need_a_watch_lead(Session, monkeypatch):
    _watch(Session, kind="account_group")
    store = _Store()
    import apps.api.services.poller.account_group as account_group
    monkeypatch.setattr(account_group, "fetch_account_group",
                        lambda watch, backfill=False: (_surge(), {"account_group": {"a1": {"seen": 1}}}))

    assert _poll(store, None, src="account_group") == (1, 0, False, False)
    cursor, last_error, _, _ = _state(Session)
    assert len(store.signals) == 1 and "account_group" in cursor and last_error is None


def test_a_collector_failure_keeps_its_own_message_and_the_cursor_is_still_held(Session, monkeypatch):
    _watch(Session)
    store = _Store()
    _detector(monkeypatch, extra={"_collector_failures": ["jobspy timed out"]})

    assert _poll(store, None) == (0, 0, True, True)
    cursor, last_error, _, _ = _state(Session)
    assert cursor["hiring_band"] == "none" and last_error == "jobspy timed out"


# ── the whole poll, including how the watch is rescheduled ───────────────────

@pytest.fixture
def handler(Session, monkeypatch):
    from apps.api.services.leadgen import store as lead_store
    from apps.api.services.workspace import manager as ws_manager

    store = _Store()
    monkeypatch.setattr(settings, "INTENT_POLLER_ENABLED", True, raising=False)
    monkeypatch.setattr(lead_store, "use_pg_store", lambda: True)
    monkeypatch.setattr(lead_store, "get_lead_store", lambda ws, slug: store)
    monkeypatch.setattr(ws_manager, "workspace_slug", lambda ws: "slug")
    _watch(Session)

    def run():
        asyncio.run(eng.handle_watch_poll(1, {"workspace_id": WS, "watch_id": "w1", "fire_key": "fire-1"}))

    return store, run


def test_a_watch_with_no_lead_reports_why_and_is_not_counted_as_failing(Session, handler, monkeypatch):
    store, run = handler
    _detector(monkeypatch)

    run()

    cursor, last_error, failures, enabled = _state(Session)
    assert store.signals == [] and cursor["hiring_band"] == "none"
    assert last_error == "no_matching_lead"
    assert not failures and enabled   # no backoff and no auto-disable for waiting on a lead


def test_creating_the_lead_delivers_the_held_event_and_clears_the_report(Session, handler, monkeypatch):
    store, run = handler
    _detector(monkeypatch)
    run()
    assert store.signals == []

    with Session() as db:
        db.add(LeadRow(workspace_id=WS, company="Synthetic Company", city="Lisbon"))
        db.commit()
        lead_id = db.query(LeadRow.id).scalar()

    run()

    cursor, last_error, failures, _ = _state(Session)
    assert [s.lead_id for s in store.signals] == [lead_id]
    assert cursor["hiring_band"] == "growing"
    assert last_error is None and not failures
