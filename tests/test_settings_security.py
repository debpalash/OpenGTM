from fastapi import FastAPI
from fastapi.testclient import TestClient

from apps.api.core.security import get_current_active_user, get_current_admin_user
from apps.api.routers import settings as settings_router


class User:
    id = 1
    is_active = True
    is_admin = False
    role = "user"


def _app():
    app = FastAPI()
    app.include_router(settings_router.router)
    return app


def test_settings_require_authentication():
    response = TestClient(_app()).get("/api/settings/providers")
    assert response.status_code == 401


def test_global_settings_mutations_require_admin():
    app = _app()
    app.dependency_overrides[get_current_active_user] = lambda: User()
    response = TestClient(app).put(
        "/api/settings/providers/not-real", json={"model": "x"}
    )
    assert response.status_code == 403


def test_admin_reaches_provider_validation():
    app = _app()
    app.dependency_overrides[get_current_active_user] = lambda: User()
    app.dependency_overrides[get_current_admin_user] = lambda: User()
    response = TestClient(app).put(
        "/api/settings/providers/not-real", json={"model": "x"}
    )
    assert response.status_code == 404


def test_viewer_cannot_write_workspace_credentials(monkeypatch):
    from apps.api.services.workspace import manager as ws_manager

    app = _app()
    app.dependency_overrides[get_current_active_user] = lambda: User()
    monkeypatch.setattr(ws_manager, "get_user_active_workspace", lambda user_id: "W1")
    monkeypatch.setattr(ws_manager, "is_member", lambda workspace_id, user_id: True)
    monkeypatch.setattr(ws_manager, "member_role", lambda workspace_id, user_id: "viewer")

    response = TestClient(app).put(
        "/api/settings/workspace-integrations/hubspot",
        json={"values": {"HUBSPOT_TOKEN": "must-not-write"}},
    )
    assert response.status_code == 403
