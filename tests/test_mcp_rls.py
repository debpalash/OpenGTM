"""PG-gated cross-tenant tests for the MCP tool path (Phase-1 security).

Proves the spec's core invariant against a LIVE Postgres with Row-Level Security:
an MCP token bound to workspace W1 cannot read W2's leads — the read tools route
through ``get_lead_store`` inside ``workspace_scope``, so RLS scopes every query
to the token's single workspace (AC2/AC3/AC9). Also proves the ``mcp_audit_log``
table is itself RLS-scoped (a W1 audit row is invisible to a W2-scoped read).

GATED on TEST_DATABASE_URL (an owner Postgres URL). Skips otherwise so the
default SQLite suite stays green. Connects for the assertions as a dedicated
NON-super, NON-BYPASSRLS login role so the policies are exercised for real.

Run (NEVER against the real `yupcha` db — use a throwaway):
    TEST_DATABASE_URL='postgresql+psycopg://user4@localhost:5432/<throwaway>' \
    PYTHONPATH=. uv run --group dev --with 'psycopg[binary]' \
    python -m pytest tests/test_mcp_rls.py -q
"""
import os
import sys
import json
import asyncio

import pytest
from sqlalchemy import create_engine, text, event
from sqlalchemy.orm import sessionmaker

PROJECT_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
if PROJECT_ROOT not in sys.path:
    sys.path.insert(0, PROJECT_ROOT)

TEST_DATABASE_URL = os.getenv("TEST_DATABASE_URL")

pytestmark = pytest.mark.skipif(
    not TEST_DATABASE_URL,
    reason="TEST_DATABASE_URL not set (Postgres MCP RLS tests skipped)",
)

APP_LOGIN_ROLE = "app_mcp_rls_test"
W1 = "ws_mcp_alpha"
W2 = "ws_mcp_beta"


def _app_url():
    from sqlalchemy.engine import make_url
    u = make_url(TEST_DATABASE_URL)
    return str(u.set(username=APP_LOGIN_ROLE, password=None))


@pytest.fixture(scope="module")
def owner_engine():
    eng = create_engine(TEST_DATABASE_URL, poolclass=None)
    yield eng
    eng.dispose()


@pytest.fixture(scope="module")
def schema(owner_engine):
    """alembic upgrade head, create the app login role + grants, seed 2 tenants."""
    import subprocess

    env = dict(os.environ, DATABASE_URL=TEST_DATABASE_URL)
    res = subprocess.run(
        ["uv", "run", "--with", "psycopg[binary]", "alembic", "upgrade", "head"],
        cwd=PROJECT_ROOT, env=env, capture_output=True, text=True,
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
        # Inherit the app role grants (leads/signals/mcp_audit_log/mcp_tokens).
        c.execute(text(f"GRANT yupcha_app TO {APP_LOGIN_ROLE}"))
        c.execute(text(f"GRANT USAGE ON SCHEMA public TO {APP_LOGIN_ROLE}"))

    # Seed leads for each tenant (owner conn, GUC set so FORCE RLS allows insert).
    seeded = {}
    with owner_engine.begin() as c:
        c.execute(text("DELETE FROM signals"))
        c.execute(text("DELETE FROM leads"))
        c.execute(text("DELETE FROM mcp_audit_log"))
    for ws, co in ((W1, "Alpha Corp"), (W2, "Beta LLC")):
        with owner_engine.begin() as c:
            c.execute(text("SELECT set_config('app.workspace_id', :w, true)"), {"w": ws})
            rid = c.execute(text(
                "INSERT INTO leads (workspace_id, company, city, score, status, email) "
                "VALUES (:w, :co, 'SF', 80, 'new', :em) RETURNING id"
            ), {"w": ws, "co": co, "em": f"hello@{ws}.com"}).scalar()
            seeded[ws] = {"lead_id": rid, "company": co}
    yield seeded


@pytest.fixture()
def app_env(schema, monkeypatch):
    """Bind store + ORM SessionLocal to a NON-super app-role engine with the GUC hook.

    Every PG txn opened by PgLeadStore, the token lookup, and the audit writer
    runs as the app role, so RLS is live. Returns the seeded dict."""
    app_eng = create_engine(_app_url())
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
    import apps.api.database as database_mod
    from apps.api.services.mcp import auth as mcp_auth
    from apps.api.services.mcp import tools as mcp_tools
    from apps.api.services.leadgen.store import PgLeadStore

    monkeypatch.setattr(store_mod, "SessionLocal", AppSession)
    monkeypatch.setattr(database_mod, "SessionLocal", AppSession)
    # Tools resolve their store via get_lead_store; point it at a real PgLeadStore
    # so reads exercise RLS (the process DATABASE_URL may be SQLite).
    monkeypatch.setattr(mcp_tools, "get_lead_store", lambda ws, slug: PgLeadStore(ws))

    # Cloud posture; membership/slug live in the SQLite meta store, so stub them.
    monkeypatch.setattr(mcp_auth.settings, "MCP_REQUIRE_AUTH", True)
    monkeypatch.setattr(mcp_auth.ws_manager, "is_member", lambda ws, uid: True)
    monkeypatch.setattr(mcp_auth.ws_manager, "workspace_slug", lambda ws: ws)
    monkeypatch.setattr(mcp_auth.ws_manager, "member_role", lambda ws, uid: "admin")

    yield schema
    app_eng.dispose()


def _run(coro):
    return asyncio.run(coro)


def _token_ctx(ws):
    from apps.api.services.mcp import auth as mcp_auth
    raw, _ = mcp_auth.create_token(
        user_id=1, workspace_id=ws, capabilities=[mcp_auth.CAP_LEADS_READ], name="t"
    )
    return mcp_auth.resolve_mcp_token(raw)


def _exec(ctx, name, args):
    from apps.api.services.mcp import tools as mcp_tools
    return json.loads(_run(mcp_tools.execute_tool(ctx, name, args)))


# ── Token is workspace-bound (AC3) ────────────────────────────────────────────

def test_token_bound_to_single_workspace(app_env):
    ctx = _token_ctx(W1)
    assert ctx.workspace_id == W1
    assert ctx.has("leads:read")


# ── find_leads scoped to W1 — W2 is invisible (AC9) ───────────────────────────

def test_find_leads_only_w1(app_env):
    seeded = app_env
    ctx = _token_ctx(W1)
    out = _exec(ctx, "find_leads", {"query": "", "limit": 50})
    companies = {l["company"] for l in out["leads"]}
    assert seeded[W1]["company"] in companies
    assert seeded[W2]["company"] not in companies


def test_pipeline_stats_counts_only_w1(app_env):
    ctx = _token_ctx(W1)
    out = _exec(ctx, "get_pipeline_stats", {})
    assert out["total"] == 1, out


# ── IDOR by id: a W1 token cannot read W2's lead (AC9) ─────────────────────────

def test_get_lead_detail_w2_id_not_found(app_env):
    seeded = app_env
    ctx = _token_ctx(W1)
    out = _exec(ctx, "get_lead_detail", {"lead_id": seeded[W2]["lead_id"]})
    assert out == {"error": "Lead not found"}


def test_score_lead_w2_id_not_found(app_env):
    seeded = app_env
    ctx = _token_ctx(W1)
    out = _exec(ctx, "score_lead", {"lead_id": seeded[W2]["lead_id"]})
    assert out == {"error": "Lead not found"}


# ── mcp_audit_log is itself RLS-scoped ────────────────────────────────────────

def test_audit_log_is_rls_scoped(app_env):
    """A W1 audit row is invisible to a W2-scoped read (fail-closed policy).

    Reads are issued as the NON-super app role so RLS is actually enforced (the
    owner login may be a superuser, which bypasses RLS entirely)."""
    from apps.api.services.mcp import audit
    ctx = _token_ctx(W1)
    audit.record(ctx, "find_leads", {"query": "x"}, result_status="ok")

    app_eng = create_engine(_app_url())
    try:
        with app_eng.connect() as c:
            c.execute(text("SELECT set_config('app.workspace_id', :w, true)"), {"w": W1})
            n1 = c.execute(text("SELECT count(*) FROM mcp_audit_log")).scalar()
        with app_eng.connect() as c:
            c.execute(text("SELECT set_config('app.workspace_id', :w, true)"), {"w": W2})
            n2 = c.execute(text("SELECT count(*) FROM mcp_audit_log")).scalar()
    finally:
        app_eng.dispose()
    assert n1 >= 1
    assert n2 == 0
