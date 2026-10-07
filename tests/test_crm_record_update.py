"""CRM record updates through the public route and a real SQLite store."""

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.core.security import get_current_active_user, get_current_admin_user
from apps.api.database import Base, get_db
from apps.api.models import EmailData, User
from apps.api.routers import crm


@pytest.fixture
def crm_client():
    engine = create_engine(
        "sqlite://", connect_args={"check_same_thread": False}, poolclass=StaticPool,
    )
    Base.metadata.create_all(engine, tables=[EmailData.__table__])
    sessions = sessionmaker(bind=engine)
    with sessions() as db:
        db.add(EmailData(id=1, name="Acme contact", email="contact@acme.test"))
        db.commit()
    app = FastAPI()
    app.include_router(crm.router)
    admin = User(id=1, username="operator", is_active=True, is_admin=True)

    def database():
        with sessions() as db:
            yield db

    app.dependency_overrides[get_db] = database
    app.dependency_overrides[get_current_active_user] = lambda: admin
    app.dependency_overrides[get_current_admin_user] = lambda: admin
    with TestClient(app, raise_server_exceptions=False) as client:
        yield client, sessions
    engine.dispose()


def test_missing_update_returns_not_found_and_preserves_existing_record(crm_client):
    client, sessions = crm_client
    response = client.patch("/api/data/999", json={"notes": "follow up"})
    assert response.status_code == 404, response.text
    assert response.json() == {"detail": "Record with id 999 not found"}
    with sessions() as db:
        record = db.get(EmailData, 1)
        assert record.name == "Acme contact"
        assert record.notes is None


def test_existing_update_still_persists_the_requested_fields(crm_client):
    client, sessions = crm_client
    response = client.patch("/api/data/1", json={"notes": "follow up", "is_used": True})
    assert response.status_code == 200, response.text
    assert response.json()["status"] == "success"
    assert response.json()["data"]["notes"] == "follow up"
    assert response.json()["data"]["is_used"] is True
    assert response.json()["data"]["name"] == "Acme contact"
    with sessions() as db:
        record = db.get(EmailData, 1)
        assert record.notes == "follow up"
        assert record.is_used is True
        assert record.name == "Acme contact"
