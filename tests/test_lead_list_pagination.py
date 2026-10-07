"""The lead list paginates visible records in both native store implementations."""

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
    engine = None
    if request.param == "sqlite":
        path = tmp_path / "leads.db"
        factory = lambda: LeadDB(str(path))
    else:
        engine = create_engine(
            f"sqlite:///{tmp_path / 'orm.db'}", connect_args={"check_same_thread": False},
        )
        LeadRow.__table__.create(engine)
        monkeypatch.setattr(store, "SessionLocal", sessionmaker(bind=engine))
        factory = lambda: store.PgLeadStore("ws-pagination")
    ctx = SimpleNamespace(workspace_id="ws-pagination", lead_db=factory)
    app = FastAPI()
    app.include_router(leads.router)
    app.dependency_overrides[leads.require_editor] = lambda: ctx
    app.dependency_overrides[leads.current_workspace] = lambda: ctx
    with TestClient(app) as http:
        for company, status, score in [
            ("Dead highest", "dead", 100),
            ("Active first", "new", 90),
            ("Dead middle", "dead", 80),
            ("Active second", "new", 70),
            ("Active third", "new", 60),
        ]:
            created = http.post("/api/lead", json={"company": company})
            assert created.status_code == 200, created.text
            edited = http.put(f"/api/lead/{created.json()['id']}", json={"status": status, "score": score})
            assert edited.status_code == 200, edited.text
        yield http
    if engine is not None:
        engine.dispose()


@pytest.mark.parametrize("status", [None, "__all__"])
def test_default_pages_are_full_and_offsets_count_visible_rows(client, status):
    params = {"limit": 2, "offset": 0}
    if status is not None:
        params["status"] = status
    first = client.get("/api/leads", params=params)
    assert first.status_code == 200, first.text
    assert [row["company"] for row in first.json()] == ["Active first", "Active second"]
    params["offset"] = 2
    later = client.get("/api/leads", params=params)
    assert later.status_code == 200, later.text
    assert [row["company"] for row in later.json()] == ["Active third"]


def test_explicit_dead_filter_remains_available(client):
    response = client.get("/api/leads", params={"status": "dead", "limit": 2})
    assert response.status_code == 200, response.text
    assert [row["company"] for row in response.json()] == ["Dead highest", "Dead middle"]
