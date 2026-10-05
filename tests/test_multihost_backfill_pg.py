"""Backfill, verification and restore proof for the metadata stores (PG-gated).

    TEST_DATABASE_URL=postgresql+psycopg://postgres:postgres@127.0.0.1:55432/postgres \
        uv run pytest tests/test_multihost_backfill_pg.py

The sources are built by the real single-host code (the SQLite workspace
manager, SCIM, OIDC and the secrets module) so the backfill is exercised on
exactly what a production install has on disk, including envelope-encrypted
per-workspace secrets and plaintext global credentials.
"""
import hashlib
import json
import shutil
import sqlite3
import subprocess
import time
from pathlib import Path

import pytest
from cryptography.fernet import Fernet
from sqlalchemy import create_engine, text

from apps.api.services.workspace import backfill, pg_meta
from tests.multihost_support import TEST_DATABASE_URL, temp_database

pytestmark = pytest.mark.skipif(
    not TEST_DATABASE_URL, reason="TEST_DATABASE_URL not set (backfill tests skipped)"
)

PLAIN_WS_SECRET = "hubspot-pat-" + "a1b2c3" * 4
PLAIN_GLOBAL_KEY = "sk-ant-global-" + "9f8e7d" * 4
CONTROL_PLANE_TABLES = (
    "workspaces, workspace_settings, workspace_members, workspace_member_permissions, "
    "workspace_oidc_identities, workspace_scim_tokens, workspace_scim_users, "
    "workspace_scim_groups, workspace_scim_group_members, user_active_workspace, app_settings"
)


@pytest.fixture(scope="module")
def pgdb():
    with temp_database() as d:
        yield d


@pytest.fixture
def source(tmp_path, monkeypatch):
    """A realistic single-host install: workspaces.db + data.db under tmp/data."""
    from apps.api.core.config import settings
    from apps.api.services.workspace import manager, scim
    from apps.api.services.workspace.secrets import set_secret

    monkeypatch.setattr(settings, "SECRETS_MASTER_KEY", Fernet.generate_key().decode())
    monkeypatch.setattr(settings, "WORKSPACE_META_STORE", "sqlite")
    monkeypatch.setattr(manager, "_project_root", lambda: Path(tmp_path))
    main_ws = manager.get_workspace_by_slug("main")
    a = manager.create_workspace("Acme Corp", owner_id=1)
    b = manager.create_workspace("Beta Inc", owner_id=2)
    manager.add_member(a.id, 3, "editor")
    manager.add_member(a.id, 4, "viewer")
    manager.add_member(main_ws.id, 1, "owner")
    manager.set_member_permission(a.id, 3, "tables.write", "deny")
    manager.set_member_permission(a.id, 4, "tables.export", "allow")
    manager.bind_oidc_identity(a.id, "https://idp.example", "sub-3", 3, "e@acme.test")
    manager.set_user_active_workspace(3, a.id)
    manager.set_user_active_workspace(1, b.id)
    manager.set_workspace_setting(a.id, "oidc_config", json.dumps({"enabled": True}))
    set_secret(a.id, "HUBSPOT_API_KEY", PLAIN_WS_SECRET)
    set_secret(b.id, "SMTP_PASSWORD", "smtp-" + "q" * 20)
    set_secret(b.id, "SALESFORCE_TOKEN", "sf-" + "z" * 20)
    scim.rotate_token(a.id, 1)
    # real SCIM rows through the SQLite connection the manager owns
    conn = manager._get_db()
    now = time.time()
    conn.execute("INSERT INTO workspace_scim_users VALUES (?,?,?,?,?,?,?)", (a.id, 3, "ext-3", "Ann", 1, now, now))
    conn.execute("INSERT INTO workspace_scim_groups VALUES (?,?,?,?,?,?)", ("g1", a.id, "", "Sales", now, now))
    conn.execute("INSERT INTO workspace_scim_group_members VALUES (?,?,?,?)", (a.id, "g1", 3, now))
    conn.commit()
    conn.close()
    # global settings: credential keys were plaintext in data.db
    data = Path(tmp_path) / "data" / "data.db"
    d = sqlite3.connect(data)
    d.execute("CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT DEFAULT '')")
    d.executemany("INSERT INTO settings VALUES (?, ?)", [
        ("ANTHROPIC_API_KEY", PLAIN_GLOBAL_KEY), ("LLM_DEFAULT_PROVIDER", "anthropic"),
        ("ENRICH_ROW_CONCURRENCY", "12"), ("ACTIVE_WORKSPACE", a.id), ("EMPTY_ONE", "")])
    d.commit()
    d.close()
    return Path(tmp_path) / "data", {"main": main_ws, "a": a, "b": b}


@pytest.fixture
def pg(pgdb, monkeypatch):
    from apps.api.core.config import settings
    owner = create_engine(pgdb.owner_url)
    with owner.begin() as c:
        c.execute(text(f"TRUNCATE {CONTROL_PLANE_TABLES}"))
    engine = create_engine(pgdb.app_url, pool_size=5, max_overflow=0)
    pg_meta._ready.clear()
    pg_meta.use_engine(engine)
    yield owner, engine
    pg_meta.use_engine(None)
    engine.dispose()
    owner.dispose()


def _digest_file(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def _scalar(engine, sql, **p):
    with engine.begin() as c:
        return c.execute(text(sql), p).scalar()


def test_run_copies_everything_and_verifies(source, pg):
    data, ws = source
    owner, _ = pg
    before = {f: _digest_file(data / f) for f in ("workspaces.db", "data.db")}
    res = backfill.backfill(str(data))
    checks = backfill.verify(str(data))
    assert all(c.ok for c in checks), [(c.table, c.missing_keys, c.extra_keys, c.changed_keys) for c in checks if not c.ok]
    assert {c.table: c.source_rows for c in checks}["workspaces"] == 3   # main, Acme, Beta
    assert {c.table: c.source_rows for c in checks}["workspace_settings"] == 4
    assert all(c.source_digest == c.target_digest for c in checks)
    assert res["workspaces"].inserted == 3
    # the target's auto-created 'main' was replaced by the source's, not duplicated
    assert _scalar(owner, "SELECT id FROM workspaces WHERE slug = 'main'") == ws["main"].id
    assert _scalar(owner, "SELECT count(*) FROM workspaces") == 3
    # sources are untouched (opened read-only)
    assert before == {f: _digest_file(data / f) for f in before}


def test_second_run_is_a_no_op(source, pg):
    data, _ = source
    first = backfill.backfill(str(data))
    d1 = {c.table: c.target_digest for c in backfill.verify(str(data))}
    again = backfill.backfill(str(data))
    d2 = {c.table: c.target_digest for c in backfill.verify(str(data))}
    assert d1 == d2
    for name, r in again.items():
        assert r.inserted == 0 and r.updated == 0, name
        assert r.skipped_existing == first[name].source_rows, name


def test_resumes_after_a_crash_mid_run(source, pg, monkeypatch):
    data, _ = source
    real = backfill._write_batch
    calls = {"n": 0}

    def flaky(table, rows, overwrite, res):
        calls["n"] += 1
        if calls["n"] == 4:
            raise RuntimeError("simulated crash")
        return real(table, rows, overwrite, res)

    monkeypatch.setattr(backfill, "_write_batch", flaky)
    with pytest.raises(RuntimeError, match="simulated crash"):
        backfill.backfill(str(data), batch=1)
    partial = backfill.verify(str(data))
    assert not all(c.ok for c in partial)           # a crash leaves it visibly incomplete ...
    monkeypatch.setattr(backfill, "_write_batch", real)
    rerun = backfill.backfill(str(data), batch=1)    # ... and the plain re-run completes it
    assert all(c.ok for c in backfill.verify(str(data)))
    assert sum(r.inserted for r in rerun.values()) > 0
    assert sum(r.skipped_existing for r in rerun.values()) > 0


def test_dry_run_writes_nothing(source, pg):
    data, _ = source
    owner, _ = pg
    backfill.backfill(str(data), dry_run=True)
    assert _scalar(owner, "SELECT count(*) FROM workspace_members") == 0
    assert _scalar(owner, "SELECT count(*) FROM app_settings") == 0


def test_verify_detects_missing_extra_and_changed_rows_without_leaking_values(source, pg):
    from apps.api.core.config import settings
    data, ws = source
    owner, _ = pg
    settings.WORKSPACE_META_STORE = "postgres"
    try:
        backfill.backfill(str(data))
        with owner.begin() as c:
            c.execute(text("UPDATE workspace_members SET role = 'viewer' WHERE workspace_id = :w AND user_id = 3"),
                      {"w": ws["a"].id})
            c.execute(text("DELETE FROM workspace_member_permissions WHERE user_id = 4"))
            c.execute(text("INSERT INTO workspace_members VALUES (:w, 99, 'member', 1.0)"), {"w": ws["a"].id})
            c.execute(text("UPDATE workspace_settings SET value = 'enc:v1:tampered' WHERE key = 'HUBSPOT_API_KEY'"))
        checks = {c.table: c for c in backfill.verify(str(data))}
    finally:
        settings.WORKSPACE_META_STORE = "sqlite"
    assert not checks["workspace_members"].ok
    assert checks["workspace_members"].changed_keys == [f"{ws['a'].id}/3"]
    assert checks["workspace_members"].extra_keys == [f"{ws['a'].id}/99"]
    assert checks["workspace_member_permissions"].missing_keys == [f"{ws['a'].id}/4/tables.export"]
    assert not checks["workspace_settings"].ok
    assert checks["workspace_settings"].changed_keys == [f"{ws['a'].id}/HUBSPOT_API_KEY"]
    assert "tampered" not in json.dumps(backfill.summarize(list(checks.values())))


def test_default_run_never_clobbers_newer_postgres_rows_but_overwrite_does(source, pg):
    data, ws = source
    owner, _ = pg
    backfill.backfill(str(data))
    with owner.begin() as c:
        c.execute(text("UPDATE workspace_members SET role = 'admin' WHERE workspace_id = :w AND user_id = 3"),
                  {"w": ws["a"].id})
    backfill.backfill(str(data))                       # default: skip existing
    assert _scalar(owner, "SELECT role FROM workspace_members WHERE workspace_id = :w AND user_id = 3",
                   w=ws["a"].id) == "admin"
    backfill.backfill(str(data), overwrite=True)       # explicit pre-cut-over refresh
    assert _scalar(owner, "SELECT role FROM workspace_members WHERE workspace_id = :w AND user_id = 3",
                   w=ws["a"].id) == "editor"
    assert all(c.ok for c in backfill.verify(str(data)))


def test_secrets_are_copied_as_ciphertext_and_work_through_the_application(source, pg, monkeypatch):
    from apps.api.core.config import settings
    from apps.api.routers import settings as settings_router
    from apps.api.services.workspace.secrets import get_secret
    data, ws = source
    owner, _ = pg
    src = sqlite3.connect(data / "workspaces.db")
    src_cipher = src.execute("SELECT value FROM workspace_settings WHERE key = 'HUBSPOT_API_KEY'").fetchone()[0]
    src.close()
    assert src_cipher.startswith("enc:v1:")
    backfill.backfill(str(data))
    assert _scalar(owner, "SELECT value FROM workspace_settings WHERE key = 'HUBSPOT_API_KEY'") == src_cipher
    # global credential: plaintext in data.db, enveloped in PostgreSQL, same plaintext through the API
    stored = _scalar(owner, "SELECT value FROM app_settings WHERE key = 'ANTHROPIC_API_KEY'")
    assert stored.startswith("enc:v1:") and PLAIN_GLOBAL_KEY not in stored
    assert _scalar(owner, "SELECT value FROM app_settings WHERE key = 'LLM_DEFAULT_PROVIDER'") == "anthropic"
    monkeypatch.setattr(settings, "WORKSPACE_META_STORE", "postgres")
    monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
    assert get_secret(ws["a"].id, "HUBSPOT_API_KEY") == PLAIN_WS_SECRET
    assert settings_router._db_get("ANTHROPIC_API_KEY") == PLAIN_GLOBAL_KEY
    report = backfill.check_decrypt()
    assert report["all_decrypt"] and report["envelopes"] == 4 and report["plaintext_secret_like"] == []


def test_check_decrypt_fails_closed_on_a_wrong_key_and_never_prints_values(source, pg, monkeypatch):
    from apps.api.core.config import settings
    data, _ = source
    backfill.backfill(str(data))
    monkeypatch.setattr(settings, "SECRETS_MASTER_KEY", Fernet.generate_key().decode())  # key lost / wrong
    report = backfill.check_decrypt()
    assert not report["all_decrypt"] and len(report["failed"]) == 4
    blob = json.dumps(report)
    assert PLAIN_WS_SECRET not in blob and PLAIN_GLOBAL_KEY not in blob and "enc:v1" not in blob


def test_default_workspace_conflict_is_refused_not_guessed(source, pg):
    data, _ = source
    # the target already has a 'main' with data of its own and a different id
    conn = pg_meta.directory()
    conn.execute("INSERT INTO workspace_members (workspace_id, user_id, role, created_at) "
                 "SELECT id, 5, 'owner', 1.0 FROM workspaces WHERE slug = 'main'")
    conn.commit()
    conn.close()
    with pytest.raises(RuntimeError, match="already has a 'main' workspace"):
        backfill.backfill(str(data))


def test_orphaned_source_rows_are_copied_and_reported_consistently(source, pg):
    """The SQLite files never enforced foreign keys; a leftover settings row for
    a deleted workspace must still verify, not be silently dropped."""
    data, _ = source
    src = sqlite3.connect(data / "workspaces.db")
    src.execute("INSERT INTO workspace_settings VALUES ('ghost-workspace', 'K', 'v')")
    src.commit()
    src.close()
    backfill.backfill(str(data))
    checks = {c.table: c for c in backfill.verify(str(data))}
    assert checks["workspace_settings"].ok and checks["workspace_settings"].source_rows == 5


def test_cli_run_verify_and_exit_codes(source, pg, capsys):
    from apps.api.scripts import multihost_backfill as cli
    data, _ = source
    assert cli.main(["run", "--data-dir", str(data), "--check-decrypt"]) == 0
    out = json.loads(capsys.readouterr().out)
    assert out["ok"] and out["verify"]["ok"] and out["decrypt"]["all_decrypt"]
    assert PLAIN_WS_SECRET not in json.dumps(out) and PLAIN_GLOBAL_KEY not in json.dumps(out)
    conn = pg_meta.directory()
    conn.execute("DELETE FROM workspace_members WHERE user_id = 4")
    conn.commit()
    conn.close()
    assert cli.main(["verify", "--data-dir", str(data)]) == 1
    assert json.loads(capsys.readouterr().out)["ok"] is False


# ── restore proof ───────────────────────────────────────────────────────────

def _libpq(url: str) -> str:
    return url.replace("postgresql+psycopg://", "postgresql://")


@pytest.mark.skipif(shutil.which("pg_dump") is None or shutil.which("pg_restore") is None,
                    reason="pg_dump/pg_restore not on PATH")
def test_restore_proof_dump_restore_into_a_fresh_database_verifies(source, pg, pgdb, tmp_path, monkeypatch):
    """Back up the migrated and backfilled database, restore it into a different,
    empty database, and prove the restored copy equals the original single-host
    source: same counts and checksums per table, secrets still decrypt with the
    same key, forced RLS and its policies came back, and the dump itself never
    contains a plaintext secret."""
    from apps.api.core.config import settings
    from apps.api.services.workspace import manager
    from apps.api.services.workspace.secrets import get_secret
    data, ws = source
    backfill.backfill(str(data))
    assert all(c.ok for c in backfill.verify(str(data)))

    dump = tmp_path / "meta.dump"
    subprocess.run(["pg_dump", "--format=custom", "--no-owner", f"--file={dump}", _libpq(pgdb.owner_url)],
                   check=True, capture_output=True)
    plain = subprocess.run(["pg_dump", "--format=plain", "--no-owner", _libpq(pgdb.owner_url)],
                           check=True, capture_output=True, text=True).stdout
    for secret in (PLAIN_WS_SECRET, PLAIN_GLOBAL_KEY, "smtp-" + "q" * 20, "sf-" + "z" * 20):
        assert secret not in plain, "plaintext secret in a database dump"

    admin = create_engine(pgdb.owner_url.rsplit("/", 1)[0] + "/postgres", isolation_level="AUTOCOMMIT")
    restored = "opengtm_m8_restore_" + pgdb.name[-6:]
    with admin.connect() as c:
        c.execute(text(f'CREATE DATABASE "{restored}"'))
    try:
        subprocess.run(["pg_restore", "--no-owner", "--role=postgres",
                        f"--dbname={_libpq(pgdb.owner_url.rsplit('/', 1)[0] + '/' + restored)}", str(dump)],
                       check=True, capture_output=True)
        app_url = pgdb.app_url.rsplit("/", 1)[0] + "/" + restored
        engine = create_engine(app_url, pool_size=3, max_overflow=0)
        pg_meta._ready.clear()
        pg_meta.use_engine(engine)
        monkeypatch.setattr(settings, "WORKSPACE_META_STORE", "postgres")

        checks = backfill.verify(str(data), stores=("meta", "settings"))
        assert all(c.ok for c in checks), [(c.table, c.missing_keys, c.changed_keys) for c in checks if not c.ok]
        assert backfill.check_decrypt()["all_decrypt"]
        assert get_secret(ws["a"].id, "HUBSPOT_API_KEY") == PLAIN_WS_SECRET
        assert manager.member_role(ws["a"].id, 3) == "editor"

        with create_engine(pgdb.owner_url.rsplit("/", 1)[0] + "/" + restored).begin() as c:
            flags = c.execute(text(
                "SELECT relname, relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = ANY(:t)"),
                {"t": list(pg_meta.PRIMARY_KEYS)}).all()
            policies = {r[0] for r in c.execute(text("SELECT tablename FROM pg_policies"))}
        assert all(f[1] and f[2] for f in flags) and len(flags) == len(pg_meta.PRIMARY_KEYS)
        assert set(pg_meta.PRIMARY_KEYS) <= policies
        with engine.begin() as c:                      # isolation survived the restore
            c.execute(text("SELECT set_config('app.workspace_id', :w, true)"), {"w": ws["a"].id})
            assert {r[0] for r in c.execute(text("SELECT workspace_id FROM workspace_settings"))} == {ws["a"].id}
        engine.dispose()
    finally:
        pg_meta.use_engine(None)
        with admin.connect() as c:
            c.execute(text("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = :n"),
                      {"n": restored})
            c.execute(text(f'DROP DATABASE IF EXISTS "{restored}"'))
        admin.dispose()
