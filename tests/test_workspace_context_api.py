"""GET /api/auth/workspace-context — the Go server's forward-auth endpoint.

Runs the real ``current_workspace`` dependency (token decoding, membership,
SSO enforcement) so the Go server inherits exactly the checks every FastAPI
data endpoint applies.
"""

from pathlib import Path

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker

from apps.api.auth import create_access_token, create_refresh_token
from apps.api.database import Base, get_db
from apps.api.models import User
from apps.api.routers.auth import api_router
from apps.api.services.workspace import manager, oidc


@pytest.fixture()
def env(monkeypatch, tmp_path):
    monkeypatch.setattr(manager, "_project_root", lambda: Path(tmp_path))
    engine = create_engine(f"sqlite:///{tmp_path}/users.db")
    Base.metadata.create_all(engine, tables=[User.__table__])
    Session = sessionmaker(bind=engine)
    with Session() as db:
        db.add_all([
            User(id=1, username="owner", hashed_password="x", is_active=True),
            User(id=2, username="analyst", hashed_password="x", is_active=True),
            User(id=3, username="outsider", hashed_password="x", is_active=True),
        ])
        db.commit()
    primary = manager.create_workspace("Context Primary", owner_id=1)
    other = manager.create_workspace("Context Other", owner_id=1)
    manager.add_member(primary.id, 2, "viewer")
    manager.add_member(other.id, 2, "editor")
    manager.set_user_active_workspace(2, other.id)

    def _db():
        db = Session()
        try:
            yield db
        finally:
            db.close()

    app = FastAPI()
    app.include_router(api_router)
    app.dependency_overrides[get_db] = _db
    yield TestClient(app), primary, other
    engine.dispose()


def _auth(username, **extra):
    return {"Authorization": f"Bearer {create_access_token({'sub': username, 'amr': ['pwd']})}", **extra}


def test_reports_requested_workspace_and_role(env):
    client, primary, _ = env
    r = client.get("/api/auth/workspace-context", headers=_auth("analyst", **{"X-Workspace-Id": primary.id}))
    assert r.status_code == 200
    assert r.json() == {
        "user_id": "2",
        "username": "analyst",
        "workspace_id": primary.id,
        "slug": primary.slug,
        "role": "viewer",
    }


def test_defaults_to_active_workspace(env):
    client, _, other = env
    r = client.get("/api/auth/workspace-context", headers=_auth("analyst"))
    assert r.status_code == 200
    assert r.json()["workspace_id"] == other.id
    assert r.json()["role"] == "editor"


def test_owner_role_reported(env):
    client, primary, _ = env
    r = client.get("/api/auth/workspace-context", headers=_auth("owner", **{"X-Workspace-Id": primary.id}))
    assert r.status_code == 200 and r.json()["role"] == "owner"


def test_non_member_is_forbidden(env):
    client, primary, _ = env
    r = client.get("/api/auth/workspace-context", headers=_auth("outsider", **{"X-Workspace-Id": primary.id}))
    assert r.status_code == 403


@pytest.mark.parametrize("headers", [
    {},
    {"Authorization": "Bearer not-a-jwt"},
    {"Authorization": f"Bearer {create_refresh_token({'sub': 'analyst'})}"},
])
def test_missing_invalid_or_refresh_token_is_unauthorized(env, headers):
    client, primary, _ = env
    r = client.get("/api/auth/workspace-context", headers={**headers, "X-Workspace-Id": primary.id})
    assert r.status_code == 401


def test_sso_enforcement_applies(env, monkeypatch):
    client, primary, _ = env
    monkeypatch.setattr(oidc, "get_config", lambda ws: {"enforce_sso": True})
    r = client.get("/api/auth/workspace-context", headers=_auth("analyst", **{"X-Workspace-Id": primary.id}))
    assert r.status_code == 403
    sso = {"Authorization": f"Bearer {create_access_token({'sub': 'analyst', 'amr': ['sso']})}",
           "X-Workspace-Id": primary.id}
    assert client.get("/api/auth/workspace-context", headers=sso).status_code == 200
