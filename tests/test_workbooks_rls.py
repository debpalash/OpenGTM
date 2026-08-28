"""Workbooks — Postgres RLS + cross-tenant isolation tests (PG-gated). Spec §9.

GATED on TEST_DATABASE_URL (an owner PG URL). SKIPs when unset so the default
SQLite suite stays green. The owner role (user4) is a superuser and therefore
BYPASSES RLS — it is used only to run migrations + seed data. All isolation
assertions go through ``app_engine``, a NON-super, NON-BYPASSRLS login role
(``app_rls_test``) granted ``yupcha_app``, so the policies are exercised for real.

Proves acceptance criteria 1-4 + 12 of
docs/specs/followup-workbooks-rls-hardening-spec.md for every tenant-owned
workbook table, including durable connector runs.

Run:
    TEST_DATABASE_URL='postgresql+psycopg://user4@localhost:5432/<throwaway>' \
    PYTHONPATH=. uv run --group dev --with 'psycopg[binary]' \
    python -m pytest tests/test_workbooks_rls.py -q
"""

import os
import uuid

import pytest
from sqlalchemy import create_engine, text

TEST_DATABASE_URL = os.getenv("TEST_DATABASE_URL")

pytestmark = pytest.mark.skipif(
    not TEST_DATABASE_URL,
    reason="TEST_DATABASE_URL not set (Postgres RLS tests skipped)",
)

APP_LOGIN_ROLE = "app_rls_workbooks_test"
APP_LOGIN_PASSWORD = "workbooks_rls_test_only"
W1 = "ws_wb_alpha"
W2 = "ws_wb_beta"

# All RLS-protected workbook tables.
_TENANT_TABLES = (
    "workbooks", "workbook_rows", "workbook_enrichments",
    "workbook_activity", "cell_traces", "connector_runs",
)


def _app_url():
    from sqlalchemy.engine import make_url
    u = make_url(TEST_DATABASE_URL)
    # ``str(URL)`` intentionally redacts passwords to ``***``; using it as a
    # connection URL would therefore authenticate with the literal redaction.
    return u.set(
        username=APP_LOGIN_ROLE, password=APP_LOGIN_PASSWORD
    ).render_as_string(hide_password=False)


@pytest.fixture(scope="module")
def owner_engine():
    eng = create_engine(TEST_DATABASE_URL)
    yield eng
    eng.dispose()


@pytest.fixture(scope="module")
def schema(owner_engine):
    import subprocess
    repo_root = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
    env = dict(os.environ, DATABASE_URL=TEST_DATABASE_URL)
    res = subprocess.run(
        ["uv", "run", "--with", "psycopg[binary]", "alembic", "upgrade", "head"],
        cwd=repo_root, env=env, capture_output=True, text=True,
    )
    assert res.returncode == 0, f"alembic upgrade failed:\n{res.stdout}\n{res.stderr}"

    with owner_engine.begin() as c:
        c.execute(text(
            f"""DO $$ BEGIN
              IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='{APP_LOGIN_ROLE}') THEN
                CREATE ROLE {APP_LOGIN_ROLE} LOGIN NOSUPERUSER NOBYPASSRLS
                  PASSWORD '{APP_LOGIN_PASSWORD}';
              END IF; END $$;"""
        ))
        c.execute(text(
            f"ALTER ROLE {APP_LOGIN_ROLE} PASSWORD '{APP_LOGIN_PASSWORD}'"
        ))
        c.execute(text(f"GRANT yupcha_app TO {APP_LOGIN_ROLE}"))
        c.execute(text(f"GRANT USAGE ON SCHEMA public TO {APP_LOGIN_ROLE}"))

    # Clean + seed one workbook (+ one of each child row) per tenant. Owner is a
    # superuser → bypasses RLS, so deletes/inserts are unconstrained; the GUC is
    # set per batch only for clarity.
    with owner_engine.begin() as c:
        for t in ("workbook_rows", "workbook_enrichments", "workbook_activity",
                  "cell_traces", "connector_runs", "workbooks"):
            c.execute(text(f"DELETE FROM {t} WHERE workspace_id IN (:a, :b)"),
                      {"a": W1, "b": W2})

    wb_ids = {}
    for ws in (W1, W2):
        wid = str(uuid.uuid4())
        wb_ids[ws] = wid
        with owner_engine.begin() as c:
            c.execute(text("SELECT set_config('app.workspace_id', :w, true)"), {"w": ws})
            c.execute(text(
                "INSERT INTO workbooks (id, workspace_id, name) VALUES (:id, :w, 'WB')"
            ), {"id": wid, "w": ws})
            c.execute(text(
                "INSERT INTO workbook_rows (workspace_id, workbook_id, position, data) "
                "VALUES (:w, :wid, 0, '{}')"
            ), {"w": ws, "wid": wid})
            c.execute(text(
                "INSERT INTO workbook_enrichments "
                "(workspace_id, workbook_id, lead_id, column_id, status) "
                "VALUES (:w, :wid, 1, 'col_a', 'complete')"
            ), {"w": ws, "wid": wid})
            c.execute(text(
                "INSERT INTO workbook_activity (workspace_id, workbook_id, kind, message) "
                "VALUES (:w, :wid, 'info', 'seed')"
            ), {"w": ws, "wid": wid})
            c.execute(text(
                "INSERT INTO cell_traces "
                "(workspace_id, workbook_id, lead_id, column_id, goal, outcome) "
                "VALUES (:w, :wid, 1, 'col_a', 'g', 'found')"
            ), {"w": ws, "wid": wid})
            c.execute(text(
                "INSERT INTO connector_runs "
                "(id, workspace_id, workbook_id, connector, status, requested_count, "
                "fetched_count, added_count, updated_count, skipped_count, pages_fetched, "
                "target_met, exhausted) "
                "VALUES (:id, :w, :wid, 'test', 'complete', 1, 1, 1, 0, 0, 1, true, false)"
            ), {"id": str(uuid.uuid4()), "w": ws, "wid": wid})
    yield wb_ids


@pytest.fixture(scope="module")
def app_engine(schema):
    eng = create_engine(_app_url())
    yield eng
    eng.dispose()


def _set_ws(conn, ws):
    conn.execute(text("SELECT set_config('app.workspace_id', :w, true)"), {"w": ws})


# ── select isolation: GUC=A sees only A across all tables ───────────────────

@pytest.mark.parametrize("table", _TENANT_TABLES)
def test_select_isolation(app_engine, table):
    with app_engine.connect() as c:
        with c.begin():
            _set_ws(c, W1)
            wss = {r[0] for r in c.execute(text(f"SELECT DISTINCT workspace_id FROM {table}"))}
            assert wss == {W1}, f"{table}: W1 saw {wss}"
        with c.begin():
            _set_ws(c, W2)
            wss = {r[0] for r in c.execute(text(f"SELECT DISTINCT workspace_id FROM {table}"))}
            assert wss == {W2}, f"{table}: W2 saw {wss}"


@pytest.mark.parametrize("table", _TENANT_TABLES)
def test_fail_closed_no_guc(app_engine, table):
    with app_engine.connect() as c, c.begin():
        n = c.execute(text(f"SELECT count(*) FROM {table}")).scalar()
    assert n == 0, f"{table} leaked with no app.workspace_id (must fail closed)"


# ── cross-tenant write rejected by WITH CHECK ───────────────────────────────

def test_cross_tenant_insert_rejected_workbooks(app_engine):
    from sqlalchemy.exc import ProgrammingError, DBAPIError
    with app_engine.connect() as c, c.begin():
        _set_ws(c, W1)
        with pytest.raises((ProgrammingError, DBAPIError)):
            c.execute(text(
                "INSERT INTO workbooks (id, workspace_id, name) VALUES (:id, :w, 'x')"
            ), {"id": str(uuid.uuid4()), "w": W2})


def test_cross_tenant_insert_rejected_child(app_engine, schema):
    """A child row stamped with another tenant's workspace_id is rejected even
    when its workbook_id belongs to the active tenant (WITH CHECK is column-based)."""
    from sqlalchemy.exc import ProgrammingError, DBAPIError
    wid_w1 = schema[W1]
    with app_engine.connect() as c, c.begin():
        _set_ws(c, W1)
        with pytest.raises((ProgrammingError, DBAPIError)):
            c.execute(text(
                "INSERT INTO workbook_rows (workspace_id, workbook_id, position, data) "
                "VALUES (:w, :wid, 9, '{}')"
            ), {"w": W2, "wid": wid_w1})


def test_cross_tenant_update_flip_rejected(app_engine):
    """UPDATE that flips a row to another tenant is rejected by WITH CHECK."""
    from sqlalchemy.exc import ProgrammingError, DBAPIError
    with app_engine.connect() as c, c.begin():
        _set_ws(c, W1)
        with pytest.raises((ProgrammingError, DBAPIError)):
            c.execute(text(
                "UPDATE workbook_rows SET workspace_id = :other WHERE workspace_id = :w"
            ), {"other": W2, "w": W1})


def test_cross_tenant_delete_is_noop(app_engine, schema):
    """Under W1, a DELETE targeting W2's workbook id affects zero rows (W2's rows
    are invisible) — never a cross-tenant delete."""
    wid_w2 = schema[W2]
    with app_engine.connect() as c, c.begin():
        _set_ws(c, W1)
        res = c.execute(text("DELETE FROM workbook_rows WHERE workbook_id = :wid"),
                        {"wid": wid_w2})
        assert res.rowcount == 0
    # W2's row is still there (verified as W2).
    with app_engine.connect() as c, c.begin():
        _set_ws(c, W2)
        n = c.execute(text("SELECT count(*) FROM workbook_rows WHERE workbook_id = :wid"),
                      {"wid": wid_w2}).scalar()
    assert n == 1


# ── schema posture: FORCE flags + grants on all tables ──────────────────────

def test_tenant_tables_force_rls(owner_engine):
    with owner_engine.connect() as c, c.begin():
        for t in _TENANT_TABLES:
            row = c.execute(text(
                "SELECT c.relrowsecurity, c.relforcerowsecurity FROM pg_class c "
                "JOIN pg_namespace n ON n.oid=c.relnamespace "
                "WHERE c.relname=:t AND n.nspname='public'"
            ), {"t": t}).first()
            assert row == (True, True), f"{t} must have ENABLE+FORCE RLS"


def test_app_role_has_dml_grants(owner_engine):
    with owner_engine.connect() as c, c.begin():
        for t in _TENANT_TABLES:
            for priv in ("SELECT", "INSERT", "UPDATE", "DELETE"):
                ok = c.execute(text(
                    "SELECT has_table_privilege('yupcha_app', :t, :p)"
                ), {"t": t, "p": priv}).scalar()
                assert ok is True, f"yupcha_app missing {priv} on {t}"


def test_child_workspace_id_not_null(owner_engine):
    with owner_engine.connect() as c, c.begin():
        for t in _TENANT_TABLES:
            nullable = c.execute(text(
                "SELECT is_nullable FROM information_schema.columns "
                "WHERE table_name=:t AND column_name='workspace_id'"
            ), {"t": t}).scalar()
            assert nullable == "NO", f"{t}.workspace_id must be NOT NULL"
