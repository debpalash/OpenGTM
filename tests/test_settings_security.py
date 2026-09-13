from fastapi import FastAPI
from fastapi.testclient import TestClient

from apps.api.core.security import get_current_active_user, get_current_admin_user
from apps.api.core.tenancy import WorkspaceCtx, current_workspace
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


def test_llm_usage_reads_only_the_resolved_workspace_store():
    class UsageStore:
        closed = False

        def get_llm_usage(self, date=None):
            assert date == "2026-09-13"
            return [{
                "provider": "tenant-provider", "model": "tenant-model",
                "calls": 2, "total_tokens": 30,
            }]

        def get_llm_usage_total(self):
            return {"total_calls": 5, "total_tokens": 90}

        def close(self):
            self.closed = True

    store = UsageStore()
    ctx = WorkspaceCtx(user=User(), workspace_id="tenant-a", slug="tenant-a")
    ctx.lead_db = lambda: store
    app = _app()
    app.dependency_overrides[get_current_active_user] = lambda: User()
    app.dependency_overrides[current_workspace] = lambda: ctx

    response = TestClient(app).get("/api/settings/llm-usage?date=2026-09-13")

    assert response.status_code == 200, response.text
    assert response.json()["providers"][0]["provider"] == "tenant-provider"
    assert response.json()["today_calls"] == 2
    assert response.json()["all_time_calls"] == 5
    assert store.closed is True
