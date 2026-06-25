"""Intent-poller API router tests (SQLite, offline). Maps to AC-13/AC-15/AC-20.

Auth/workspace deps are overridden so we exercise validation + CRUD + poll-now
logic without a real auth stack. Covers: CRUD + scoping, kind/interval/signal_type
validation, flag-OFF 404, poll-now reuses the scheduled fire_key (already-queued),
and poll-now rate-limit / quota.
"""

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.core.config import settings
from apps.api.core.tenancy import WorkspaceCtx, current_workspace
from apps.api.database import Base, get_db
from apps.api.models import Job
from apps.api.services.poller.models import (
    PollBudgetLedger, WatchSchedule, WatchSubscription,
)
from apps.api.routers.watches import router as watches_router, require_editor

WS = "ws_api_poll"


class _User:
    id = "user-1"


@pytest.fixture()
def client(monkeypatch):
    monkeypatch.setattr(settings, "INTENT_POLLER_ENABLED", True, raising=False)
    monkeypatch.setattr(settings, "PG_LEAD_STORE", True, raising=False)

    engine = create_engine(
        "sqlite:///:memory:", connect_args={"check_same_thread": False}, poolclass=StaticPool,
    )
    Base.metadata.create_all(engine, tables=[
        WatchSubscription.__table__, WatchSchedule.__table__, PollBudgetLedger.__table__,
        Job.__table__,
    ])
    Session = sessionmaker(bind=engine)
    # Point the engine module's SessionLocal at the in-memory DB (poll_now_quota
    # reads via the request session; enqueue uses the request session too).
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

    def _override_ws():
        return WorkspaceCtx(user=_User(), workspace_id=WS, slug="api")

    app.dependency_overrides[get_db] = _override_db
    app.dependency_overrides[current_workspace] = _override_ws
    app.dependency_overrides[require_editor] = _override_ws
    return TestClient(app), Session


def test_create_list_get_patch_delete_AC15(client):
    tc, _ = client
    r = tc.post("/api/watches", json={"kind": "feed", "target": "https://acme.com/rss"})
    assert r.status_code == 200, r.text
    wid = r.json()["id"]
    assert r.json()["signal_types"] == ["news"]
    assert r.json()["enabled"] is True

    # list
    r = tc.get("/api/watches")
    assert r.status_code == 200
    assert any(w["id"] == wid for w in r.json()["watches"])

    # get
    assert tc.get(f"/api/watches/{wid}").json()["id"] == wid

    # patch interval
    r = tc.patch(f"/api/watches/{wid}", json={"interval": "hourly"})
    assert r.status_code == 200 and r.json()["interval"] == "hourly"

    # delete
    assert tc.delete(f"/api/watches/{wid}").json()["deleted"] is True
    assert tc.get(f"/api/watches/{wid}").status_code == 404


def test_validation_AC15(client):
    tc, _ = client
    assert tc.post("/api/watches", json={"kind": "bogus", "target": "x"}).status_code == 422
    assert tc.post("/api/watches", json={"kind": "feed", "target": "x", "interval": "yearly"}).status_code == 422
    # signal_type not valid for kind
    r = tc.post("/api/watches", json={"kind": "feed", "target": "x", "signal_types": ["company_funded"]})
    assert r.status_code == 422


def test_flag_off_404_AC13(monkeypatch):
    monkeypatch.setattr(settings, "INTENT_POLLER_ENABLED", False, raising=False)
    engine = create_engine("sqlite:///:memory:", connect_args={"check_same_thread": False}, poolclass=StaticPool)
    Base.metadata.create_all(engine, tables=[
        WatchSubscription.__table__, WatchSchedule.__table__, PollBudgetLedger.__table__, Job.__table__,
    ])
    Session = sessionmaker(bind=engine)
    app = FastAPI()
    app.include_router(watches_router)
    app.dependency_overrides[get_db] = lambda: (lambda s=Session(): (yield s))()
    app.dependency_overrides[current_workspace] = lambda: WorkspaceCtx(user=_User(), workspace_id=WS, slug="api")
    app.dependency_overrides[require_editor] = app.dependency_overrides[current_workspace]
    tc = TestClient(app)
    assert tc.get("/api/watches").status_code == 404
    assert tc.post("/api/watches", json={"kind": "feed", "target": "x"}).status_code == 404


def test_poll_now_reuses_scheduled_fire_key_AC20(client):
    tc, Session = client
    wid = tc.post("/api/watches", json={"kind": "feed", "target": "https://acme.com/rss"}).json()["id"]
    # create already enqueued a scheduled poll → poll-now returns already_queued.
    r = tc.post(f"/api/watches/{wid}/poll")
    assert r.status_code == 200, r.text
    assert r.json()["status"] == "already_queued"
    # no second job for the scheduled fire_key
    s = Session()
    n = s.query(Job).filter(Job.type == "watch_poll", Job.status == "pending").count()
    s.close()
    assert n == 1


def test_poll_now_rate_limited_AC20(client, monkeypatch):
    tc, Session = client
    from datetime import datetime, timezone
    wid = tc.post("/api/watches", json={"kind": "feed", "target": "https://acme.com/rss"}).json()["id"]
    # mark last_polled_at now + clear the scheduled job so we reach the rate-limit
    s = Session()
    w = s.query(WatchSubscription).filter(WatchSubscription.id == wid).first()
    w.last_polled_at = datetime.now(timezone.utc)
    w.next_poll_at = None
    s.query(Job).delete()
    s.commit()
    s.close()
    r = tc.post(f"/api/watches/{wid}/poll")
    assert r.status_code == 429
