"""
WI-6 — per-workspace encrypted integration secrets.

Covers: set/get round-trip, ciphertext at rest (not equal to plaintext),
per-workspace isolation, fallback to the global setting, and that output.py's
CRM path uses the per-workspace secret when present (CRM send mocked).
"""
import asyncio
import os
import sqlite3

import pytest

# Dev env so the master key derives from SECRET_KEY without failing closed.
os.environ.setdefault("APP_ENV", "test")

from apps.api.services.workspace import secrets as secrets_mod


@pytest.fixture
def temp_ws_db(tmp_path, monkeypatch):
    """Point the secrets store at an isolated sqlite DB with the real schema."""
    db_path = tmp_path / "workspaces.db"

    def _fake_get_db():
        conn = sqlite3.connect(str(db_path))
        conn.row_factory = sqlite3.Row
        conn.execute(
            """CREATE TABLE IF NOT EXISTS workspace_settings (
                   workspace_id TEXT NOT NULL,
                   key TEXT NOT NULL,
                   value TEXT DEFAULT '',
                   PRIMARY KEY (workspace_id, key)
               )"""
        )
        return conn

    monkeypatch.setattr(
        "apps.api.services.workspace.manager._get_db", _fake_get_db
    )
    return db_path


def _raw_stored(db_path, workspace_id, key):
    conn = sqlite3.connect(str(db_path))
    row = conn.execute(
        "SELECT value FROM workspace_settings WHERE workspace_id=? AND key=?",
        (workspace_id, key),
    ).fetchone()
    conn.close()
    return row[0] if row else None


def test_set_get_roundtrip(temp_ws_db):
    secrets_mod.set_secret("ws-A", "HUBSPOT_TOKEN", "pat-na1-secret")
    assert secrets_mod.get_secret("ws-A", "HUBSPOT_TOKEN") == "pat-na1-secret"


def test_at_rest_is_ciphertext(temp_ws_db):
    secrets_mod.set_secret("ws-A", "HUBSPOT_TOKEN", "pat-na1-secret")
    stored = _raw_stored(temp_ws_db, "ws-A", "HUBSPOT_TOKEN")
    assert stored is not None
    # Never store plaintext; must be the encrypted envelope.
    assert stored != "pat-na1-secret"
    assert "pat-na1-secret" not in stored
    assert stored.startswith("enc:v1:")
    # And it decrypts back.
    assert secrets_mod.decrypt_value(stored) == "pat-na1-secret"


def test_per_workspace_isolation(temp_ws_db):
    secrets_mod.set_secret("ws-A", "HUBSPOT_TOKEN", "token-A")
    secrets_mod.set_secret("ws-B", "HUBSPOT_TOKEN", "token-B")
    assert secrets_mod.get_secret("ws-A", "HUBSPOT_TOKEN") == "token-A"
    assert secrets_mod.get_secret("ws-B", "HUBSPOT_TOKEN") == "token-B"
    # A's secret must not be visible to B.
    assert not secrets_mod.has_workspace_secret("ws-B", "MISSING_KEY")
    assert secrets_mod.has_workspace_secret("ws-A", "HUBSPOT_TOKEN")


def test_fallback_to_global(temp_ws_db, monkeypatch):
    # No per-workspace secret → falls back to the global setting.
    monkeypatch.setattr(
        "apps.api.routers.settings._db_get",
        lambda key, default="": "global-token" if key == "HUBSPOT_TOKEN" else default,
    )
    value, source = secrets_mod.get_secret_with_source("ws-A", "HUBSPOT_TOKEN")
    assert value == "global-token"
    assert source == "global"

    # When a per-workspace secret exists it WINS over global.
    secrets_mod.set_secret("ws-A", "HUBSPOT_TOKEN", "ws-token")
    value, source = secrets_mod.get_secret_with_source("ws-A", "HUBSPOT_TOKEN")
    assert value == "ws-token"
    assert source == "workspace"


def test_default_when_nothing_set(temp_ws_db, monkeypatch):
    monkeypatch.setattr("apps.api.routers.settings._db_get", lambda key, default="": "")
    monkeypatch.delenv("HUBSPOT_TOKEN", raising=False)
    value, source = secrets_mod.get_secret_with_source("ws-A", "HUBSPOT_TOKEN", "fallback")
    assert value == "fallback"
    assert source == "default"


def test_blank_value_does_not_clobber(temp_ws_db):
    secrets_mod.set_secret("ws-A", "HUBSPOT_TOKEN", "keep-me")
    secrets_mod.set_secret("ws-A", "HUBSPOT_TOKEN", "   ")  # ignored
    assert secrets_mod.get_secret("ws-A", "HUBSPOT_TOKEN") == "keep-me"


def test_output_crm_uses_workspace_secret(temp_ws_db, monkeypatch):
    """execute_output_column's CRM path must resolve the per-workspace token."""
    secrets_mod.set_secret("ws-A", "HUBSPOT_TOKEN", "ws-hubspot-token")

    captured = {}

    from apps.api.services.crm import hubspot

    async def fake_push(lead, field_map=None, workspace_id=None):
        # Record which token the client resolved for this workspace.
        captured["token"] = hubspot._get_token(workspace_id)
        captured["workspace_id"] = workspace_id
        return {"success": True, "action": "created", "hubspot_id": "123"}

    monkeypatch.setattr(hubspot, "push_lead_as_contact", fake_push)
    # is_connected just needs to see a token for the workspace.
    monkeypatch.setattr(
        hubspot, "is_connected", lambda workspace_id=None: bool(hubspot._get_token(workspace_id))
    )

    from apps.api.services.workbook.output import execute_output_column

    col_config = {"destination": "crm", "destination_config": {"type": "hubspot"}}
    lead_data = {"company": "Acme", "email": "a@acme.com"}

    res = asyncio.run(execute_output_column(
        col_config=col_config,
        lead_data=lead_data,
        columns_config=[],
        workbook_id="wb-1",
        lead_id=1,
        workspace_id="ws-A",
    ))

    assert res["success"] is True
    assert captured["workspace_id"] == "ws-A"
    assert captured["token"] == "ws-hubspot-token"


def test_output_crm_falls_back_to_global(temp_ws_db, monkeypatch):
    """With no per-workspace token, the CRM path uses the global token."""
    monkeypatch.setattr(
        "apps.api.routers.settings._db_get",
        lambda key, default="": "global-hubspot" if key == "HUBSPOT_TOKEN" else default,
    )
    captured = {}
    from apps.api.services.crm import hubspot

    async def fake_push(lead, field_map=None, workspace_id=None):
        captured["token"] = hubspot._get_token(workspace_id)
        return {"success": True, "action": "created", "hubspot_id": "9"}

    monkeypatch.setattr(hubspot, "push_lead_as_contact", fake_push)
    monkeypatch.setattr(
        hubspot, "is_connected", lambda workspace_id=None: bool(hubspot._get_token(workspace_id))
    )

    from apps.api.services.workbook.output import execute_output_column

    res = asyncio.run(execute_output_column(
        col_config={"destination": "crm", "destination_config": {"type": "hubspot"}},
        lead_data={"company": "Acme", "email": "a@acme.com"},
        columns_config=[],
        workbook_id="wb-1",
        lead_id=1,
        workspace_id="ws-without-secret",
    ))
    assert res["success"] is True
    assert captured["token"] == "global-hubspot"
