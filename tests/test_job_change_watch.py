"""Person-level job-change watch (`job_change` kind → `job_change` signal).

All DDG/lookup I/O is mocked via the ``searcher`` seam. Covers: baseline-set on
first observation (no signal), same company (no signal), changed company
(signal with the correct payload), low-confidence parse (no signal),
company-name normalization, the per-cycle contact cap (+LRU rotation),
workspace isolation of emitted signals, lead-contact materialization scoping,
and the API validation (weekly-minimum interval, contacts required).
"""

import json

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.core.config import settings
from apps.api.database import Base
from apps.api.services.poller import job_change as jc
from apps.api.services.poller.models import WATCH_KINDS

WS_A = "ws_alpha"
WS_B = "ws_beta"


# ── fakes ─────────────────────────────────────────────────────────────────────

class FakeWatch:
    def __init__(self, **kw):
        self.id = kw.get("id", "w-jc")
        self.workspace_id = kw.get("workspace_id", WS_A)
        self.kind = "job_change"
        self.target = kw.get("target", "key accounts roster")
        self.resolved_cik = None
        self.lead_id = kw.get("lead_id")
        self.signal_types = ["job_change"]
        self.interval = "weekly"
        self.cursor = kw.get("cursor", {"bootstrapped": True})
        self.config = kw.get("config", {})


def _contact(name, company="", linkedin_url="", lead_id=None):
    c = {"lead_id": lead_id, "name": name, "company": company, "linkedin_url": linkedin_url}
    c["key"] = jc.contact_key(c)
    return c


def _serp(title, href, body=""):
    return {"title": title, "href": href, "body": body}


def _searcher_returning(results):
    calls = []

    def search(query, *a, **kw):
        calls.append(query)
        return list(results)

    search.calls = calls
    return search


# ── company normalization ─────────────────────────────────────────────────────

def test_normalize_company_strips_legal_suffixes():
    assert jc.normalize_company("Acme, Inc.") == "acme"
    assert jc.normalize_company("Acme LLC") == "acme"
    assert jc.normalize_company("Acme Robotics Pvt. Ltd.") == "acme robotics"
    assert jc.normalize_company("ACME Pte Ltd") == jc.normalize_company("acme")
    assert jc.normalize_company("Acme GmbH") == "acme"
    # different actual companies stay different
    assert jc.normalize_company("Acme Inc") != jc.normalize_company("Acme Robotics Inc")


# ── snippet/title parsing (confidence rules) ──────────────────────────────────

def test_resolver_parses_three_segment_title():
    s = _searcher_returning([_serp(
        "Jane Doe - VP Sales - NewCo | LinkedIn",
        "https://www.linkedin.com/in/janedoe",
    )])
    company, url = jc.resolve_current_company("Jane Doe", searcher=s)
    assert company == "NewCo"
    assert url == "https://www.linkedin.com/in/janedoe"


def test_resolver_parses_title_at_company_and_experience_snippet():
    s = _searcher_returning([_serp(
        "Jane Doe - VP Sales at NewCo | LinkedIn",
        "https://linkedin.com/in/janedoe",
    )])
    company, _ = jc.resolve_current_company("Jane Doe", searcher=s)
    assert company == "NewCo"

    s2 = _searcher_returning([_serp(
        "Jane Doe | LinkedIn",
        "https://linkedin.com/in/janedoe",
        body="Experience: NewCo · Location: Austin",
    )])
    company2, _ = jc.resolve_current_company("Jane Doe", searcher=s2)
    assert company2 == "NewCo"


def test_resolver_low_confidence_returns_none():
    # name-only title, no company pattern anywhere → None
    s = _searcher_returning([_serp("Jane Doe | LinkedIn", "https://linkedin.com/in/janedoe")])
    assert jc.resolve_current_company("Jane Doe", searcher=s) == (None, None)
    # non-linkedin URL is never evidence
    s2 = _searcher_returning([_serp("Jane Doe - VP - NewCo", "https://example.com/janedoe")])
    assert jc.resolve_current_company("Jane Doe", searcher=s2) == (None, None)
    # wrong person (name mismatch, no handle pin) → skipped
    s3 = _searcher_returning([_serp(
        "Janet Dough - VP - NewCo | LinkedIn", "https://linkedin.com/in/janetdough",
    )])
    assert jc.resolve_current_company("Jane Doe", searcher=s3) == (None, None)


def test_resolver_handle_pinning():
    # handle known: a different profile's handle is skipped even if parseable
    s = _searcher_returning([
        _serp("Jane Doe - VP - WrongCo | LinkedIn", "https://linkedin.com/in/otherjane"),
        _serp("Jane Doe - VP - RightCo | LinkedIn", "https://linkedin.com/in/janedoe"),
    ])
    company, url = jc.resolve_current_company(
        "Jane Doe", linkedin_url="https://www.linkedin.com/in/janedoe", searcher=s,
    )
    assert company == "RightCo"
    assert url.endswith("/in/janedoe")


# ── detection state machine ───────────────────────────────────────────────────

def test_first_observation_sets_baseline_no_signal():
    w = FakeWatch()
    contacts = [_contact("Jane Doe", company="Acme Inc",
                         linkedin_url="https://linkedin.com/in/janedoe")]
    s = _searcher_returning([_serp(
        "Jane Doe - VP Sales - NewCo | LinkedIn", "https://linkedin.com/in/janedoe",
    )])
    events, patch = jc.fetch_job_changes(w, contacts, searcher=s, now=1000.0)
    assert events == []  # never emit on first observation
    st = patch[jc.STATE_KEY][contacts[0]["key"]]
    assert st["last_known_company"] == "NewCo"  # baseline = resolved employer
    assert st["last_checked_at"] == 1000.0


def test_same_company_no_signal():
    key = jc.contact_key({"lead_id": None, "name": "Jane Doe",
                          "linkedin_url": "https://linkedin.com/in/janedoe"})
    w = FakeWatch(cursor={"bootstrapped": True, jc.STATE_KEY: {
        key: {"last_known_company": "Acme Inc", "last_checked_at": 1.0},
    }})
    contacts = [_contact("Jane Doe", linkedin_url="https://linkedin.com/in/janedoe")]
    s = _searcher_returning([_serp(
        "Jane Doe - VP Sales - Acme Inc | LinkedIn", "https://linkedin.com/in/janedoe",
    )])
    events, patch = jc.fetch_job_changes(w, contacts, searcher=s, now=2.0)
    assert events == []
    assert patch[jc.STATE_KEY][key]["last_known_company"] == "Acme Inc"
    assert patch[jc.STATE_KEY][key]["last_checked_at"] == 2.0


def test_changed_company_emits_signal_with_payload():
    contacts = [_contact("Jane Doe", company="Acme Inc",
                         linkedin_url="https://linkedin.com/in/janedoe", lead_id=42)]
    key = contacts[0]["key"]
    w = FakeWatch(cursor={"bootstrapped": True, jc.STATE_KEY: {
        key: {"last_known_company": "Acme Inc", "last_checked_at": 1.0},
    }})
    s = _searcher_returning([_serp(
        "Jane Doe - VP Sales - NewCo | LinkedIn", "https://www.linkedin.com/in/janedoe",
    )])
    events, patch = jc.fetch_job_changes(w, contacts, searcher=s, now=2.0)
    assert len(events) == 1
    ev = events[0]
    assert ev.signal_type == "job_change"
    assert ev.weight == 9
    assert ev.lead_id == 42
    assert ev.company == "NewCo"
    payload = json.loads(ev.description)
    assert payload == {
        "lead_id": 42,
        "person_name": "Jane Doe",
        "old_company": "Acme Inc",
        "new_company": "NewCo",
        "evidence_url": "https://www.linkedin.com/in/janedoe",
    }
    assert ev.source_url == "https://www.linkedin.com/in/janedoe"
    # state advanced → a re-check at the new employer emits nothing
    w.cursor = {"bootstrapped": True, **patch}
    events2, _ = jc.fetch_job_changes(w, contacts, searcher=s, now=3.0)
    assert events2 == []
    # deterministic natural id (dedup key input)
    assert ev.natural_event_id == f"{key}:acme->newco"


def test_low_confidence_parse_keeps_previous_state_no_signal():
    key = jc.contact_key({"lead_id": None, "name": "Jane Doe",
                          "linkedin_url": "https://linkedin.com/in/janedoe"})
    w = FakeWatch(cursor={"bootstrapped": True, jc.STATE_KEY: {
        key: {"last_known_company": "Acme Inc", "last_checked_at": 1.0},
    }})
    contacts = [_contact("Jane Doe", linkedin_url="https://linkedin.com/in/janedoe")]
    s = _searcher_returning([_serp("Jane Doe | LinkedIn", "https://linkedin.com/in/janedoe")])
    events, patch = jc.fetch_job_changes(w, contacts, searcher=s, now=2.0)
    assert events == []
    assert patch[jc.STATE_KEY][key]["last_known_company"] == "Acme Inc"


def test_normalized_equal_companies_do_not_emit():
    # "Acme, Inc." → "Acme LLC" is NOT a job change (legal-suffix noise).
    key = jc.contact_key({"lead_id": None, "name": "Jane Doe",
                          "linkedin_url": "https://linkedin.com/in/janedoe"})
    w = FakeWatch(cursor={"bootstrapped": True, jc.STATE_KEY: {
        key: {"last_known_company": "Acme, Inc.", "last_checked_at": 1.0},
    }})
    contacts = [_contact("Jane Doe", linkedin_url="https://linkedin.com/in/janedoe")]
    s = _searcher_returning([_serp(
        "Jane Doe - VP Sales - Acme LLC | LinkedIn", "https://linkedin.com/in/janedoe",
    )])
    events, _ = jc.fetch_job_changes(w, contacts, searcher=s, now=2.0)
    assert events == []
    # …but a genuinely different company DOES emit.
    s2 = _searcher_returning([_serp(
        "Jane Doe - VP Sales - Globex Pvt Ltd | LinkedIn", "https://linkedin.com/in/janedoe",
    )])
    events2, _ = jc.fetch_job_changes(w, contacts, searcher=s2, now=3.0)
    assert len(events2) == 1
    assert json.loads(events2[0].description)["new_company"] == "Globex Pvt Ltd"


def test_total_search_failure_leaves_cursor_untouched():
    def boom(query, *a, **kw):
        raise RuntimeError("429 ratelimit")

    w = FakeWatch()
    contacts = [_contact("Jane Doe", linkedin_url="https://linkedin.com/in/janedoe")]
    events, patch = jc.fetch_job_changes(w, contacts, searcher=boom, now=2.0)
    assert events is None and patch is None  # fetch failure → no cursor advance


# ── per-cycle cap ─────────────────────────────────────────────────────────────

def test_per_cycle_cap_and_lru_rotation(monkeypatch):
    monkeypatch.setattr(settings, "INTENT_POLLER_JOB_CHANGE_MAX_CONTACTS_PER_POLL", 50,
                        raising=False)
    contacts = [
        _contact(f"Person {i}", linkedin_url=f"https://linkedin.com/in/person{i}")
        for i in range(5)
    ]
    w = FakeWatch(config={"contacts": [], "max_contacts_per_poll": 2})
    s = _searcher_returning([])  # nothing resolves; we only count lookups

    events, patch = jc.fetch_job_changes(w, contacts, searcher=s, now=10.0)
    assert events == []
    assert len(s.calls) == 2  # cap enforced
    checked = {k for k, v in patch[jc.STATE_KEY].items() if v["last_checked_at"] == 10.0}
    assert checked == {contacts[0]["key"], contacts[1]["key"]}

    # next cycle: least-recently-checked first → the OTHER contacts get checked
    w.cursor = {"bootstrapped": True, **patch}
    s2 = _searcher_returning([])
    _, patch2 = jc.fetch_job_changes(w, contacts, searcher=s2, now=20.0)
    checked2 = {k for k, v in patch2[jc.STATE_KEY].items() if v["last_checked_at"] == 20.0}
    assert checked2 == {contacts[2]["key"], contacts[3]["key"]}


def test_cap_is_clamped_by_settings(monkeypatch):
    monkeypatch.setattr(settings, "INTENT_POLLER_JOB_CHANGE_MAX_CONTACTS_PER_POLL", 1,
                        raising=False)
    # per-watch override can only LOWER the global cap, never raise it
    assert jc.per_poll_cap(FakeWatch(config={"max_contacts_per_poll": 10})) == 1
    monkeypatch.setattr(settings, "INTENT_POLLER_JOB_CHANGE_MAX_CONTACTS_PER_POLL", 50,
                        raising=False)
    assert jc.per_poll_cap(FakeWatch(config={})) == 50
    assert jc.per_poll_cap(FakeWatch(config={"max_contacts_per_poll": 3})) == 3


# ── kind / signal-type registration ───────────────────────────────────────────

def test_job_change_kind_registered():
    from apps.api.services.poller import engine as eng
    from apps.api.services.signals.monitor import SIGNAL_TYPES

    assert "job_change" in WATCH_KINDS
    assert eng._source_set(FakeWatch()) == ["job_change"]
    assert SIGNAL_TYPES["job_change"]["weight"] == 9


def test_resolve_lead_id_skips_company_matching_for_job_change():
    from apps.api.services.poller import engine as eng

    # no watch-level lead pin → (None, None): no 'ambiguous_lead' from the
    # free-form roster label, routing is per-event.
    assert eng._resolve_lead_id(None, FakeWatch()) == (None, None)
    assert eng._resolve_lead_id(None, FakeWatch(lead_id=7)) == (7, None)


# ── workspace isolation of emitted signals ────────────────────────────────────

@pytest.fixture()
def signal_session_factory(monkeypatch):
    from apps.api.services.leadgen.orm_models import SignalRow

    engine = create_engine(
        "sqlite:///:memory:", connect_args={"check_same_thread": False},
        poolclass=StaticPool,
    )
    Base.metadata.create_all(engine, tables=[SignalRow.__table__])
    factory = sessionmaker(bind=engine, autoflush=False, autocommit=False)

    import apps.api.services.signals.store as sig_store_mod
    monkeypatch.setattr(sig_store_mod, "SessionLocal", factory)
    monkeypatch.setattr(settings, "AUTOMATIONS_ENABLED", False, raising=False)
    return factory


def _emit_for(ws_id):
    """Run detection for one workspace and emit through the shared signal store."""
    from apps.api.services.poller import engine as eng
    from apps.api.services.signals.store import get_signal_store

    contacts = [_contact("Jane Doe", linkedin_url="https://linkedin.com/in/janedoe")]
    key = contacts[0]["key"]
    w = FakeWatch(workspace_id=ws_id, cursor={"bootstrapped": True, jc.STATE_KEY: {
        key: {"last_known_company": "Acme Inc", "last_checked_at": 1.0},
    }})
    s = _searcher_returning([_serp(
        "Jane Doe - VP Sales - NewCo | LinkedIn", "https://linkedin.com/in/janedoe",
    )])
    events, _ = jc.fetch_job_changes(w, contacts, searcher=s, now=2.0)
    assert len(events) == 1
    store = get_signal_store(ws_id)
    emitted, _ = eng._emit_events(store, w, None, events)
    assert emitted == 1
    return store


def test_workspace_isolation_of_emitted_signals(signal_session_factory):
    store_a = _emit_for(WS_A)
    store_b = _emit_for(WS_B)

    sigs_a = store_a.get_signals(signal_type="job_change")
    sigs_b = store_b.get_signals(signal_type="job_change")
    assert len(sigs_a) == 1 and len(sigs_b) == 1
    assert sigs_a[0]["workspace_id"] == WS_A
    assert sigs_b[0]["workspace_id"] == WS_B
    # deterministic ids are workspace-scoped → no cross-tenant collision
    assert sigs_a[0]["id"] != sigs_b[0]["id"]
    # payload survives the store round-trip
    payload = json.loads(sigs_a[0]["description"])
    assert payload["new_company"] == "NewCo" and payload["old_company"] == "Acme Inc"
    assert sigs_a[0]["company"] == "NewCo"
    # re-emit of the same event is a dedup no-op (deterministic signals.id)
    _emit_for(WS_A)
    assert len(store_a.get_signals(signal_type="job_change")) == 1


# ── lead-contact materialization (workspace-scoped) ───────────────────────────

def test_materialize_contacts_is_workspace_scoped():
    from apps.api.services.leadgen.orm_models import LeadRow

    engine = create_engine(
        "sqlite:///:memory:", connect_args={"check_same_thread": False},
        poolclass=StaticPool,
    )
    Base.metadata.create_all(engine, tables=[LeadRow.__table__])
    Session = sessionmaker(bind=engine)
    with Session() as s:
        s.add(LeadRow(id=1, workspace_id=WS_A, company="Acme Inc",
                      contact_person="Jane Doe",
                      linkedin_url="https://linkedin.com/in/janedoe"))
        s.add(LeadRow(id=2, workspace_id=WS_B, company="OtherCo",
                      contact_person="Bob Roe"))
        s.commit()

        w = FakeWatch(workspace_id=WS_A, config={"contacts": [
            {"lead_id": 1},
            {"lead_id": 2},               # other workspace → dropped
            {"lead_id": 999},             # missing → dropped
            {"name": "Explicit Person", "company": "Globex"},
        ]})
        contacts = jc.materialize_contacts(s, w)

    assert len(contacts) == 2
    lead_contact = contacts[0]
    assert lead_contact["lead_id"] == 1
    assert lead_contact["name"] == "Jane Doe"
    assert lead_contact["company"] == "Acme Inc"
    assert jc.linkedin_handle(lead_contact["linkedin_url"]) == "janedoe"
    assert contacts[1]["name"] == "Explicit Person"
    assert contacts[1]["key"].startswith("nm:")


# ── API validation (weekly minimum, contacts required) ────────────────────────

class _User:
    id = "user-1"


@pytest.fixture()
def api_client(monkeypatch):
    from apps.api.core.tenancy import WorkspaceCtx, current_workspace
    from apps.api.database import get_db
    from apps.api.models import Job
    from apps.api.routers.watches import router as watches_router, require_editor
    from apps.api.services.poller.models import (
        PollBudgetLedger, WatchSchedule, WatchSubscription,
    )

    monkeypatch.setattr(settings, "INTENT_POLLER_ENABLED", True, raising=False)
    monkeypatch.setattr(settings, "PG_LEAD_STORE", True, raising=False)

    engine = create_engine(
        "sqlite:///:memory:", connect_args={"check_same_thread": False},
        poolclass=StaticPool,
    )
    Base.metadata.create_all(engine, tables=[
        WatchSubscription.__table__, WatchSchedule.__table__,
        PollBudgetLedger.__table__, Job.__table__,
    ])
    Session = sessionmaker(bind=engine)
    import apps.api.database as database
    import apps.api.services.poller.engine as eng_mod
    monkeypatch.setattr(database, "SessionLocal", Session, raising=False)
    monkeypatch.setattr(eng_mod, "SessionLocal", Session, raising=False)

    app = FastAPI()
    app.include_router(watches_router)

    def _override_db():
        s = Session()
        try:
            yield s
        finally:
            s.close()

    app.dependency_overrides[get_db] = _override_db
    app.dependency_overrides[current_workspace] = lambda: WorkspaceCtx(
        user=_User(), workspace_id=WS_A, slug="alpha")
    app.dependency_overrides[require_editor] = app.dependency_overrides[current_workspace]
    return TestClient(app)


def test_api_create_job_change_watch_defaults_weekly(api_client):
    r = api_client.post("/api/watches", json={
        "kind": "job_change", "target": "key accounts",
        "config": {"contacts": [
            {"name": "Jane Doe", "company": "Acme",
             "linkedin_url": "https://linkedin.com/in/janedoe"},
            {"lead_id": 42},
        ]},
    })
    assert r.status_code == 200, r.text
    body = r.json()
    assert body["interval"] == "weekly"           # default weekly
    assert body["signal_types"] == ["job_change"]
    assert len(body["config"]["contacts"]) == 2

    # PATCH: interval below weekly rejected; config re-validated
    wid = body["id"]
    assert api_client.patch(f"/api/watches/{wid}",
                            json={"interval": "daily"}).status_code == 422
    assert api_client.patch(f"/api/watches/{wid}",
                            json={"config": {"contacts": []}}).status_code == 422
    r2 = api_client.patch(f"/api/watches/{wid}", json={
        "config": {"contacts": [{"name": "Bob Roe"}], "max_contacts_per_poll": 5},
    })
    assert r2.status_code == 200
    assert r2.json()["config"]["max_contacts_per_poll"] == 5


def test_api_job_change_validation(api_client):
    # weekly minimum enforced at create
    for bad_interval in ("hourly", "daily"):
        r = api_client.post("/api/watches", json={
            "kind": "job_change", "target": "x", "interval": bad_interval,
            "config": {"contacts": [{"name": "Jane Doe"}]},
        })
        assert r.status_code == 422, bad_interval
    # contacts required
    assert api_client.post("/api/watches", json={
        "kind": "job_change", "target": "x",
    }).status_code == 422
    assert api_client.post("/api/watches", json={
        "kind": "job_change", "target": "x", "config": {"contacts": []},
    }).status_code == 422
    # entry needs lead_id or name
    assert api_client.post("/api/watches", json={
        "kind": "job_change", "target": "x",
        "config": {"contacts": [{"company": "Acme"}]},
    }).status_code == 422
    # bad linkedin url
    assert api_client.post("/api/watches", json={
        "kind": "job_change", "target": "x",
        "config": {"contacts": [{"name": "J", "linkedin_url": "https://x.com/j"}]},
    }).status_code == 422
    # signal_type mismatch for the kind
    assert api_client.post("/api/watches", json={
        "kind": "job_change", "target": "x", "signal_types": ["company_funded"],
        "config": {"contacts": [{"name": "Jane Doe"}]},
    }).status_code == 422
    # other kinds reject config
    r = api_client.post("/api/watches", json={"kind": "feed", "target": "https://a.com/rss"})
    assert r.status_code == 200
    assert api_client.patch(f"/api/watches/{r.json()['id']}",
                            json={"config": {"contacts": [{"name": "J"}]}}).status_code == 422
