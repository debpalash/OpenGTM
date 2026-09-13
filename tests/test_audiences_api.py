"""Persistent audience CRUD, validation, counts, and tenant isolation."""

import pytest
from types import SimpleNamespace
from fastapi import FastAPI
from fastapi.testclient import TestClient
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.core.tenancy import WorkspaceCtx, current_workspace
from apps.api.database import Base, get_db
from apps.api.routers.audiences import router, require_editor
from apps.api.services.audiences.models import Audience, AudienceMember, AudienceMembershipEvent, AudienceSchedule
from apps.api.services.automations.models import Trigger
from apps.api.services.workbook.models import Workbook, WorkbookRow
from apps.api.core.config import settings
from apps.api.models import Job

WS1 = "ws-audiences-1"
WS2 = "ws-audiences-2"


class _User:
    id = "user-1"


class _LeadStore:
    closed = False

    def query_leads_page(self, filters, page, page_size):
        count = 7 if filters.get("score_tier") == "hot" else 2
        all_rows = [{"id": lead_id, "company": f"Account {lead_id}"} for lead_id in range(1, count + 1)]
        start = (page - 1) * page_size
        return all_rows[start:start + page_size], count

    def close(self):
        self.closed = True


def _ctx(workspace_id=WS1):
    ctx = WorkspaceCtx(user=_User(), workspace_id=workspace_id, slug=workspace_id)
    ctx.lead_db = lambda: _LeadStore()
    return ctx


@pytest.fixture()
def client():
    engine = create_engine(
        "sqlite:///:memory:", connect_args={"check_same_thread": False}, poolclass=StaticPool,
    )
    Base.metadata.create_all(engine, tables=[
        Audience.__table__, AudienceMember.__table__, AudienceMembershipEvent.__table__,
        AudienceSchedule.__table__, Job.__table__,
    ])
    Session = sessionmaker(bind=engine)
    app = FastAPI()
    app.include_router(router)

    def override_db():
        session = Session()
        try:
            yield session
        finally:
            session.close()

    app.dependency_overrides[get_db] = override_db
    app.dependency_overrides[current_workspace] = lambda: _ctx()
    app.dependency_overrides[require_editor] = lambda: _ctx()
    return TestClient(app), Session, app


def test_audience_crud_and_dynamic_count(client):
    tc, Session, _ = client
    created = tc.post("/api/audiences", json={
        "name": "Hot accounts", "filters": {"score_tier": "hot", "has_email": True},
    })
    assert created.status_code == 201, created.text
    audience = created.json()
    assert audience["member_count"] == 7
    assert audience["refreshed_at"] is not None
    assert audience["refresh_enabled"] is True
    assert audience["next_refresh_at"] is not None
    session = Session()
    assert session.query(Job).filter(Job.type == "audience_refresh", Job.status == "pending").count() == 1
    session.close()
    assert len(tc.get(f"/api/audiences/{audience['id']}/members").json()) == 7
    assert len(tc.get(f"/api/audiences/{audience['id']}/events").json()) == 7

    listed = tc.get("/api/audiences")
    assert listed.status_code == 200
    assert [item["name"] for item in listed.json()] == ["Hot accounts"]

    updated = tc.patch(f"/api/audiences/{audience['id']}", json={
        "name": "Warm accounts", "filters": {"score_tier": "warm"},
    })
    assert updated.status_code == 200
    assert updated.json()["member_count"] == 2

    paused = tc.patch(f"/api/audiences/{audience['id']}", json={"refresh_enabled": False})
    assert paused.status_code == 200 and paused.json()["next_refresh_at"] is None
    session = Session()
    assert session.query(Job).filter(Job.type == "audience_refresh", Job.status == "pending").count() == 0
    session.close()

    refreshed = tc.post(f"/api/audiences/{audience['id']}/refresh")
    assert refreshed.status_code == 200
    assert refreshed.json()["audience"]["member_count"] == 2
    assert refreshed.json()["entered"] == 0
    assert refreshed.json()["exited"] == 0
    events = tc.get(f"/api/audiences/{audience['id']}/events").json()
    assert len(events) == 12
    assert sum(event["event_type"] == "exited" for event in events) == 5
    assert tc.delete(f"/api/audiences/{audience['id']}").status_code == 204
    assert tc.get("/api/audiences").json() == []


def test_audience_validation_and_duplicate_name(client):
    tc, _, _ = client
    assert tc.post("/api/audiences", json={"name": "Bad", "filters": {"sql": "no"}}).status_code == 422
    assert tc.post("/api/audiences", json={"name": "Too fast", "filters": {}, "refresh_interval_minutes": 5}).status_code == 422
    assert tc.post("/api/audiences", json={"name": "Bad", "filters": {"has_email": "yes"}}).status_code == 422
    assert tc.post("/api/audiences", json={"name": "Scores", "filters": {"min_score": 80, "max_score": 20}}).status_code == 422
    assert tc.post("/api/audiences", json={"name": "Unique", "filters": {}}).status_code == 201
    assert tc.post("/api/audiences", json={"name": "Unique", "filters": {}}).status_code == 409


def test_audience_workspace_isolation(client):
    tc, Session, app = client
    session = Session()
    hidden = Audience(workspace_id=WS2, name="Other tenant", filters={}, member_count=99)
    session.add(hidden)
    session.commit()
    hidden_id = hidden.id
    session.close()

    assert tc.get("/api/audiences").json() == []
    assert tc.patch(f"/api/audiences/{hidden_id}", json={"name": "stolen"}).status_code == 404
    assert tc.delete(f"/api/audiences/{hidden_id}").status_code == 404

    app.dependency_overrides[current_workspace] = lambda: _ctx(WS2)
    app.dependency_overrides[require_editor] = lambda: _ctx(WS2)
    assert [item["name"] for item in tc.get("/api/audiences").json()] == ["Other tenant"]


def test_membership_entry_enqueues_scoped_automation(monkeypatch):
    from apps.api.services.automations import events as automation_events

    engine = create_engine(
        "sqlite:///:memory:", connect_args={"check_same_thread": False}, poolclass=StaticPool,
    )
    Base.metadata.create_all(engine, tables=[Trigger.__table__, Workbook.__table__, WorkbookRow.__table__])
    Session = sessionmaker(bind=engine)
    session = Session()
    workbook = Workbook(name="Activation", workspace_id=WS1)
    session.add(workbook)
    session.flush()
    row = WorkbookRow(workbook_id=workbook.id, workspace_id=WS1, position=0, data={}, lead_id=42)
    rule = Trigger(
        workspace_id=WS1, name="Activate entrants", trigger_type="on_audience_enter",
        trigger_config={"audience_ids": ["aud-1"]}, actions=[], scope_workbook_ids=[workbook.id],
    )
    session.add_all([row, rule])
    session.commit()

    enqueued = []
    monkeypatch.setattr(settings, "AUTOMATIONS_ENABLED", True, raising=False)
    monkeypatch.setattr(automation_events, "_enqueue_eval", lambda db, **kwargs: enqueued.append(kwargs))
    count = automation_events.emit_audience_membership(session, WS1, [SimpleNamespace(
        id=9, audience_id="aud-1", lead_id=42, event_type="entered",
    )])

    assert count == 1
    assert enqueued[0]["fire_key"] == "audience:9:entered"
    assert enqueued[0]["targets"] == [{"workbook_id": workbook.id, "row_id": str(row.id)}]
    session.close()
