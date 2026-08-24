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
APP_LOGIN_PASSWORD = "mcp_rls_test_only"
W1 = "ws_mcp_alpha"
W2 = "ws_mcp_beta"


def _app_url():
    from sqlalchemy.engine import make_url
    u = make_url(TEST_DATABASE_URL)
    return u.set(username=APP_LOGIN_ROLE, password=APP_LOGIN_PASSWORD).render_as_string(hide_password=False)


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
        c.execute(text(f"ALTER ROLE {APP_LOGIN_ROLE} PASSWORD '{APP_LOGIN_PASSWORD}'"))
        c.execute(text(f"GRANT yupcha_app TO {APP_LOGIN_ROLE}"))
        c.execute(text(f"GRANT USAGE ON SCHEMA public TO {APP_LOGIN_ROLE}"))

    # Seed leads for each tenant (owner conn, GUC set so FORCE RLS allows insert).
    seeded = {}
    with owner_engine.begin() as c:
        c.execute(text("DELETE FROM signals"))
        c.execute(text("DELETE FROM leads"))
        c.execute(text("DELETE FROM mcp_audit_log"))
        c.execute(text("DELETE FROM outreach_enrollments"))
        c.execute(text("DELETE FROM outreach_sequences"))
        c.execute(text("DELETE FROM workbooks"))
        c.execute(text("DELETE FROM trigger_cap_reservations"))
    for ws, co in ((W1, "Alpha Corp"), (W2, "Beta LLC")):
        with owner_engine.begin() as c:
            c.execute(text("SELECT set_config('app.workspace_id', :w, true)"), {"w": ws})
            rid = c.execute(text(
                "INSERT INTO leads (workspace_id, company, city, score, status, email) "
                "VALUES (:w, :co, 'SF', 80, 'new', :em) RETURNING id"
            ), {"w": ws, "co": co, "em": f"hello@{ws}.com"}).scalar()
            seeded[ws] = {"lead_id": rid, "company": co}
    # Seed one outreach sequence in W1 (for the enroll write tests).
    with owner_engine.begin() as c:
        c.execute(text("SELECT set_config('app.workspace_id', :w, true)"), {"w": W1})
        c.execute(text(
            "INSERT INTO outreach_sequences "
            "(id, workspace_id, name, steps, status, daily_limit, "
            " send_window_start, send_window_end, send_window_tz, consent_basis) "
            "VALUES ('seq_w1', :w, 'Seq', '[]', 'draft', 50, 9, 18, 'UTC', 'legit')"
        ), {"w": W1})
        seeded[W1]["seq_id"] = "seq_w1"
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
    # The outreach store keeps its own module-level SessionLocal reference; bind it
    # to the app-role engine too so enroll writes run under RLS as the app role.
    import apps.api.services.outreach.store as outreach_store_mod
    monkeypatch.setattr(outreach_store_mod, "SessionLocal", AppSession)
    # Tools resolve their store via get_lead_store; point it at a real PgLeadStore
    # so reads exercise RLS (the process DATABASE_URL may be SQLite).
    monkeypatch.setattr(mcp_tools, "get_lead_store", lambda ws, slug: PgLeadStore(ws))

    # Cloud posture; membership/slug live in the SQLite meta store, so stub them.
    monkeypatch.setattr(mcp_auth.settings, "MCP_REQUIRE_AUTH", True)
    monkeypatch.setattr(mcp_auth.ws_manager, "is_member", lambda ws, uid: True)
    monkeypatch.setattr(mcp_auth.ws_manager, "workspace_slug", lambda ws: ws)
    monkeypatch.setattr(mcp_auth.ws_manager, "member_role", lambda ws, uid: "admin")
    # Phase-2 write tools enabled (the OFF/denial cases are unit-tested offline).
    monkeypatch.setattr(mcp_tools.settings, "MCP_WRITE_ENABLED", True)

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


# ══════════════════════════ Phase-2 WRITE proofs ═══════════════════════════════
# Every write routes through get_lead_store / the outreach store inside
# workspace_scope, so RLS scopes the mutation to the token's single workspace —
# a token bound to W1 can never write to W2 even when args name W2.

def _write_ctx(ws, caps):
    from apps.api.services.mcp import auth as mcp_auth
    raw, _ = mcp_auth.create_token(user_id=1, workspace_id=ws, capabilities=caps, name="w")
    return mcp_auth.resolve_mcp_token(raw)


def _count_app(ws, sql, params=None):
    """Count under the NON-super app role with the W GUC set (real RLS)."""
    eng = create_engine(_app_url())
    try:
        with eng.connect() as c:
            c.execute(text("SELECT set_config('app.workspace_id', :w, true)"), {"w": ws})
            return c.execute(text(sql), params or {}).scalar()
    finally:
        eng.dispose()


def test_create_lead_lands_in_w1_only(app_env):
    from apps.api.services.mcp import auth as mcp_auth
    ctx = _write_ctx(W1, [mcp_auth.CAP_LEADS_WRITE, mcp_auth.CAP_LEADS_READ])
    out = _exec(ctx, "create_lead", {"company": "MCP Made Co", "city": "NYC", "email": "x@mcp.co"})
    assert out["status"] == "created", out
    lid = out["lead_id"]
    # W1 sees its new lead; W2 (RLS) does not.
    assert _exec(ctx, "get_lead_detail", {"lead_id": lid})["company"] == "MCP Made Co"
    ctx2 = _write_ctx(W2, [mcp_auth.CAP_LEADS_READ])
    assert _exec(ctx2, "get_lead_detail", {"lead_id": lid}) == {"error": "Lead not found"}


def test_update_lead_cross_tenant_blocked(app_env):
    from apps.api.services.mcp import auth as mcp_auth
    seeded = app_env
    w2_lead = seeded[W2]["lead_id"]
    # W1 token tries to overwrite W2's lead by id → not found (RLS), no mutation.
    ctx = _write_ctx(W1, [mcp_auth.CAP_LEADS_WRITE])
    out = _exec(ctx, "update_lead", {"lead_id": w2_lead, "city": "HACKED"})
    assert out == {"error": "Lead not found"}
    # W2's row is untouched.
    ctx2 = _write_ctx(W2, [mcp_auth.CAP_LEADS_READ])
    assert _exec(ctx2, "get_lead_detail", {"lead_id": w2_lead})["city"] == "SF"


def test_create_workbook_rls_scoped(app_env):
    from apps.api.services.mcp import auth as mcp_auth
    ctx = _write_ctx(W1, [mcp_auth.CAP_WORKBOOKS_WRITE])
    out = _exec(ctx, "create_workbook", {"description": "emails in SF"})
    assert out["status"] == "created", out
    wid = out["workbook_id"]
    assert _count_app(W1, "SELECT count(*) FROM workbooks WHERE id=:i", {"i": wid}) == 1
    assert _count_app(W2, "SELECT count(*) FROM workbooks WHERE id=:i", {"i": wid}) == 0


def test_write_denied_without_cap_is_audited(app_env):
    from apps.api.services.mcp import auth as mcp_auth
    # No leads:write cap → refused AND a W1-scoped 'denied' audit row is written.
    ctx = _write_ctx(W1, [mcp_auth.CAP_LEADS_READ])
    out = _exec(ctx, "create_lead", {"company": "Nope"})
    assert out["error"] == "capability not granted"
    n = _count_app(
        W1,
        "SELECT count(*) FROM mcp_audit_log "
        "WHERE tool_name='create_lead' AND result_status='denied'",
    )
    assert n >= 1


def test_enroll_w1_records_consent(app_env):
    from apps.api.services.mcp import auth as mcp_auth
    seeded = app_env
    ctx = _write_ctx(W1, [mcp_auth.CAP_SEQUENCES_ENROLL])
    out = _exec(ctx, "enroll_leads", {
        "sequence_id": seeded[W1]["seq_id"], "lead_ids": [seeded[W1]["lead_id"]],
    })
    assert out["enrolled"] == 1, out
    cs = _count_app(
        W1,
        "SELECT count(*) FROM outreach_enrollments "
        "WHERE sequence_id=:s AND consent_source='mcp_enroll'",
        {"s": seeded[W1]["seq_id"]},
    )
    assert cs == 1


def test_enroll_cross_tenant_sequence_blocked(app_env):
    from apps.api.services.mcp import auth as mcp_auth
    seeded = app_env
    # W2 token enrolling into W1's sequence → sequence_exists is W2-scoped → miss.
    ctx = _write_ctx(W2, [mcp_auth.CAP_SEQUENCES_ENROLL])
    out = _exec(ctx, "enroll_leads", {
        "sequence_id": seeded[W1]["seq_id"], "lead_ids": [seeded[W2]["lead_id"]],
    })
    assert out == {"error": "Sequence not found"}
