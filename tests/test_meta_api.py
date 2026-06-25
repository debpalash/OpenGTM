"""Meta endpoints — /api/me/context and /api/flags (offline, deps overridden).

Auth/workspace deps are overridden so we exercise the role + flags logic without a
real auth stack, mirroring tests/test_automations_api.py.
"""

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from apps.api.core.config import settings
from apps.api.core.tenancy import WorkspaceCtx, current_workspace
from apps.api.routers.meta import router as meta_router
from apps.api.services.workspace import manager as ws_manager

WS = "ws_meta"


class _User:
    id = "user-meta"


@pytest.fixture()
def app():
    app = FastAPI()
    app.include_router(meta_router)
    app.dependency_overrides[current_workspace] = lambda: WorkspaceCtx(
        user=_User(), workspace_id=WS, slug="meta"
    )
    return app


# ── /api/me/context ────────────────────────────────────────────────────────

@pytest.mark.parametrize("role", ["admin", "editor", "member", "owner"])
def test_me_context_reports_role(app, monkeypatch, role):
    monkeypatch.setattr(ws_manager, "member_role", lambda ws, uid: role)
    r = TestClient(app).get("/api/me/context")
    assert r.status_code == 200
    body = r.json()
    assert body["role"] == role
    assert body["workspace_id"] == WS
    assert body["user_id"] == "user-meta"
    assert body["is_owner"] is (role == "owner")


def test_me_context_missing_role_falls_back_to_member(app, monkeypatch):
    monkeypatch.setattr(ws_manager, "member_role", lambda ws, uid: None)
    r = TestClient(app).get("/api/me/context")
    assert r.status_code == 200
    assert r.json()["role"] == "member"
    assert r.json()["is_owner"] is False


def test_me_context_requires_auth():
    """Without the override, current_workspace runs → unauthenticated 401/403."""
    bare = FastAPI()
    bare.include_router(meta_router)
    r = TestClient(bare).get("/api/me/context")
    assert r.status_code in (401, 403)


# ── /api/flags ─────────────────────────────────────────────────────────────

def test_flags_reflect_config(app, monkeypatch):
    monkeypatch.setattr(settings, "AUTOMATIONS_ENABLED", True, raising=False)
    monkeypatch.setattr(settings, "INTENT_POLLER_ENABLED", False, raising=False)
    monkeypatch.setattr(settings, "PG_LEAD_STORE", True, raising=False)
    monkeypatch.setattr(
        settings, "AUTOMATIONS_ALLOW_LEGACY_OUTREACH", True, raising=False
    )
    # Force the effective pg helper deterministic regardless of DB backend.
    import apps.api.services.leadgen.store as store
    monkeypatch.setattr(store, "use_pg_store", lambda: True)

    r = TestClient(app).get("/api/flags")
    assert r.status_code == 200
    assert r.json() == {
        "automations_enabled": True,
        "intent_poller_enabled": False,
        "pg_lead_store": True,
        "allow_legacy_outreach": True,
    }


def test_flags_pg_reports_effective_availability(app, monkeypatch):
    """pg_lead_store reflects use_pg_store(), not the raw config bool."""
    monkeypatch.setattr(settings, "PG_LEAD_STORE", True, raising=False)
    import apps.api.services.leadgen.store as store
    monkeypatch.setattr(store, "use_pg_store", lambda: False)

    r = TestClient(app).get("/api/flags")
    assert r.status_code == 200
    assert r.json()["pg_lead_store"] is False


def test_flags_defaults_off(app, monkeypatch):
    monkeypatch.setattr(settings, "AUTOMATIONS_ENABLED", False, raising=False)
    monkeypatch.setattr(settings, "INTENT_POLLER_ENABLED", False, raising=False)
    monkeypatch.setattr(
        settings, "AUTOMATIONS_ALLOW_LEGACY_OUTREACH", False, raising=False
    )
    import apps.api.services.leadgen.store as store
    monkeypatch.setattr(store, "use_pg_store", lambda: False)

    r = TestClient(app).get("/api/flags")
    body = r.json()
    assert body["automations_enabled"] is False
    assert body["intent_poller_enabled"] is False
    assert body["allow_legacy_outreach"] is False
