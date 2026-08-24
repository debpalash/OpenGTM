from types import SimpleNamespace

from fastapi import Depends, FastAPI
from fastapi.testclient import TestClient

from apps.api.core.security import get_current_active_user
from apps.api.core import tenancy
from apps.api.database import get_db


class _FakeDialect:
    name = "postgresql"


class _FakeBind:
    dialect = _FakeDialect()


class _FakeSession:
    def __init__(self):
        self.workspace_id = None

    def get_bind(self):
        return _FakeBind()

    def execute(self, _statement, parameters):
        self.workspace_id = parameters["workspace_id"]


def test_current_workspace_context_reaches_async_endpoint(monkeypatch):
    """Regression: sync dependencies write ContextVars in a thread copy."""
    monkeypatch.setattr(tenancy.ws_manager, "get_user_active_workspace", lambda _uid: "ws-1")
    monkeypatch.setattr(tenancy.ws_manager, "is_member", lambda _ws, _uid: True)
    monkeypatch.setattr(tenancy.ws_manager, "workspace_slug", lambda _ws: "main")

    app = FastAPI()
    fake_db = _FakeSession()
    app.dependency_overrides[get_current_active_user] = lambda: SimpleNamespace(id=1)
    app.dependency_overrides[get_db] = lambda: fake_db

    @app.get("/probe")
    async def probe(_ctx=Depends(tenancy.current_workspace)):
        return {"workspace_id": tenancy.current_workspace_var.get()}

    token = tenancy.current_workspace_var.set(None)
    try:
        response = TestClient(app).get("/probe")
    finally:
        tenancy.current_workspace_var.reset(token)

    assert response.status_code == 200
    assert response.json() == {"workspace_id": "ws-1"}
    assert fake_db.workspace_id == "ws-1"
