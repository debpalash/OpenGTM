"""Outreach — Postgres RLS + cross-tenant isolation tests (PG-gated). Spec §12.2.

GATED on TEST_DATABASE_URL (an owner PG URL). SKIPs when unset so the default
SQLite suite stays green. Connects as a NON-super, NON-BYPASSRLS login role so
the policies are exercised for real.

Run:
    TEST_DATABASE_URL='postgresql+psycopg://user4@localhost:5432/<throwaway>' \
    PYTHONPATH=. uv run --group dev --with 'psycopg[binary]' \
    python -m pytest tests/test_outreach_rls.py -q
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

APP_LOGIN_ROLE = "app_rls_test"
W1 = "ws_out_alpha"
W2 = "ws_out_beta"

_TENANT_TABLES = (
    "outreach_sequences", "outreach_enrollments", "outreach_sends", "outreach_suppressions",
)


def _app_url():
    from sqlalchemy.engine import make_url
    u = make_url(TEST_DATABASE_URL)
    return str(u.set(username=APP_LOGIN_ROLE, password=None))


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
                CREATE ROLE {APP_LOGIN_ROLE} LOGIN NOSUPERUSER NOBYPASSRLS;
              END IF; END $$;"""
        ))
        c.execute(text(f"GRANT yupcha_app TO {APP_LOGIN_ROLE}"))
        c.execute(text(f"GRANT USAGE ON SCHEMA public TO {APP_LOGIN_ROLE}"))

    # Seed one sequence + enrollment + send + suppression per tenant (as owner,
    # GUC per batch so FORCE RLS WITH CHECK passes).
    with owner_engine.begin() as c:
        for t in _TENANT_TABLES:
            c.execute(text(f"DELETE FROM {t}"))
    for ws in (W1, W2):
        with owner_engine.begin() as c:
            c.execute(text("SELECT set_config('app.workspace_id', :w, true)"), {"w": ws})
            sid = str(uuid.uuid4())
            c.execute(text(
                "INSERT INTO outreach_sequences (id, workspace_id, name, steps, consent_basis) "
                "VALUES (:id, :w, 'S', '[]', 'legit')"
            ), {"id": sid, "w": ws})
            c.execute(text(
                "INSERT INTO outreach_enrollments (workspace_id, sequence_id, lead_id, to_email_snapshot) "
                "VALUES (:w, :sid, 1, :em)"
            ), {"w": ws, "sid": sid, "em": f"a@{ws}.com"})
            c.execute(text(
                "INSERT INTO outreach_sends (workspace_id, sequence_id, to_email, status, idempotency_key) "
                "VALUES (:w, :sid, :em, 'sent', :idem)"
            ), {"w": ws, "sid": sid, "em": f"a@{ws}.com", "idem": f"idem:{ws}"})
            c.execute(text(
                "INSERT INTO outreach_suppressions (workspace_id, email, reason) "
                "VALUES (:w, :em, 'manual')"
            ), {"w": ws, "em": f"sup@{ws}.com"})
    yield


@pytest.fixture(scope="module")
def app_engine(schema):
    eng = create_engine(_app_url())
    yield eng
    eng.dispose()


def _set_ws(conn, ws):
    conn.execute(text("SELECT set_config('app.workspace_id', :w, true)"), {"w": ws})


# ── I1: GUC=A sees only A across all four tenant tables ──────────────────────

@pytest.mark.parametrize("table", _TENANT_TABLES)
def test_i1_select_isolation(app_engine, table):
    with app_engine.connect() as c:
        with c.begin():
            _set_ws(c, W1)
            wss = {r[0] for r in c.execute(text(f"SELECT DISTINCT workspace_id FROM {table}"))}
            assert wss == {W1}, f"{table}: w1 saw {wss}"
        with c.begin():
            _set_ws(c, W2)
            wss = {r[0] for r in c.execute(text(f"SELECT DISTINCT workspace_id FROM {table}"))}
            assert wss == {W2}, f"{table}: w2 saw {wss}"


@pytest.mark.parametrize("table", _TENANT_TABLES)
def test_i1_fail_closed_no_guc(app_engine, table):
    with app_engine.connect() as c, c.begin():
        n = c.execute(text(f"SELECT count(*) FROM {table}")).scalar()
    assert n == 0, f"{table} leaked with no app.workspace_id (should fail closed)"


# ── I2: cross-tenant write rejected by WITH CHECK ───────────────────────────

def test_i2_cross_tenant_insert_rejected(app_engine):
    from sqlalchemy.exc import ProgrammingError, DBAPIError
    with app_engine.connect() as c, c.begin():
        _set_ws(c, W1)
        with pytest.raises((ProgrammingError, DBAPIError)):
            c.execute(text(
                "INSERT INTO outreach_sequences (id, workspace_id, name, steps, consent_basis) "
                "VALUES (:id, :w, 'x', '[]', 'l')"
            ), {"id": str(uuid.uuid4()), "w": W2})


def test_i2b_schedules_is_not_rls(owner_engine):
    with owner_engine.connect() as c, c.begin():
        rel = c.execute(text(
            "SELECT c.relrowsecurity FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace "
            "WHERE c.relname='outreach_schedules' AND n.nspname='public'"
        )).scalar()
    assert rel is False, "outreach_schedules must NOT have RLS (it's the mirror)"


def test_i2c_tenant_tables_force_rls(owner_engine):
    with owner_engine.connect() as c, c.begin():
        for t in _TENANT_TABLES:
            row = c.execute(text(
                "SELECT c.relrowsecurity, c.relforcerowsecurity FROM pg_class c "
                "JOIN pg_namespace n ON n.oid=c.relnamespace "
                "WHERE c.relname=:t AND n.nspname='public'"
            ), {"t": t}).first()
            assert row == (True, True), f"{t} must have ENABLE+FORCE RLS"


# ── I9: assert_rls_role refuses the store under a superuser role ─────────────

def test_i9_assert_rls_role_refuses_superuser(owner_engine, monkeypatch):
    """The OWNER (superuser) connection must fail-fast through the store path —
    asserted via use_pg_store()/assert_rls_role (the same gate handle_send uses)."""
    from sqlalchemy.orm import sessionmaker
    import apps.api.services.leadgen.store as ls

    monkeypatch.setattr(ls, "_role_check_result", None, raising=False)
    monkeypatch.setattr(ls, "SessionLocal", sessionmaker(bind=owner_engine), raising=False)
    from apps.api.services.leadgen.store import assert_rls_role, RlsRoleError
    with pytest.raises(RlsRoleError):
        assert_rls_role(strict=True)


# ── I6: lead-in-two-workspaces — same int id, each its own snapshot ─────────

def test_i6_lead_in_two_workspaces(app_engine, monkeypatch):
    from sqlalchemy.orm import sessionmaker
    import apps.api.core.tenancy as tenancy
    from sqlalchemy import event

    AppSession = sessionmaker(bind=app_engine, autoflush=False)

    @event.listens_for(AppSession, "after_begin")
    def _guc(session, transaction, connection):
        ws = tenancy.current_workspace_var.get()
        if ws:
            connection.exec_driver_sql("SELECT set_config('app.workspace_id', %s, true)", (ws,))

    import apps.api.services.outreach.store as store_mod
    monkeypatch.setattr(store_mod, "SessionLocal", AppSession, raising=False)
    from apps.api.services.outreach.store import PgOutreachStore

    a, b = PgOutreachStore(W1), PgOutreachStore(W2)
    # Same lead_id 99 in both, different sequences/emails.
    with tenancy.workspace_scope(W1):
        sa = a.create_sequence("A", consent_basis="l")
        a.enroll(sa["id"], 99, "in-a@x.com")
    with tenancy.workspace_scope(W2):
        sb = b.create_sequence("B", consent_basis="l")
        b.enroll(sb["id"], 99, "in-b@x.com")
    with tenancy.workspace_scope(W1):
        due_a = a.due_enrollments(sa["id"])
    with tenancy.workspace_scope(W2):
        due_b = b.due_enrollments(sb["id"])
    assert due_a[0]["to_email_snapshot"] == "in-a@x.com"
    assert due_b[0]["to_email_snapshot"] == "in-b@x.com"


# ── I7: data migration idempotency + lead_not_found + no-charge ─────────────

def test_i7_data_migration_idempotent(owner_engine, tmp_path, monkeypatch):
    import sqlite3

    # Build a legacy outreach.db.
    data_dir = tmp_path / "data"
    data_dir.mkdir()
    db_path = data_dir / "outreach.db"
    con = sqlite3.connect(db_path)
    con.executescript(
        """
        CREATE TABLE sequences (id TEXT PRIMARY KEY, name TEXT, description TEXT,
            steps TEXT, status TEXT, daily_limit INT, send_window_start INT,
            send_window_end INT, created_at REAL, updated_at REAL);
        CREATE TABLE sequence_leads (id INTEGER PRIMARY KEY, sequence_id TEXT,
            lead_id INT, current_step INT, status TEXT, next_send_at REAL,
            sent_count INT, last_sent_at REAL, error TEXT);
        CREATE TABLE send_log (id INTEGER PRIMARY KEY, sequence_id TEXT, lead_id INT,
            step_number INT, to_email TEXT, subject TEXT, status TEXT,
            message_id TEXT, sent_at REAL, opened_at REAL, replied_at REAL, error TEXT);
        """
    )
    sid = "legacy-seq-1"
    con.execute("INSERT INTO sequences (id,name,steps,status) VALUES (?,?,?,?)",
                (sid, "Legacy", '[{"step_number":0,"subject":"s","body_html":"b"}]', "draft"))
    # lead 5001 resolvable, lead 9999 NOT.
    con.execute("INSERT INTO sequence_leads (sequence_id,lead_id,status) VALUES (?,?,?)", (sid, 5001, "pending"))
    con.execute("INSERT INTO sequence_leads (sequence_id,lead_id,status) VALUES (?,?,?)", (sid, 9999, "pending"))
    con.execute("INSERT INTO send_log (sequence_id,lead_id,step_number,to_email,subject,status,sent_at) "
                "VALUES (?,?,?,?,?,?,?)", (sid, 5001, 0, "x@y.com", "s", "sent", 1700000000.0))
    con.execute("INSERT INTO send_log (sequence_id,lead_id,step_number,to_email,subject,status,sent_at) "
                "VALUES (?,?,?,?,?,?,?)", (sid, 9999, 0, "z@y.com", "s", "sent", 1700000000.0))
    con.commit()
    con.close()

    MWS = "ws_migrate"
    # Seed a resolvable lead 5001 in MWS (as owner, GUC set).
    with owner_engine.begin() as c:
        c.execute(text("DELETE FROM outreach_sends WHERE workspace_id=:w"), {"w": MWS})
        c.execute(text("DELETE FROM outreach_enrollments WHERE workspace_id=:w"), {"w": MWS})
        c.execute(text("DELETE FROM outreach_sequences WHERE workspace_id=:w"), {"w": MWS})
        c.execute(text("DELETE FROM leads WHERE workspace_id=:w"), {"w": MWS})
        c.execute(text("SELECT set_config('app.workspace_id', :w, true)"), {"w": MWS})
        c.execute(text("INSERT INTO leads (id, workspace_id, company, email) VALUES (5001, :w, 'Acme', 'lead@acme.com')"), {"w": MWS})

    # Point the migrator at our throwaway PG + legacy db + workspace. The
    # migrator binds pg_engine at import from apps.api.database (SQLite under the
    # test conftest), so override it directly with a real PG engine.
    monkeypatch.setenv("OUTREACH_MIGRATION_WS", MWS)
    monkeypatch.setattr("apps.api.core.config.settings.DATA_DIR", str(data_dir), raising=False)
    import apps.api.scripts.migrate_sqlite_to_pg as mig
    monkeypatch.setattr(mig, "pg_engine", owner_engine, raising=False)

    def _counts():
        with owner_engine.begin() as c:
            c.execute(text("SELECT set_config('app.workspace_id', :w, true)"), {"w": MWS})
            enr = c.execute(text("SELECT count(*) FROM outreach_enrollments WHERE workspace_id=:w"), {"w": MWS}).scalar()
            snd = c.execute(text("SELECT count(*) FROM outreach_sends WHERE workspace_id=:w"), {"w": MWS}).scalar()
            lnf = c.execute(text("SELECT count(*) FROM outreach_sends WHERE workspace_id=:w AND skip_reason='lead_not_found'"), {"w": MWS}).scalar()
            charged = c.execute(text("SELECT coalesce(sum(charged_usd),0) FROM outreach_sends WHERE workspace_id=:w"), {"w": MWS}).scalar()
            ledger = c.execute(text("SELECT count(*) FROM credit_ledger_entries WHERE reason='outreach_send' AND workspace_id=:w"), {"w": MWS}).scalar()
        return enr, snd, lnf, float(charged), ledger

    mig.migrate_outreach()
    e1, s1, l1, ch1, led1 = _counts()
    mig.migrate_outreach()  # re-run → idempotent
    e2, s2, l2, ch2, led2 = _counts()

    assert e1 == 1 and e2 == 1, "only the resolvable enrollment migrated, idempotently"
    assert s1 == 2 and s2 == 2, "both sends present, idempotent on re-run"
    assert l1 == 1, "unresolved lead send marked lead_not_found"
    assert ch1 == 0.0 and ch2 == 0.0, "migrated sends never charged"
    assert led1 == 0 and led2 == 0, "migration never touches the ledger"
