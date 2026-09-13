"""Persistent audience CRUD, validation, counts, and tenant isolation."""

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.core.tenancy import WorkspaceCtx, current_workspace
from apps.api.database import Base, get_db
from apps.api.routers.audiences import router, require_editor
from apps.api.services.audiences.models import Audience

WS1 = "ws-audiences-1"
WS2 = "ws-audiences-2"


class _User:
    id = "user-1"


class _LeadStore:
    closed = False

    def query_leads_page(self, filters, page, page_size):
        count = 7 if filters.get("score_tier") == "hot" else 2
        return [], count

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
    Base.metadata.create_all(engine, tables=[Audience.__table__])
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
    tc, _, _ = client
    created = tc.post("/api/audiences", json={
        "name": "Hot accounts", "filters": {"score_tier": "hot", "has_email": True},
    })
    assert created.status_code == 201, created.text
    audience = created.json()
    assert audience["member_count"] == 7

    listed = tc.get("/api/audiences")
    assert listed.status_code == 200
    assert [item["name"] for item in listed.json()] == ["Hot accounts"]

    updated = tc.patch(f"/api/audiences/{audience['id']}", json={
        "name": "Warm accounts", "filters": {"score_tier": "warm"},
    })
    assert updated.status_code == 200
    assert updated.json()["member_count"] == 2

    refreshed = tc.post(f"/api/audiences/{audience['id']}/refresh")
    assert refreshed.status_code == 200 and refreshed.json()["member_count"] == 2
    assert tc.delete(f"/api/audiences/{audience['id']}").status_code == 204
    assert tc.get("/api/audiences").json() == []


def test_audience_validation_and_duplicate_name(client):
    tc, _, _ = client
    assert tc.post("/api/audiences", json={"name": "Bad", "filters": {"sql": "no"}}).status_code == 422
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
