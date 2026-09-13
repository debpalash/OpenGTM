from fastapi import FastAPI, Request
from fastapi.testclient import TestClient
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.database import Base
from apps.api.services.governance.audit import GovernanceAuditMiddleware
from apps.api.services.governance.models import GovernanceAuditEvent


def test_workspace_mutations_are_audited_without_request_bodies(monkeypatch):
    engine = create_engine("sqlite:///:memory:", connect_args={"check_same_thread": False}, poolclass=StaticPool)
    Base.metadata.create_all(engine, tables=[GovernanceAuditEvent.__table__])
    Session = sessionmaker(bind=engine)
    monkeypatch.setattr("apps.api.database.SessionLocal", Session)
    app = FastAPI()
    app.add_middleware(GovernanceAuditMiddleware)

    @app.post("/api/things/{thing_id}")
    def mutate(thing_id: str, request: Request):
        request.state.workspace_id = "ws-audit"
        request.state.actor_user_id = 42
        request.state.actor_role = "admin"
        return {"id": thing_id}

    client = TestClient(app)
    response = client.post("/api/things/abc", json={"email": "never-store@example.test", "secret": "never-store"}, headers={"X-Request-Id": "req-123"})
    assert response.status_code == 200 and response.headers["X-Request-Id"] == "req-123"
    with Session() as db:
        event = db.query(GovernanceAuditEvent).one()
        assert event.workspace_id == "ws-audit" and event.actor_user_id == 42
        assert event.route == "/api/things/{thing_id}" and event.resource_path == "/api/things/abc"
        assert event.outcome == "success" and event.request_id == "req-123"
        assert "never-store" not in str(event.to_api())


def test_reads_and_unauthenticated_writes_are_not_workspace_audited(monkeypatch):
    engine = create_engine("sqlite:///:memory:", connect_args={"check_same_thread": False}, poolclass=StaticPool)
    Base.metadata.create_all(engine, tables=[GovernanceAuditEvent.__table__])
    Session = sessionmaker(bind=engine)
    monkeypatch.setattr("apps.api.database.SessionLocal", Session)
    app = FastAPI(); app.add_middleware(GovernanceAuditMiddleware)
    @app.get("/read")
    def read(request: Request):
        request.state.workspace_id = "ws-audit"
        return {}
    @app.post("/login")
    def login(): return {}
    client = TestClient(app); client.get("/read"); client.post("/login")
    with Session() as db:
        assert db.query(GovernanceAuditEvent).count() == 0
