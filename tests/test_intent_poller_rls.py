"""Intent-poller — Postgres RLS + on_signal + concurrency + budget tests (PG-gated).

GATED on TEST_DATABASE_URL (an owner/superuser PG URL). SKIPs when unset so the
default SQLite suite stays green. Connects as a dedicated NON-super, NON-BYPASSRLS
login role so the policies are exercised for real (RLS is inert under superuser).

Covers: AC-1/AC-2 (dedup → exactly one on_signal fire, re-emit no-op),
AC-3 (no fire when unmatched), AC-4/AC-23 (RLS isolation + fail-closed GUC),
AC-9b (mirror integrity), AC-17 (single-flight unique index), AC-22 (atomic
budget + poll-now counts).

Run:
    TEST_DATABASE_URL='postgresql+psycopg://user4@localhost:5432/<throwaway>' \\
    PYTHONPATH=. uv run --group dev --with 'psycopg[binary]' \\
    python -m pytest tests/test_intent_poller_rls.py -q
"""

import os
import uuid
from datetime import date

import pytest
from sqlalchemy import create_engine, event, text
from sqlalchemy.engine import make_url
from sqlalchemy.orm import sessionmaker

TEST_DATABASE_URL = os.getenv("TEST_DATABASE_URL")

pytestmark = pytest.mark.skipif(
    not TEST_DATABASE_URL,
    reason="TEST_DATABASE_URL not set (Postgres RLS tests skipped)",
)

APP_LOGIN_ROLE = "app_rls_test"
W1 = "ws_poller_alpha"
W2 = "ws_poller_beta"


def _app_url():
    return str(make_url(TEST_DATABASE_URL).set(username=APP_LOGIN_ROLE, password=None))


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

    # Seed one watch + one budget row per tenant.
    with owner_engine.begin() as c:
        c.execute(text("DELETE FROM watch_subscriptions"))
        c.execute(text("DELETE FROM poll_budget_ledger"))
        c.execute(text("DELETE FROM watch_schedules"))
    for ws in (W1, W2):
        with owner_engine.begin() as c:
            c.execute(text("SELECT set_config('app.workspace_id', :w, true)"), {"w": ws})
            c.execute(text(
                "INSERT INTO watch_subscriptions (id, workspace_id, kind, target, "
                "enabled, interval, cursor) VALUES "
                "(:id, :w, 'feed', 'https://acme.com/rss', true, 'daily', "
                """'{"bootstrapped": false}')"""
            ), {"id": str(uuid.uuid4()), "w": ws})
            c.execute(text(
                "INSERT INTO poll_budget_ledger (workspace_id, day, n, n_poll_now) "
                "VALUES (:w, :d, 0, 0)"
            ), {"w": ws, "d": date.today()})
    yield


@pytest.fixture(scope="module")
def app_engine(schema):
    eng = create_engine(_app_url())
    yield eng
    eng.dispose()


def _set_ws(conn, ws):
    conn.execute(text("SELECT set_config('app.workspace_id', :w, true)"), {"w": ws})


_TENANT_TABLES = ("watch_subscriptions", "poll_budget_ledger")


# ── AC-4: SELECT isolation per tenant ────────────────────────────────────────

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


# ── AC-23: fail-closed on unset GUC ──────────────────────────────────────────

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
                "INSERT INTO watch_subscriptions (id, workspace_id, kind, target, enabled) "
                "VALUES (:id, :w, 'feed', 'x', true)"
            ), {"id": str(uuid.uuid4()), "w": W2})  # foreign ws → WITH CHECK blocks


def test_watch_schedules_is_not_rls(owner_engine):
    """watch_schedules is the deliberate non-RLS mirror — readable w/o a GUC."""
    with owner_engine.connect() as c, c.begin():
        rel = c.execute(text(
            "SELECT c.relrowsecurity FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace "
            "WHERE c.relname='watch_schedules' AND n.nspname='public'"
        )).scalar()
    assert rel is False, "watch_schedules must NOT have RLS enabled (it's the mirror)"


def test_rls_tables_have_force_rls(owner_engine):
    with owner_engine.connect() as c, c.begin():
        for t in _TENANT_TABLES:
            row = c.execute(text(
                "SELECT c.relrowsecurity, c.relforcerowsecurity FROM pg_class c "
                "JOIN pg_namespace n ON n.oid=c.relnamespace "
                "WHERE c.relname=:t AND n.nspname='public'"
            ), {"t": t}).first()
            assert row == (True, True), f"{t} must have ENABLE+FORCE RLS"


# ── AC-22: atomic budget upsert + poll-now counts ────────────────────────────

def test_budget_upsert_atomic_and_poll_now_counts(app_engine, owner_engine):
    """ON CONFLICT DO UPDATE SET n=n+1 RETURNING n increments once per call;
    poll-now increments BOTH n and n_poll_now (not exempt, spec §7/AC-22)."""
    ws = "ws_budget_x"
    with owner_engine.begin() as c:
        c.execute(text("DELETE FROM poll_budget_ledger WHERE workspace_id=:w"), {"w": ws})
    AppSession = _bind_app_session(app_engine)
    import apps.api.services.poller.engine as eng

    import apps.api.database as database
    _orig = database.SessionLocal
    eng.SessionLocal = AppSession
    try:
        from apps.api.core.tenancy import workspace_scope
        with workspace_scope(ws):
            with AppSession() as db, db.begin():
                n1, pn1 = eng._debit_budget(db, ws, poll_now=False)
                n2, pn2 = eng._debit_budget(db, ws, poll_now=False)
                n3, pn3 = eng._debit_budget(db, ws, poll_now=True)
            assert (n1, n2, n3) == (1, 2, 3)
            assert (pn1, pn2, pn3) == (0, 0, 1)  # only poll-now bumps n_poll_now
    finally:
        eng.SessionLocal = _orig


# ── AC-17: single-flight unique index (no double-enqueue) ────────────────────

def test_single_flight_unique_index(app_engine, owner_engine):
    """Two enqueues with the SAME fire_key collapse to one active job; the second
    is caught by the partial unique index (IntegrityError) → already-enqueued."""
    # jobs is NOT an RLS table → use the owner engine session for this test.
    OwnerSession = sessionmaker(bind=owner_engine, autoflush=False)
    import apps.api.services.poller.engine as eng

    fire_key = f"watch:{uuid.uuid4()}:once"
    with owner_engine.begin() as c:
        c.execute(text("DELETE FROM jobs WHERE fire_key=:k"), {"k": fire_key})

    from datetime import datetime, timezone
    now = datetime.now(timezone.utc)
    with OwnerSession() as db, db.begin():
        ok1 = eng._enqueue_poll_if_absent(
            db, fire_key=fire_key, workspace_id=W1, watch_id="w", next_run_at=now)
    with OwnerSession() as db, db.begin():
        ok2 = eng._enqueue_poll_if_absent(
            db, fire_key=fire_key, workspace_id=W1, watch_id="w", next_run_at=now)
    assert ok1 is True and ok2 is False
    with owner_engine.begin() as c:
        n = c.execute(text(
            "SELECT count(*) FROM jobs WHERE fire_key=:k AND status IN ('pending','processing')"
        ), {"k": fire_key}).scalar()
    assert n == 1, f"expected exactly one active job, got {n}"


# ── AC-1/AC-2/AC-3: on_signal fires once via PgLeadStore.add_signal ──────────

def test_on_signal_fires_once_and_dedup_noop(app_engine, owner_engine, monkeypatch):
    """A poller signal for a scoped lead enqueues exactly one trigger_eval with
    fire_key=signal:<pk>; re-inserting the same deterministic id does NOT re-fire
    (AC-1/AC-2). A signal whose lead is in no scoped workbook fires zero (AC-3)."""
    from apps.api.core.config import settings as app_settings
    import apps.api.core.tenancy as tenancy
    monkeypatch.setattr(app_settings, "AUTOMATIONS_ENABLED", True, raising=False)

    AppSession = _bind_app_session(app_engine)
    import apps.api.services.leadgen.store as store_mod
    import apps.api.services.signals.store as sig_store_mod
    monkeypatch.setattr(store_mod, "SessionLocal", AppSession)
    # PgLeadStore.add_signal now delegates to the shared SignalStore — bind its
    # SessionLocal to the app-role engine too so the write+emit run under RLS.
    monkeypatch.setattr(sig_store_mod, "SessionLocal", AppSession)

    from apps.api.services.queue_service import QueueService
    enqueued = []

    def fake_add_job(self, db, jtype, payload, priority=1):
        enqueued.append((jtype, payload))

        class _J:
            id = len(enqueued)
        return _J()

    monkeypatch.setattr(QueueService, "add_job", fake_add_job)

    ws = W1
    # Seed an on_signal rule + workbook + row for lead 4242 (matched) under app.
    with app_engine.begin() as c:
        _set_ws(c, ws)
        c.execute(text("DELETE FROM signals WHERE workspace_id=:w"), {"w": ws})
        c.execute(text("DELETE FROM triggers WHERE workspace_id=:w AND trigger_type='on_signal'"), {"w": ws})
        c.execute(text(
            "INSERT INTO triggers (id, workspace_id, name, enabled, trigger_type, "
            "trigger_config, actions, scope_workbook_ids) VALUES "
            "(:id, :w, 'sig', true, 'on_signal', "
            """'{"signal_types": ["company_funded"]}', '[]', '[]')"""
        ), {"id": str(uuid.uuid4()), "w": ws})
    # workbook + row are RLS-protected (workbooks RLS migration e5f6a7b8c9d0) →
    # seed via the owner engine (superuser bypasses RLS).
    wb_id = str(uuid.uuid4())
    with owner_engine.begin() as c:
        c.execute(text("DELETE FROM workbook_rows WHERE lead_id IN (4242, 9999)"))
        c.execute(text("DELETE FROM workbooks WHERE workspace_id=:w AND name='wb'"), {"w": ws})
        c.execute(text(
            "INSERT INTO workbooks (id, workspace_id, name, columns_config) "
            "VALUES (:id, :w, 'wb', '[]')"
        ), {"id": wb_id, "w": ws})
        c.execute(text(
            "INSERT INTO workbook_rows (workbook_id, workspace_id, lead_id, data, enrichments) "
            "VALUES (:wb, :w, 4242, '{}', '{}')"
        ), {"wb": wb_id, "w": ws})

    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.leadgen.store import PgLeadStore
    from apps.api.services.signals.monitor import Signal
    from apps.api.services.poller import keys

    sig_id = keys.signal_event_id(ws, "funding", "0001", "company_funded", "0001:accZ")
    sig = Signal(id=sig_id, workspace_id=ws, lead_id=4242, company="Acme",
                 signal_type="company_funded", title="t", created_at=1.0)

    with workspace_scope(ws):
        store = PgLeadStore(ws)
        store.add_signal(sig)         # first → fires
        store.add_signal(sig)         # same deterministic id → no second fire

    fires = [p for (t, p) in enqueued if t == "trigger_eval"
             and p.get("fire_key") == f"signal:{sig_id}"]
    assert len(fires) == 1, f"expected exactly one on_signal fire, got {len(fires)}"

    # AC-3: a signal for an UNMATCHED lead (no workbook row) fires zero.
    enqueued.clear()
    sig2_id = keys.signal_event_id(ws, "funding", "0001", "company_funded", "0001:accY")
    sig2 = Signal(id=sig2_id, workspace_id=ws, lead_id=9999, company="Ghost",
                  signal_type="company_funded", title="t", created_at=1.0)
    with workspace_scope(ws):
        PgLeadStore(ws).add_signal(sig2)
    assert [p for (t, p) in enqueued if t == "trigger_eval"] == []


# ── AC-4 (write side): add_signal stamps THIS workspace verbatim ─────────────

def test_add_signal_stamps_workspace_and_isolated(app_engine, monkeypatch):
    AppSession = _bind_app_session(app_engine)
    import apps.api.services.leadgen.store as store_mod
    import apps.api.services.signals.store as sig_store_mod
    monkeypatch.setattr(store_mod, "SessionLocal", AppSession)
    # add_signal delegates to the shared SignalStore → bind its SessionLocal too.
    monkeypatch.setattr(sig_store_mod, "SessionLocal", AppSession)
    from apps.api.core.config import settings as app_settings
    monkeypatch.setattr(app_settings, "AUTOMATIONS_ENABLED", False, raising=False)

    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.leadgen.store import PgLeadStore
    from apps.api.services.signals.monitor import Signal
    from apps.api.services.poller import keys

    sid = keys.signal_event_id(W1, "funding", "0009", "company_funded", "0009:accW")
    # dataclass carries W2 but the store must force-stamp W1 (its own workspace).
    sig = Signal(id=sid, workspace_id=W2, lead_id=7, company="X",
                 signal_type="company_funded", title="t", created_at=2.0)
    with workspace_scope(W1):
        PgLeadStore(W1).add_signal(sig)
    # read under W1 → present; under W2 → absent.
    with workspace_scope(W1):
        got1 = PgLeadStore(W1).get_signals(signal_type="company_funded", lead_id=7, limit=5)
    with workspace_scope(W2):
        got2 = PgLeadStore(W2).get_signals(signal_type="company_funded", lead_id=7, limit=5)
    assert any(s["id"] == sid and s["workspace_id"] == W1 for s in got1)
    assert all(s["id"] != sid for s in got2)


# ── helpers ──────────────────────────────────────────────────────────────────

def _bind_app_session(app_engine):
    """A sessionmaker on the app-role engine whose after_begin sets the RLS GUC
    from the contextvar (mirrors database.py)."""
    import apps.api.core.tenancy as tenancy

    AppSession = sessionmaker(bind=app_engine, autoflush=False, autocommit=False)

    @event.listens_for(AppSession, "after_begin")
    def _guc(session, transaction, connection):  # noqa: ANN001
        ws = tenancy.current_workspace_var.get()
        if ws:
            connection.exec_driver_sql(
                "SELECT set_config('app.workspace_id', %s, true)", (ws,)
            )

    return AppSession
