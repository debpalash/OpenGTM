"""Automations — Postgres RLS + cross-tenant isolation tests (PG-gated). AC-13.

GATED on TEST_DATABASE_URL (an owner/superuser PG URL). SKIPs when unset so the
default SQLite suite stays green. Connects as a dedicated NON-super, NON-BYPASSRLS
login role so the policies are exercised for real (RLS is inert under superuser).

Run:
    TEST_DATABASE_URL='postgresql+psycopg://user4@localhost:5432/<throwaway>' \
    PYTHONPATH=. uv run --group dev --with 'psycopg[binary]' \
    python -m pytest tests/test_automations_rls.py -q
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
W1 = "ws_auto_alpha"
W2 = "ws_auto_beta"


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
            f"""
            DO $$ BEGIN
              IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='{APP_LOGIN_ROLE}') THEN
                CREATE ROLE {APP_LOGIN_ROLE} LOGIN NOSUPERUSER NOBYPASSRLS;
              END IF;
            END $$;
            """
        ))
        c.execute(text(f"GRANT yupcha_app TO {APP_LOGIN_ROLE}"))
        c.execute(text(f"GRANT USAGE ON SCHEMA public TO {APP_LOGIN_ROLE}"))

    # Seed: one trigger + one run + one action_result + one reservation per tenant.
    with owner_engine.begin() as c:
        c.execute(text("DELETE FROM trigger_action_results"))
        c.execute(text("DELETE FROM trigger_cap_reservations"))
        c.execute(text("DELETE FROM trigger_runs"))
        c.execute(text("DELETE FROM triggers"))
    for ws in (W1, W2):
        with owner_engine.begin() as c:
            c.execute(text("SELECT set_config('app.workspace_id', :w, true)"), {"w": ws})
            tid = str(uuid.uuid4())
            rid = str(uuid.uuid4())
            c.execute(text(
                "INSERT INTO triggers (id, workspace_id, name, enabled, trigger_type, "
                "trigger_config, actions) VALUES (:id, :w, 'r', true, 'on_row_added', "
                "'{}', '[]')"
            ), {"id": tid, "w": ws})
            c.execute(text(
                "INSERT INTO trigger_runs (id, workspace_id, trigger_id, status) "
                "VALUES (:id, :w, :tid, 'completed')"
            ), {"id": rid, "w": ws, "tid": tid})
            c.execute(text(
                "INSERT INTO trigger_action_results (workspace_id, run_id, trigger_id, "
                "workbook_id, row_id, action_index, action_type, idempotency_key, status) "
                "VALUES (:w, :rid, :tid, 'wb', '1', 0, 'webhook', :idem, 'success')"
            ), {"w": ws, "rid": rid, "tid": tid, "idem": f"idem:{ws}"})
            c.execute(text(
                "INSERT INTO trigger_cap_reservations (workspace_id, trigger_id, day_utc, "
                "reserved_usd, reserved_actions, idempotency_key, state) "
                "VALUES (:w, :tid, '2026-06-25', 0.1, 1, :idem, 'settled')"
            ), {"w": ws, "tid": tid, "idem": f"cap:{ws}"})
    yield


@pytest.fixture(scope="module")
def app_engine(schema):
    eng = create_engine(_app_url())
    yield eng
    eng.dispose()


def _set_ws(conn, ws):
    conn.execute(text("SELECT set_config('app.workspace_id', :w, true)"), {"w": ws})


_TENANT_TABLES = ("triggers", "trigger_runs", "trigger_action_results", "trigger_cap_reservations")


@pytest.mark.parametrize("table", _TENANT_TABLES)
def test_select_isolation(app_engine, table):
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
def test_fail_closed_no_guc(app_engine, table):
    with app_engine.connect() as c, c.begin():
        n = c.execute(text(f"SELECT count(*) FROM {table}")).scalar()
    assert n == 0, f"{table} leaked with no app.workspace_id (should fail closed)"


def test_cross_tenant_insert_rejected(app_engine):
    from sqlalchemy.exc import ProgrammingError, DBAPIError
    with app_engine.connect() as c, c.begin():
        _set_ws(c, W1)
        with pytest.raises((ProgrammingError, DBAPIError)):
            c.execute(text(
                "INSERT INTO triggers (id, workspace_id, name, enabled, trigger_type, "
                "trigger_config, actions) VALUES (:id, :w, 'x', true, 'on_row_added', '{}', '[]')"
            ), {"id": str(uuid.uuid4()), "w": W2})  # foreign ws → WITH CHECK blocks


def test_scheduled_triggers_is_not_rls(owner_engine):
    """scheduled_triggers is the deliberate non-RLS mirror — readable w/o a GUC."""
    with owner_engine.connect() as c, c.begin():
        rel = c.execute(text(
            "SELECT c.relrowsecurity FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace "
            "WHERE c.relname='scheduled_triggers' AND n.nspname='public'"
        )).scalar()
    assert rel is False, "scheduled_triggers must NOT have RLS enabled (it's the mirror)"


def test_rls_tables_have_force_rls(owner_engine):
    with owner_engine.connect() as c, c.begin():
        for t in _TENANT_TABLES:
            row = c.execute(text(
                "SELECT c.relrowsecurity, c.relforcerowsecurity FROM pg_class c "
                "JOIN pg_namespace n ON n.oid=c.relnamespace "
                "WHERE c.relname=:t AND n.nspname='public'"
            ), {"t": t}).first()
            assert row == (True, True), f"{t} must have ENABLE+FORCE RLS"


# ── AC-4/AC-5: real on_signal path via PgLeadStore.add_signal ───────────────

def test_on_signal_real_path_enqueues_once(app_engine, owner_engine, monkeypatch):
    """Calling the REAL PgLeadStore.add_signal fires emit_signal_matches which
    enqueues exactly one trigger_eval; re-inserting the same signal (same PK) does
    NOT re-fire. Also proves the legacy global scan is NOT an event source."""
    from sqlalchemy.orm import sessionmaker
    from sqlalchemy import event
    import apps.api.core.tenancy as tenancy
    from apps.api.core.config import settings as app_settings

    monkeypatch.setattr(app_settings, "AUTOMATIONS_ENABLED", True, raising=False)

    AppSession = sessionmaker(bind=app_engine, autoflush=False, autocommit=False)

    @event.listens_for(AppSession, "after_begin")
    def _guc(session, transaction, connection):
        ws = tenancy.current_workspace_var.get()
        if ws:
            connection.exec_driver_sql(
                "SELECT set_config('app.workspace_id', %s, true)", (ws,)
            )

    # Point the leadgen store + events at the app-role session.
    import apps.api.services.leadgen.store as store_mod
    import apps.api.services.signals.store as sig_store_mod
    monkeypatch.setattr(store_mod, "SessionLocal", AppSession)
    # PgLeadStore.add_signal now delegates to the shared SignalStore — bind its
    # SessionLocal to the app-role engine too so write+emit run under RLS.
    monkeypatch.setattr(sig_store_mod, "SessionLocal", AppSession)

    # Capture enqueued jobs instead of writing to the (RLS-less) jobs table.
    from apps.api.services.queue_service import QueueService
    enqueued = []

    def fake_add_job(self, db, jtype, payload, priority=1):
        enqueued.append((jtype, payload))

        class _J:
            id = len(enqueued)
        return _J()

    monkeypatch.setattr(QueueService, "add_job", fake_add_job)

    # Seed an on_signal rule + workbook + row (lead_id=4242) for W1.
    with app_engine.begin() as c:
        _set_ws(c, W1)
        tid = str(uuid.uuid4())
        c.execute(text(
            "INSERT INTO triggers (id, workspace_id, name, enabled, trigger_type, "
            "trigger_config, actions, scope_workbook_ids) VALUES "
            "(:id, :w, 'sigrule', true, 'on_signal', "
            """'{"signal_types": ["hiring"]}', '[]', '[]')"""
        ), {"id": tid, "w": W1})

    # workbooks/workbook_rows are app-scoped (non-RLS). Seed them via the OWNER
    # engine (the app role only has SELECT on them, by design); the emitter then
    # reads them under the app role to map the signal's lead → its row.
    wb_id = str(uuid.uuid4())
    with owner_engine.begin() as c:
        c.execute(text(
            "INSERT INTO workbooks (id, name, workspace_id, source_type, columns_config) "
            "VALUES (:id, 'sigwb', :w, 'empty', '[]')"
        ), {"id": wb_id, "w": W1})
        c.execute(text(
            "INSERT INTO workbook_rows (workbook_id, position, data, lead_id) "
            """VALUES (:wb, 0, '{"company": "Acme"}', 4242)"""
        ), {"wb": wb_id})

    from apps.api.services.leadgen.store import PgLeadStore
    from apps.api.services.signals.monitor import Signal

    sig = Signal(id=str(uuid.uuid4()), workspace_id=W1, lead_id=4242,
                 company="Acme", signal_type="hiring", title="hiring")

    with tenancy.workspace_scope(W1):
        store = PgLeadStore(W1)
        store.add_signal(sig)
        store.add_signal(sig)  # re-insert same PK → no new fire

    trig_jobs = [p for (jt, p) in enqueued if jt == "trigger_eval"]
    assert len(trig_jobs) == 1, f"expected exactly one trigger_eval, got {len(trig_jobs)}"
    assert trig_jobs[0]["fire_key"] == f"signal:{sig.id}"
    assert trig_jobs[0]["targets"], "signal target row not resolved"
