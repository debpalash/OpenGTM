import asyncio
from pathlib import Path

import pytest
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker

from apps.api.database import Base
from apps.api.models import User
from apps.api.routers.auth import sso_callback
from apps.api.services.workspace import manager, oidc


def _session():
    engine = create_engine("sqlite:///:memory:")
    Base.metadata.create_all(engine, tables=[User.__table__])
    return sessionmaker(bind=engine)()


def test_oidc_config_is_allowlisted_and_secret_is_encrypted(monkeypatch, tmp_path):
    monkeypatch.setattr(manager, "_project_root", lambda: Path(tmp_path))
    monkeypatch.setattr(oidc.settings, "SSO_ALLOWED_ISSUER_HOSTS", "login.example.com")
    workspace = manager.create_workspace("OIDC Config", owner_id=1)

    with pytest.raises(ValueError, match="not approved"):
        oidc.save_config(workspace.id, {"enabled": True, "issuer": "https://evil.example", "client_id": "client"}, "secret")
    saved = oidc.save_config(
        workspace.id,
        {
            "enabled": True,
            "issuer": "https://login.example.com/tenant/",
            "client_id": "client",
            "allowed_domains": ["EXAMPLE.COM", "example.com"],
            "auto_provision": True,
            "default_role": "viewer",
        },
        "top-secret",
    )
    assert saved["issuer"] == "https://login.example.com/tenant"
    assert saved["allowed_domains"] == ["example.com"]
    raw = manager.get_workspace_setting(workspace.id, oidc.SECRET_KEY)
    assert raw and "top-secret" not in raw
    assert oidc.get_secret(workspace.id, oidc.SECRET_KEY) == "top-secret"


def test_oidc_callback_jit_provisions_binds_and_deprovisions(monkeypatch, tmp_path):
    monkeypatch.setattr(manager, "_project_root", lambda: Path(tmp_path))
    workspace = manager.create_workspace("OIDC Login", owner_id=1)
    config = {
        "enabled": True,
        "issuer": "https://login.example.com",
        "client_id": "client",
        "allowed_domains": ["example.com"],
        "auto_provision": True,
        "default_role": "viewer",
    }
    manager.set_workspace_setting(workspace.id, oidc.CONFIG_KEY, __import__("json").dumps(config))

    class Client:
        async def authorize_access_token(self, request):
            return {"userinfo": {"sub": "subject-1", "email": "Person@Example.com", "email_verified": True}}

    monkeypatch.setattr(oidc, "client_for", lambda _: Client())
    db = _session()
    db.add(User(id=1, username="owner", hashed_password="x"))
    db.commit()
    response = asyncio.run(sso_callback(workspace.slug, object(), db))
    assert response.status_code == 307
    user = db.query(User).filter(User.username == "person@example.com").one()
    assert manager.member_role(workspace.id, user.id) == "viewer"
    assert manager.oidc_identity_user(workspace.id, config["issuer"], "subject-1") == user.id
    assert "sso_access_token=" in response.headers["location"]

    manager.remove_member(workspace.id, user.id)
    assert manager.oidc_identity_user(workspace.id, config["issuer"], "subject-1") is None
    db.close()


def test_oidc_callback_rejects_unverified_or_wrong_domain(monkeypatch, tmp_path):
    monkeypatch.setattr(manager, "_project_root", lambda: Path(tmp_path))
    workspace = manager.create_workspace("OIDC Reject", owner_id=1)
    manager.set_workspace_setting(workspace.id, oidc.CONFIG_KEY, __import__("json").dumps({
        "enabled": True, "issuer": "https://login.example.com", "client_id": "client",
        "allowed_domains": ["example.com"], "auto_provision": True, "default_role": "viewer",
    }))

    class Client:
        async def authorize_access_token(self, request):
            return {"userinfo": {"sub": "subject-2", "email": "person@other.com", "email_verified": True}}

    monkeypatch.setattr(oidc, "client_for", lambda _: Client())
    db = _session()
    response = asyncio.run(sso_callback(workspace.slug, object(), db))
    assert response.status_code == 303
    assert "sso_error=Email%20domain%20is%20not%20allowed" in response.headers["location"]
    assert db.query(User).count() == 0
    db.close()
