"""Merging duplicate leads must keep the master lead."""

from types import SimpleNamespace

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker

from apps.api.routers import leads
from apps.api.services.leadgen import store
from apps.api.services.leadgen.db import LeadDB
from apps.api.services.leadgen.orm_models import LeadRow


@pytest.fixture(params=["sqlite", "orm"])
def client(request, tmp_path, monkeypatch):
    if request.param == "sqlite":
        path = tmp_path / "leads.db"
        factory = lambda: LeadDB(str(path))
    else:
        engine = create_engine(
            f"sqlite:///{tmp_path / 'orm.db'}",
            connect_args={"check_same_thread": False, "timeout": 30},
        )
        LeadRow.__table__.create(engine)
        monkeypatch.setattr(store, "SessionLocal", sessionmaker(bind=engine))
        factory = lambda: store.PgLeadStore("ws-fixture")
    ctx = SimpleNamespace(workspace_id="ws-fixture", lead_db=factory)
    app = FastAPI()
    app.include_router(leads.router)
    app.dependency_overrides[leads.require_editor] = lambda: ctx
    app.dependency_overrides[leads.current_workspace] = lambda: ctx
    with TestClient(app) as http:
        yield http
    if request.param == "orm":
        engine.dispose()


def _create(client, company, **extra):
    response = client.post("/api/lead", json={"company": company, "city": "Pune", **extra})
    assert response.status_code == 200, response.text
    return response.json()["id"]


def _exists(client, lead_id):
    return client.get(f"/api/lead/{lead_id}").status_code == 200


def test_merge_with_master_listed_as_duplicate_keeps_master(client):
    master = _create(client, "Acme Corp", email="sales@acme.example")
    duplicate = _create(client, "Acme Corporation", phone="+15555550100")

    response = client.post(
        "/api/leads/dedup/merge",
        json={"master_id": master, "duplicate_ids": [duplicate, master]},
    )

    assert response.status_code == 200, response.text
    assert response.json()["duplicates_deleted"] == 1
    assert _exists(client, master), "the master lead was deleted by its own merge"
    assert not _exists(client, duplicate)
    kept = client.get(f"/api/lead/{master}").json()
    assert kept["email"] == "sales@acme.example"
    assert kept["phone"] == "+15555550100"


def test_merge_of_only_the_master_is_rejected_and_deletes_nothing(client):
    master = _create(client, "Solo Corp")

    response = client.post(
        "/api/leads/dedup/merge",
        json={"master_id": master, "duplicate_ids": [master]},
    )

    assert response.status_code == 400, response.text
    assert _exists(client, master)
