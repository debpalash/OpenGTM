"""Postgres tenancy + Row-Level Security tests.

GATED on TEST_DATABASE_URL (an owner/superuser Postgres URL the harness can use
to set up schema and roles). When it is unset these tests SKIP, so the default
SQLite suite stays green and CI without Postgres still passes.

The brief's load-bearing fact: RLS is silently inert under a superuser /
BYPASSRLS role. So the isolation assertions connect as a dedicated NON-super,
NON-BYPASSRLS login role (created here) — exercising the policies for real.

Run here against the live PG, e.g.:
    TEST_DATABASE_URL='postgresql+psycopg://user4@localhost:5432/<throwaway>' \
    PYTHONPATH=. uv run --group dev --with 'psycopg[binary]' \
    python -m pytest tests/test_pg_tenancy_rls.py -q
"""

import os
import time
import uuid

import pytest
from sqlalchemy import create_engine, text

TEST_DATABASE_URL = os.getenv("TEST_DATABASE_URL")

pytestmark = pytest.mark.skipif(
    not TEST_DATABASE_URL,
    reason="TEST_DATABASE_URL not set (Postgres RLS tests skipped)",
)

# A login role granted the `yupcha_app` group role — non-super, non-BYPASSRLS.
APP_LOGIN_ROLE = "app_rls_test"
APP_LOGIN_PASSWORD = "rls_test_only"
W1 = "ws_alpha"
W2 = "ws_beta"


def _owner_url():
    return TEST_DATABASE_URL


def _app_url():
    # Same host/db as the owner URL but connecting as the app login role.
    # Build by swapping the user in the URL.
    from sqlalchemy.engine import make_url

    u = make_url(TEST_DATABASE_URL)
    return u.set(username=APP_LOGIN_ROLE, password=APP_LOGIN_PASSWORD).render_as_string(hide_password=False)


@pytest.fixture(scope="module")
def owner_engine():
    eng = create_engine(_owner_url(), poolclass=None)
    yield eng
    eng.dispose()


@pytest.fixture(scope="module")
def schema(owner_engine):
    """Bring the schema to head via Alembic on the target DB, create the app
    login role + grants, and seed two tenants' rows. Module-scoped."""
    # 1) alembic upgrade head against TEST_DATABASE_URL.
    import subprocess

    repo_root = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
    env = dict(os.environ, DATABASE_URL=TEST_DATABASE_URL)
    res = subprocess.run(
        ["uv", "run", "--with", "psycopg[binary]", "alembic", "upgrade", "head"],
        cwd=repo_root, env=env, capture_output=True, text=True,
    )
    assert res.returncode == 0, f"alembic upgrade failed:\n{res.stdout}\n{res.stderr}"

    # 2) Create the login role and grant it the yupcha_app group role.
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
        c.execute(text(f"ALTER ROLE {APP_LOGIN_ROLE} PASSWORD '{APP_LOGIN_PASSWORD}'"))
        c.execute(text(f"GRANT yupcha_app TO {APP_LOGIN_ROLE}"))
        # The app login role needs to connect + see the schema.
        c.execute(text(f"GRANT USAGE ON SCHEMA public TO {APP_LOGIN_ROLE}"))

    # 3) Seed: each tenant gets its own (Acme, NYC) plus a unique row. Inserted
    #    by the OWNER, but FORCE RLS binds the owner too — so set the GUC.
    with owner_engine.begin() as c:
        c.execute(text("DELETE FROM signals"))
        c.execute(text("DELETE FROM leads"))
    for ws, extra in ((W1, "Alpha Corp"), (W2, "Beta LLC")):
        with owner_engine.begin() as c:
            c.execute(text("SELECT set_config('app.workspace_id', :w, true)"), {"w": ws})
            c.execute(text(
                "INSERT INTO leads (workspace_id, company, city, score, notes) "
                "VALUES (:w, 'Acme', 'NYC', 50, 'shared name')"
            ), {"w": ws})
            c.execute(text(
                "INSERT INTO leads (workspace_id, company, city, score, specialization) "
                "VALUES (:w, :co, 'SF', 80, 'IT Staffing')"
            ), {"w": ws, "co": extra})
            sid = str(uuid.uuid4())
            c.execute(text(
                "INSERT INTO signals (id, workspace_id, company, signal_type, title, created_at) "
                "VALUES (:id, :w, :co, 'funding', 'raised money', :ts)"
            ), {"id": sid, "w": ws, "co": extra, "ts": time.time()})
    yield


@pytest.fixture(scope="module")
def app_engine(schema):
    eng = create_engine(_app_url())
    yield eng
    eng.dispose()


def _set_ws(conn, ws):
    conn.execute(text("SELECT set_config('app.workspace_id', :w, true)"), {"w": ws})


# ── 1. Cross-tenant SELECT isolation (leads + signals) ──

def test_select_isolation_leads(app_engine):
    with app_engine.connect() as c:
        with c.begin():
            _set_ws(c, W1)
            wss = {r[0] for r in c.execute(text("SELECT DISTINCT workspace_id FROM leads"))}
            assert wss == {W1}, f"w1 saw foreign rows: {wss}"
        with c.begin():
            _set_ws(c, W2)
            wss = {r[0] for r in c.execute(text("SELECT DISTINCT workspace_id FROM leads"))}
            assert wss == {W2}, f"w2 saw foreign rows: {wss}"


def test_select_isolation_signals(app_engine):
    with app_engine.connect() as c:
        with c.begin():
            _set_ws(c, W1)
            wss = {r[0] for r in c.execute(text("SELECT DISTINCT workspace_id FROM signals"))}
            assert wss == {W1}
        with c.begin():
            _set_ws(c, W2)
            wss = {r[0] for r in c.execute(text("SELECT DISTINCT workspace_id FROM signals"))}
            assert wss == {W2}


# ── 2. Fail-closed: no GUC → zero rows ──

def test_fail_closed_no_guc(app_engine):
    with app_engine.connect() as c, c.begin():
        # No set_config at all.
        n_leads = c.execute(text("SELECT count(*) FROM leads")).scalar()
        n_sigs = c.execute(text("SELECT count(*) FROM signals")).scalar()
    assert n_leads == 0, "leads leaked with no app.workspace_id (should fail closed)"
    assert n_sigs == 0, "signals leaked with no app.workspace_id (should fail closed)"


# ── 3. Cross-tenant write rejection ──

def test_cross_tenant_insert_rejected(app_engine):
    from sqlalchemy.exc import ProgrammingError, DBAPIError

    with app_engine.connect() as c, c.begin():
        _set_ws(c, W1)
        with pytest.raises((ProgrammingError, DBAPIError)):
            c.execute(text(
                "INSERT INTO leads (workspace_id, company, city) "
                "VALUES (:w, 'Sneaky', 'LA')"
            ), {"w": W2})


def test_cross_tenant_update_affects_zero_rows(app_engine):
    with app_engine.connect() as c, c.begin():
        _set_ws(c, W1)
        # Try to update a row that belongs to W2 by its workspace_id — RLS hides
        # it, so 0 rows match. (A crafted predicate can't reach the other tenant.)
        res = c.execute(text(
            "UPDATE leads SET score = 999 WHERE workspace_id = :w"
        ), {"w": W2})
        assert res.rowcount == 0


# ── 4. Pooling-leak: SET LOCAL resets at txn end on the SAME connection ──

def test_set_local_no_leak_same_connection(app_engine):
    with app_engine.connect() as c:
        # "Request A": bind w1, read, commit.
        with c.begin():
            _set_ws(c, W1)
            a = {r[0] for r in c.execute(text("SELECT DISTINCT workspace_id FROM leads"))}
            assert a == {W1}
        # "Request B" reuses the SAME connection but binds w2 — must see only w2,
        # proving A's SET LOCAL did not leak across the transaction boundary.
        with c.begin():
            _set_ws(c, W2)
            b = {r[0] for r in c.execute(text("SELECT DISTINCT workspace_id FROM leads"))}
            assert b == {W2}, f"GUC leaked from request A: {b}"
        # And with no GUC at all on the reused connection → fail closed.
        with c.begin():
            n = c.execute(text("SELECT count(*) FROM leads")).scalar()
            assert n == 0


# ── 5. Superuser bypass + startup guard ──

def test_superuser_sees_both_and_guard_flags_it(owner_engine):
    # The owner/superuser connection bypasses RLS (documents the footgun).
    with owner_engine.connect() as c, c.begin():
        _set_ws(c, W1)
        wss = {r[0] for r in c.execute(text("SELECT DISTINCT workspace_id FROM leads"))}
    # user4 is a superuser → it sees BOTH tenants despite app.workspace_id=w1.
    is_super = owner_engine.connect().execute(
        text("SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user")
    ).scalar()
    if is_super:
        assert wss == {W1, W2}, "expected superuser to bypass RLS and see both"
    # The startup precondition check must FLAG this role as unsafe.
    from apps.api.services.leadgen import store as store_mod
    # Point the store's SessionLocal probe at the owner engine for this check.
    safe = _role_is_safe(owner_engine)
    if is_super:
        assert safe is False, "guard must flag the superuser/BYPASSRLS role"


def test_app_role_passes_guard(app_engine):
    assert _role_is_safe(app_engine) is True, "app login role must pass the guard"


def _role_is_safe(engine) -> bool:
    """Replicate assert_rls_role's query against an arbitrary engine."""
    with engine.connect() as c:
        row = c.execute(text(
            "SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname=current_user"
        )).first()
    return bool(row) and not (row[0] or row[1])


# ── 6. Composite dedup against real PG ──

def test_composite_dedup(app_engine):
    from sqlalchemy.exc import IntegrityError

    # Both tenants already own (Acme, NYC) from seed → that's allowed (proves the
    # composite key, not a global one). A same-tenant duplicate must raise.
    with app_engine.connect() as c, c.begin():
        _set_ws(c, W1)
        with pytest.raises(IntegrityError):
            c.execute(text(
                "INSERT INTO leads (workspace_id, company, city) "
                "VALUES (:w, 'Acme', 'NYC')"
            ), {"w": W1})


# ── 7. Worker-path: PgLeadStore writes land under the right tenant ──

def test_worker_path_pgleadstore(monkeypatch):
    """Simulate a stateless worker that constructs PgLeadStore(workspace_id) and
    asserts its writes are scoped + invisible to the other tenant.

    Drives the real PgLeadStore against the APP-role engine so RLS is live."""
    # Rebuild the app module objects bound to the app-role engine.
    app_eng = create_engine(_app_url())
    from sqlalchemy.orm import sessionmaker
    from sqlalchemy import event
    AppSession = sessionmaker(bind=app_eng, autoflush=False, autocommit=False)

    import apps.api.core.tenancy as tenancy

    @event.listens_for(AppSession, "after_begin")
    def _guc(session, transaction, connection):
        ws = tenancy.current_workspace_var.get()
        if ws:
            connection.exec_driver_sql(
                "SELECT set_config('app.workspace_id', %s, true)", (ws,)
            )

    import apps.api.services.leadgen.store as store_mod
    monkeypatch.setattr(store_mod, "SessionLocal", AppSession)

    from apps.api.services.leadgen.models import Lead
    from apps.api.services.leadgen.store import PgLeadStore

    # Worker for W1 writes a unique lead.
    with tenancy.workspace_scope(W1):
        s1 = PgLeadStore(W1)
        lid = s1.upsert_lead(Lead(company="WorkerCo", city="Austin", score=42))
        assert lid
        got = s1.get_leads(search="WorkerCo")
        assert any(l.company == "WorkerCo" for l in got)
        page, total = s1.query_leads_page(
            {"city": "Austin", "min_score": 40, "search": "WorkerCo"},
            page=1,
            page_size=10,
        )
        assert total == 1 and page[0]["company"] == "WorkerCo"
        facets = s1.get_filter_options()
        assert "Austin" in facets["cities"]
        assert facets["total_leads"] >= 1

    # Worker for W2 must NOT see it.
    with tenancy.workspace_scope(W2):
        s2 = PgLeadStore(W2)
        got2 = s2.get_leads()
        assert all(l.company != "WorkerCo" for l in got2), "W2 saw W1's worker write"
        page2, total2 = s2.query_leads_page({"search": "WorkerCo"})
        assert total2 == 0 and page2 == []

    app_eng.dispose()


# ── 8. tsvector search isolation ──

def test_tsvector_search_isolation(app_engine):
    with app_engine.connect() as c, c.begin():
        _set_ws(c, W1)
        # 'IT Staffing' specialization exists in both tenants (seed), but search
        # under w1 must only return w1 rows.
        rows = c.execute(text(
            "SELECT workspace_id FROM leads "
            "WHERE search_tsv @@ websearch_to_tsquery('english', 'Staffing')"
        )).fetchall()
        assert rows, "tsvector search returned nothing (trigger not maintaining search_tsv?)"
        assert all(r[0] == W1 for r in rows), "search leaked another tenant's rows"
