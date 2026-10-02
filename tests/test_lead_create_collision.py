"""Creating a company lead must never silently replace its existing contact."""

from concurrent.futures import ThreadPoolExecutor
import threading
from types import SimpleNamespace

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker

from apps.api.routers import leads
from apps.api.services.leadgen.db import LeadDB
from apps.api.services.leadgen.orm_models import LeadRow
from apps.api.services.leadgen import store


@pytest.fixture(params=["sqlite", "orm"])
def client(request, tmp_path, monkeypatch):
    if request.param == "sqlite":
        path = tmp_path / "leads.db"
        factory = lambda: LeadDB(str(path))
    else:
        engine = create_engine(f"sqlite:///{tmp_path / 'orm.db'}", connect_args={"check_same_thread": False, "timeout": 30})
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


def test_create_collision_preserves_contact_and_omitted_enrichment(client):
    first = client.post("/api/lead", json={
        "company": "Fixture", "city": "Repro City", "contact_person": "Person One",
        "email": "one@example.invalid", "website": "https://example.invalid",
    })
    assert first.status_code == 200, first.text
    lead_id = first.json()["id"]
    assert client.put(f"/api/lead/{lead_id}", json={"phone": "+15555550100", "specialization": "B2B SaaS"}).status_code == 200
    before = client.get(f"/api/lead/{lead_id}").json()
    collision = client.post("/api/lead", json={
        "company": "Fixture", "city": "Repro City", "contact_person": "Person Two",
        "email": "two@example.invalid",
    })
    assert collision.status_code == 409, collision.text
    assert collision.json()["detail"]["lead_id"] == lead_id
    assert client.get(f"/api/lead/{lead_id}").json() == before
    # An explicit edit still supports replacement and deliberate clearing.
    changed = client.put(f"/api/lead/{lead_id}", json={"contact_person": "Person Two", "email": "two@example.invalid", "phone": ""})
    assert changed.status_code == 200, changed.text
    updated = client.get(f"/api/lead/{lead_id}").json()
    assert updated["contact_person"] == "Person Two"
    assert updated["email"] == "two@example.invalid" and updated["phone"] == ""
    assert updated["website"] == "https://example.invalid"
    assert updated["specialization"] == "B2B SaaS"


def test_concurrent_create_has_one_winner_without_contact_replacement(client):
    # Initialize the schema before the simultaneous HTTP requests.
    assert client.post("/api/lead", json={"company": "Other Fixture"}).status_code == 200
    barrier = threading.Barrier(6)

    def create(index):
        barrier.wait(timeout=10)
        response = client.post("/api/lead", json={
            "company": "Concurrent Fixture", "city": "Repro City",
            "contact_person": f"Person {index}", "email": f"person{index}@example.invalid",
        })
        return index, response

    with ThreadPoolExecutor(max_workers=6) as pool:
        responses = list(pool.map(create, range(6)))
    winners = [(index, response) for index, response in responses if response.status_code == 200]
    assert len(winners) == 1, [(index, response.status_code, response.text) for index, response in responses]
    winner, response = winners[0]
    lead_id = response.json()["id"]
    for index, result in responses:
        if index != winner:
            assert result.status_code == 409, result.text
            assert result.json()["detail"]["lead_id"] == lead_id
    saved = client.get(f"/api/lead/{lead_id}").json()
    assert saved["contact_person"] == f"Person {winner}"
    assert saved["email"] == f"person{winner}@example.invalid"
