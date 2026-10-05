"""PostgreSQL workspace metadata, secrets and settings (PG-gated), plus unit tests.

    TEST_DATABASE_URL=postgresql+psycopg://postgres:postgres@127.0.0.1:55432/postgres \
        uv run pytest tests/test_multihost_meta_pg.py

Covers what the parity module cannot: that the data really is in PostgreSQL,
forced row-level security on every new table (a tenant-bound connection sees
only its own workspace, and nothing can read another workspace's secrets, not
even through the control-plane flag), secrets stored as ciphertext and never in
logs, global credential settings enveloped at rest, per-workspace secret
rotation, and refusal to run as a role that would make RLS inert.
"""
import logging
import uuid

import pytest
from sqlalchemy import create_engine, text

from apps.api.services.workspace import pg_meta
from tests.multihost_support import TEST_DATABASE_URL, temp_database

needs_pg = pytest.mark.skipif(
    not TEST_DATABASE_URL, reason="TEST_DATABASE_URL not set (PostgreSQL metadata tests skipped)"
)

# ── pure translation unit tests (no database) ───────────────────────────────


def test_translate_placeholders_and_percent():
    assert pg_meta.translate("SELECT 1 WHERE a = ? AND b LIKE 'x%'") == \
        "SELECT 1 WHERE a = %s AND b LIKE 'x%%'"


def test_translate_insert_or_replace_becomes_upsert_on_the_primary_key():
    sql = ("INSERT OR REPLACE INTO workspace_members "
           "(workspace_id, user_id, role, created_at) VALUES (?, ?, ?, ?)")
    out = pg_meta.translate(sql)
    assert out == ("INSERT INTO workspace_members (workspace_id, user_id, role, created_at) "
                   "VALUES (%s, %s, %s, %s) ON CONFLICT (workspace_id, user_id) DO UPDATE SET "
                   "role = EXCLUDED.role, created_at = EXCLUDED.created_at")
    assert pg_meta.translate(
        "INSERT OR REPLACE INTO user_active_workspace (user_id, workspace_id) VALUES (?, ?)"
    ).endswith("ON CONFLICT (user_id) DO UPDATE SET workspace_id = EXCLUDED.workspace_id")


def test_translate_insert_or_ignore_and_begin_immediate():
    out = pg_meta.translate(
        "INSERT OR IGNORE INTO workspace_scim_group_members (a, b) VALUES (?, ?)")
    assert out == "INSERT INTO workspace_scim_group_members (a, b) VALUES (%s, %s) ON CONFLICT DO NOTHING"
    assert pg_meta.translate("BEGIN IMMEDIATE").startswith("SELECT pg_advisory_xact_lock(")


def test_translate_rejects_replace_into_unknown_table():
    with pytest.raises(ValueError):
        pg_meta.translate("INSERT OR REPLACE INTO nope (a) VALUES (?)")


def test_backend_selection_is_validated(monkeypatch):
    from apps.api.core.config import settings
    monkeypatch.setattr(settings, "WORKSPACE_META_STORE", "sqlite")
    assert not pg_meta.is_postgres()
    monkeypatch.setattr(settings, "WORKSPACE_META_STORE", "mysql")
    with pytest.raises(ValueError):
        pg_meta.backend()


def test_postgres_store_on_sqlite_database_is_a_configuration_error(monkeypatch):
    from apps.api.core.config import settings
    monkeypatch.setattr(settings, "WORKSPACE_META_STORE", "postgres")
    pg_meta.use_engine(None)
    with pytest.raises(RuntimeError, match="requires a PostgreSQL"):
        pg_meta.directory()


def test_sensitive_setting_classification():
    for key in ("OPENAI_API_KEY", "SMTP_PASSWORD", "HUBSPOT_TOKEN", "STRIPE_WEBHOOK_SECRET", "LINKEDIN_LI_AT_COOKIE"):
        assert pg_meta.is_sensitive_setting(key), key
    for key in ("LLM_DEFAULT_PROVIDER", "OPENAI_BASE_URL", "ENRICH_ROW_CONCURRENCY", "ACTIVE_WORKSPACE"):
        assert not pg_meta.is_sensitive_setting(key), key


# ── PostgreSQL ──────────────────────────────────────────────────────────────

@pytest.fixture(scope="module")
def pgdb():
    with temp_database() as d:
        yield d


@pytest.fixture
def pg(pgdb, monkeypatch):
    from apps.api.core.config import settings

    owner = create_engine(pgdb.owner_url)
    with owner.begin() as c:
        c.execute(text(
            "TRUNCATE workspaces, workspace_settings, workspace_members, workspace_member_permissions, "
            "workspace_oidc_identities, workspace_scim_tokens, workspace_scim_users, workspace_scim_groups, "
            "workspace_scim_group_members, user_active_workspace, app_settings"))
    engine = create_engine(pgdb.app_url, pool_size=5, max_overflow=0)
    pg_meta._ready.clear()
    pg_meta.use_engine(engine)
    monkeypatch.setattr(settings, "WORKSPACE_META_STORE", "postgres")
    yield owner, engine
    pg_meta.use_engine(None)
    engine.dispose()
    owner.dispose()


def _scalar(engine, sql, **params):
    with engine.begin() as c:
        return c.execute(text(sql), params).scalar()


@needs_pg
def test_data_really_lives_in_postgres_not_in_a_local_file(pg, tmp_path, monkeypatch):
    from pathlib import Path
    from apps.api.services.workspace import manager
    owner, _ = pg
    monkeypatch.setattr(manager, "_project_root", lambda: Path(tmp_path))
    ws = manager.create_workspace("Where am I", owner_id=7)
    assert _scalar(owner, "SELECT count(*) FROM workspaces WHERE id = :i", i=ws.id) == 1
    assert _scalar(owner, "SELECT role FROM workspace_members WHERE workspace_id = :i AND user_id = 7",
                   i=ws.id) == "owner"
    assert not (tmp_path / "data" / "workspaces.db").exists()
    assert _scalar(owner, "SELECT count(*) FROM workspaces WHERE slug = 'main'") == 1


@needs_pg
def test_every_new_table_forces_row_level_security(pg):
    owner, _ = pg
    with owner.begin() as c:
        rows = c.execute(text(
            "SELECT relname, relrowsecurity, relforcerowsecurity FROM pg_class "
            "WHERE relname = ANY(:t)"), {"t": list(pg_meta.PRIMARY_KEYS)}).all()
    assert {r[0] for r in rows} == set(pg_meta.PRIMARY_KEYS)
    assert all(r[1] and r[2] for r in rows), rows


@needs_pg
def test_workspace_settings_policy_admits_only_the_bound_workspace(pg):
    """The strictest table: nothing but app.workspace_id = row's workspace opens
    it. Not another workspace, not the control-plane flag, not no binding."""
    from apps.api.services.workspace import manager
    from apps.api.services.workspace.secrets import set_secret
    owner, engine = pg
    a = manager.create_workspace("Tenant A", owner_id=1)
    b = manager.create_workspace("Tenant B", owner_id=1)
    set_secret(a.id, "HUBSPOT_API_KEY", "a-secret-value")
    set_secret(b.id, "HUBSPOT_API_KEY", "b-secret-value")

    def visible(guc_ws=None, control=None):
        with engine.begin() as c:
            if guc_ws:
                c.execute(text("SELECT set_config('app.workspace_id', :w, true)"), {"w": guc_ws})
            if control:
                c.execute(text("SELECT set_config('app.control_plane', :v, true)"), {"v": control})
            return {r[0] for r in c.execute(text("SELECT workspace_id FROM workspace_settings"))}

    assert visible() == set()                         # unbound: fail closed
    assert visible(guc_ws=a.id) == {a.id}
    assert visible(guc_ws=b.id) == {b.id}
    assert visible(control="on") == set()             # the directory flag is NOT a secrets key
    assert visible(guc_ws="no-such-workspace") == set()

    with pytest.raises(Exception) as err:             # WITH CHECK: cannot write another tenant's row
        with engine.begin() as c:
            c.execute(text("SELECT set_config('app.workspace_id', :w, true)"), {"w": a.id})
            c.execute(text("INSERT INTO workspace_settings VALUES (:w, 'X', 'enc:v1:forged')"), {"w": b.id})
    assert "row-level security" in str(err.value)

    with engine.begin() as c:                         # and cannot rewrite or delete it
        c.execute(text("SELECT set_config('app.workspace_id', :w, true)"), {"w": a.id})
        assert c.execute(text("UPDATE workspace_settings SET value = 'x' WHERE workspace_id = :w"),
                         {"w": b.id}).rowcount == 0
        assert c.execute(text("DELETE FROM workspace_settings WHERE workspace_id = :w"),
                         {"w": b.id}).rowcount == 0
    assert _scalar(owner, "SELECT count(*) FROM workspace_settings WHERE workspace_id = :w", w=b.id) == 1


@needs_pg
def test_directory_tables_hide_other_workspaces_from_a_tenant_bound_session(pg):
    from apps.api.services.workspace import manager, scim
    owner, engine = pg
    a = manager.create_workspace("Dir A", owner_id=1)
    b = manager.create_workspace("Dir B", owner_id=2)
    manager.add_member(a.id, 3, "viewer")
    scim.rotate_token(a.id, 1)
    scim.rotate_token(b.id, 2)
    tables = {
        "workspaces": "id", "workspace_members": "workspace_id",
        "workspace_scim_tokens": "workspace_id", "user_active_workspace": "workspace_id",
    }
    manager.set_user_active_workspace(1, a.id)
    manager.set_user_active_workspace(2, b.id)
    with engine.begin() as c:                         # what a tenant-bound ORM session sees
        c.execute(text("SELECT set_config('app.workspace_id', :w, true)"), {"w": a.id})
        for table, col in tables.items():
            seen = {r[0] for r in c.execute(text(f"SELECT {col} FROM {table}"))}
            assert seen == {a.id}, (table, seen)
    with engine.begin() as c:                         # unbound: nothing
        for table in tables:
            assert c.execute(text(f"SELECT count(*) FROM {table}")).scalar() == 0
    # the directory path itself can answer cross-tenant questions
    assert set(manager.list_user_workspace_ids(1)) == {a.id}
    assert {w.id for w in manager.list_workspaces()} >= {a.id, b.id}


@needs_pg
def test_secrets_are_ciphertext_at_rest_isolated_and_decrypt_through_the_api(pg, caplog):
    from apps.api.services.workspace import manager
    from apps.api.services.workspace.secrets import (
        get_secret, get_secret_with_source, has_workspace_secret, set_secret)
    owner, _ = pg
    a = manager.create_workspace("Secrets A", owner_id=1)
    b = manager.create_workspace("Secrets B", owner_id=1)
    plaintext = "sk-live-" + uuid.uuid4().hex
    with caplog.at_level(logging.DEBUG):
        set_secret(a.id, "SMTP_PASSWORD", plaintext)
        assert get_secret(a.id, "SMTP_PASSWORD") == plaintext
    stored = _scalar(owner, "SELECT value FROM workspace_settings WHERE workspace_id = :w AND key = 'SMTP_PASSWORD'",
                     w=a.id)
    assert stored.startswith("enc:v1:") and plaintext not in stored
    assert plaintext not in caplog.text
    assert get_secret_with_source(b.id, "SMTP_PASSWORD", "dflt") == ("dflt", "default")
    assert has_workspace_secret(a.id, "SMTP_PASSWORD") and not has_workspace_secret(b.id, "SMTP_PASSWORD")
    set_secret(a.id, "SMTP_PASSWORD", "   ")           # blank does not clobber (existing contract)
    assert get_secret(a.id, "SMTP_PASSWORD") == plaintext


@needs_pg
def test_rotation_reencrypts_every_workspace_and_keeps_values(pg):
    from apps.api.services.workspace import manager
    from apps.api.services.workspace.secrets import get_secret, rotate_encrypted_secrets, set_secret
    owner, _ = pg
    ids = [manager.create_workspace(f"Rot {i}", owner_id=1).id for i in range(3)]
    for i, wid in enumerate(ids):
        set_secret(wid, "CRM_TOKEN", f"token-{i}")
    def snapshot():
        with owner.connect() as c:
            return {r[0]: r[1] for r in c.execute(
                text("SELECT workspace_id, value FROM workspace_settings WHERE key = 'CRM_TOKEN'"))}

    before = snapshot()
    assert rotate_encrypted_secrets() == {"rotated": 3}
    after = snapshot()
    assert all(after[w] != before[w] and after[w].startswith("enc:v1:") for w in ids)
    assert [get_secret(w, "CRM_TOKEN") for w in ids] == ["token-0", "token-1", "token-2"]


@needs_pg
def test_global_settings_envelope_credentials_and_leave_plain_keys_readable(pg, monkeypatch):
    from apps.api.routers import settings as settings_router
    owner, engine = pg
    monkeypatch.delenv("OPENAI_API_KEY", raising=False)
    monkeypatch.delenv("LLM_DEFAULT_PROVIDER", raising=False)
    settings_router._db_set("OPENAI_API_KEY", "sk-global-" + "z" * 12)
    settings_router._db_set("LLM_DEFAULT_PROVIDER", "anthropic")
    with owner.connect() as c:
        raw = dict(c.execute(text("SELECT key, value FROM app_settings")).all())
    assert raw["OPENAI_API_KEY"].startswith("enc:v1:") and "sk-global" not in raw["OPENAI_API_KEY"]
    assert raw["LLM_DEFAULT_PROVIDER"] == "anthropic"
    monkeypatch.delenv("OPENAI_API_KEY")                # force the read through the database
    assert settings_router._db_get("OPENAI_API_KEY") == "sk-global-" + "z" * 12
    assert settings_router._db_get("LLM_DEFAULT_PROVIDER") == "anthropic"
    # a tenant-bound session cannot read the global settings at all
    with engine.begin() as c:
        c.execute(text("SELECT set_config('app.workspace_id', 'anything', true)"))
        assert c.execute(text("SELECT count(*) FROM app_settings")).scalar() == 0


@needs_pg
def test_global_settings_seed_does_not_overwrite_and_is_idempotent(pg, monkeypatch):
    from apps.api.routers import settings as settings_router
    monkeypatch.setenv("ANTHROPIC_API_KEY", "from-env-1")
    monkeypatch.setenv("LLM_DEFAULT_PROVIDER", "openrouter")
    settings_router._seed_from_env()
    settings_router._seed_from_env()
    assert pg_meta.settings_keys() >= {"ANTHROPIC_API_KEY", "LLM_DEFAULT_PROVIDER"}
    monkeypatch.setenv("ANTHROPIC_API_KEY", "from-env-2")
    settings_router._seed_from_env()
    assert pg_meta.settings_get("ANTHROPIC_API_KEY") == "from-env-1"


@needs_pg
def test_two_hosts_see_one_directory(pg, pgdb):
    """Two separate engines (stand-ins for two hosts) share membership writes."""
    from apps.api.services.workspace import manager
    host_a = pg[1]
    ws = manager.create_workspace("Shared", owner_id=1)
    host_b = create_engine(pgdb.app_url, pool_size=2, max_overflow=0)
    try:
        pg_meta.use_engine(host_b)
        assert manager.is_member(ws.id, 1)
        manager.add_member(ws.id, 9, "editor")
        pg_meta.use_engine(host_a)
        assert manager.member_role(ws.id, 9) == "editor"
    finally:
        host_b.dispose()


@needs_pg
def test_refuses_a_role_that_bypasses_row_level_security(pgdb, monkeypatch):
    from apps.api.core.config import settings
    monkeypatch.setattr(settings, "WORKSPACE_META_STORE", "postgres")
    monkeypatch.setattr(settings, "PG_RLS_REQUIRE_SAFE_ROLE", True)
    superuser = create_engine(pgdb.owner_url)
    pg_meta.use_engine(superuser)
    try:
        with pytest.raises(RuntimeError, match="bypasses row-level security"):
            pg_meta.directory()
    finally:
        pg_meta.use_engine(None)
        superuser.dispose()
