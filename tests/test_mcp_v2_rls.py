"""PG-gated cross-tenant + send-safety tests for the MCP v2 write tools.

Proves against a LIVE Postgres with Row-Level Security that the v2 ops cannot
cross tenant boundaries and that the send tool routes through the REAL outreach
send path (suppression honoured, SMTP mocked — never a real email):

  * ``create_automation`` lands only in the token's workspace (RLS WITH CHECK);
    a W2 token's rule is invisible to W1.
  * ``delete_lead`` / ``delete_workbook`` by a W1 token against a W2 id is a
    no-op miss (RLS) — W2's row is untouched. Admin role is re-checked live.
  * ``send_email`` is refused (audited) when AUTOMATIONS_ALLOW_LEGACY_OUTREACH is
    off; a cross-tenant lead is skipped (no email); a suppressed recipient is
    skipped by the durable handler with NO SMTP call.

GATED on TEST_DATABASE_URL. Connects for assertions as a NON-super, NON-BYPASSRLS
login role so the policies are exercised for real.

Run (NEVER against the real `yupcha` db — use a throwaway):
    TEST_DATABASE_URL='postgresql+psycopg://user4@localhost:5432/<throwaway>' \
    PYTHONPATH=. uv run --group dev --with 'psycopg[binary]' \
    python -m pytest tests/test_mcp_v2_rls.py -q
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
    reason="TEST_DATABASE_URL not set (Postgres MCP v2 RLS tests skipped)",
)

APP_LOGIN_ROLE = "app_mcp_v2_rls_test"
APP_LOGIN_PASSWORD = "mcp_v2_rls_test_only"
W1 = "ws_mcpv2_alpha"
W2 = "ws_mcpv2_beta"


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
        c.execute(text(f"ALTER ROLE {APP_LOGIN_ROLE} PASSWORD '{APP_LOGIN_PASSWORD}'"))
        c.execute(text(f"GRANT yupcha_app TO {APP_LOGIN_ROLE}"))
        c.execute(text(f"GRANT USAGE ON SCHEMA public TO {APP_LOGIN_ROLE}"))
        # The durable queue (`jobs`) is owned, not RLS — the worker role that runs
        # the send handler has DML on it. Model that so the send path can enqueue.
        c.execute(text(f"GRANT SELECT, INSERT, UPDATE, DELETE ON jobs TO {APP_LOGIN_ROLE}"))
        c.execute(text(f"GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO {APP_LOGIN_ROLE}"))

    with owner_engine.begin() as c:
        for tbl in ("signals", "leads", "mcp_audit_log", "outreach_sends",
                    "outreach_enrollments", "outreach_suppressions",
                    "outreach_sequences", "workbooks", "triggers",
                    "trigger_cap_reservations"):
            c.execute(text(f"DELETE FROM {tbl}"))

    seeded = {}
    for ws, co in ((W1, "Alpha Corp"), (W2, "Beta LLC")):
        with owner_engine.begin() as c:
            c.execute(text("SELECT set_config('app.workspace_id', :w, true)"), {"w": ws})
            rid = c.execute(text(
                "INSERT INTO leads (workspace_id, company, city, score, status, email) "
                "VALUES (:w, :co, 'SF', 80, 'new', :em) RETURNING id"
            ), {"w": ws, "co": co, "em": f"hello@{ws}.com"}).scalar()
            wid = c.execute(text(
                "INSERT INTO workbooks (id, workspace_id, name, source_type, "
                " source_config, filter_criteria, columns_config) "
                "VALUES (gen_random_uuid()::text, :w, 'WB', 'empty', '{}', '{}', '[]') "
                "RETURNING id"
            ), {"w": ws}).scalar()
            seeded[ws] = {"lead_id": rid, "company": co, "workbook_id": wid,
                          "email": f"hello@{ws}.com"}
    yield seeded


@pytest.fixture()
def app_env(schema, monkeypatch):
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
    import apps.api.services.outreach.store as outreach_store_mod
    from apps.api.services.mcp import auth as mcp_auth
    from apps.api.services.mcp import tools as mcp_tools
    from apps.api.services.leadgen.store import PgLeadStore

    monkeypatch.setattr(store_mod, "SessionLocal", AppSession)
    monkeypatch.setattr(database_mod, "SessionLocal", AppSession)
    monkeypatch.setattr(outreach_store_mod, "SessionLocal", AppSession)
    monkeypatch.setattr(mcp_tools, "get_lead_store", lambda ws, slug: PgLeadStore(ws))

    monkeypatch.setattr(mcp_auth.settings, "MCP_REQUIRE_AUTH", True)
    monkeypatch.setattr(mcp_auth.ws_manager, "is_member", lambda ws, uid: True)
    monkeypatch.setattr(mcp_auth.ws_manager, "workspace_slug", lambda ws: ws)
    monkeypatch.setattr(mcp_auth.ws_manager, "member_role", lambda ws, uid: "admin")
    # v2 writes enabled + automations master switch on for create_automation/send.
    monkeypatch.setattr(mcp_tools.settings, "MCP_WRITE_ENABLED", True)
    monkeypatch.setattr(mcp_tools.settings, "AUTOMATIONS_ENABLED", True)
    # legacy outreach OFF by default — send tests turn it on explicitly.
    monkeypatch.setattr(mcp_tools.settings, "AUTOMATIONS_ALLOW_LEGACY_OUTREACH", False)

    yield schema
    app_eng.dispose()


def _run(coro):
    return asyncio.run(coro)


def _ctx(ws, caps):
    from apps.api.services.mcp import auth as mcp_auth
    raw, _ = mcp_auth.create_token(user_id=1, workspace_id=ws, capabilities=caps, name="v2")
    return mcp_auth.resolve_mcp_token(raw)


def _exec(ctx, name, args):
    from apps.api.services.mcp import tools as mcp_tools
    return json.loads(_run(mcp_tools.execute_tool(ctx, name, args)))


def _count_app(ws, sql, params=None):
    eng = create_engine(_app_url())
    try:
        with eng.connect() as c:
            c.execute(text("SELECT set_config('app.workspace_id', :w, true)"), {"w": ws})
            return c.execute(text(sql), params or {}).scalar()
    finally:
        eng.dispose()


# ══════════════════════════ create_automation ══════════════════════════════════

def test_create_automation_lands_in_w1_only(app_env):
    from apps.api.services.mcp import auth as mcp_auth
    ctx = _ctx(W1, [mcp_auth.CAP_AUTOMATIONS_WRITE])
    out = _exec(ctx, "create_automation", {
        "name": "W1 rule", "trigger_type": "on_row_added", "actions": [],
    })
    assert out["status"] == "created", out
    tid = out["automation_id"]
    assert _count_app(W1, "SELECT count(*) FROM triggers WHERE id=:i", {"i": tid}) == 1
    assert _count_app(W2, "SELECT count(*) FROM triggers WHERE id=:i", {"i": tid}) == 0


def test_create_automation_w2_invisible_to_w1(app_env):
    from apps.api.services.mcp import auth as mcp_auth
    ctx2 = _ctx(W2, [mcp_auth.CAP_AUTOMATIONS_WRITE])
    out = _exec(ctx2, "create_automation", {
        "name": "W2 rule", "trigger_type": "on_row_added", "actions": [],
    })
    assert out["status"] == "created", out
    tid = out["automation_id"]
    assert _count_app(W2, "SELECT count(*) FROM triggers WHERE id=:i", {"i": tid}) == 1
    assert _count_app(W1, "SELECT count(*) FROM triggers WHERE id=:i", {"i": tid}) == 0


# ══════════════════════════ delete_lead / delete_workbook ══════════════════════

def test_delete_lead_cross_tenant_blocked(app_env):
    from apps.api.services.mcp import auth as mcp_auth
    seeded = app_env
    w2_lead = seeded[W2]["lead_id"]
    ctx = _ctx(W1, [mcp_auth.CAP_LEADS_DELETE])
    out = _exec(ctx, "delete_lead", {"lead_id": w2_lead})
    assert out["status"] == "not_found"  # RLS miss — never deletes W2's lead
    # W2's lead still exists.
    assert _count_app(W2, "SELECT count(*) FROM leads WHERE id=:i", {"i": w2_lead}) == 1


def test_delete_lead_admin_required(app_env, monkeypatch):
    from apps.api.services.mcp import auth as mcp_auth
    seeded = app_env
    monkeypatch.setattr(mcp_auth.ws_manager, "member_role", lambda ws, uid: "member")
    ctx = _ctx(W1, [mcp_auth.CAP_LEADS_DELETE])
    out = _exec(ctx, "delete_lead", {"lead_id": seeded[W1]["lead_id"]})
    assert out["error"] == "insufficient workspace role"
    assert _count_app(W1, "SELECT count(*) FROM leads WHERE id=:i",
                      {"i": seeded[W1]["lead_id"]}) == 1  # untouched
    # The denial is audited (W1-scoped).
    assert _count_app(
        W1,
        "SELECT count(*) FROM mcp_audit_log "
        "WHERE tool_name='delete_lead' AND result_status='denied'") >= 1


def test_delete_workbook_cross_tenant_blocked(app_env):
    from apps.api.services.mcp import auth as mcp_auth
    seeded = app_env
    w2_wb = seeded[W2]["workbook_id"]
    ctx = _ctx(W1, [mcp_auth.CAP_WORKBOOKS_DELETE])
    out = _exec(ctx, "delete_workbook", {"workbook_id": w2_wb})
    assert out["status"] == "not_found"
    assert _count_app(W2, "SELECT count(*) FROM workbooks WHERE id=:i", {"i": w2_wb}) == 1


def test_delete_workbook_w1_deletes_own(app_env):
    from apps.api.services.mcp import auth as mcp_auth
    seeded = app_env
    w1_wb = seeded[W1]["workbook_id"]
    ctx = _ctx(W1, [mcp_auth.CAP_WORKBOOKS_DELETE])
    out = _exec(ctx, "delete_workbook", {"workbook_id": w1_wb})
    assert out["status"] == "deleted"
    assert _count_app(W1, "SELECT count(*) FROM workbooks WHERE id=:i", {"i": w1_wb}) == 0


# ══════════════════════════ send_email ═════════════════════════════════════════

def test_send_email_refused_when_legacy_off_audited(app_env):
    from apps.api.services.mcp import auth as mcp_auth
    seeded = app_env
    ctx = _ctx(W1, [mcp_auth.CAP_OUTREACH_SEND])
    out = _exec(ctx, "send_email", {"lead_id": seeded[W1]["lead_id"], "subject": "x"})
    assert out == {"error": "legacy_outreach_disabled"}
    assert _count_app(
        W1,
        "SELECT count(*) FROM mcp_audit_log "
        "WHERE tool_name='send_email' AND result_status='denied'") >= 1


def test_send_email_cross_tenant_lead_skipped(app_env, monkeypatch):
    from apps.api.services.mcp import auth as mcp_auth
    from apps.api.services.mcp import tools as mcp_tools
    seeded = app_env
    monkeypatch.setattr(mcp_tools.settings, "AUTOMATIONS_ALLOW_LEGACY_OUTREACH", True)
    # W1 token names W2's lead id → resolves to nothing under RLS → no email.
    ctx = _ctx(W1, [mcp_auth.CAP_OUTREACH_SEND])
    out = _exec(ctx, "send_email", {"lead_id": seeded[W2]["lead_id"], "subject": "x"})
    # W2's lead is invisible under W1's RLS scope → no email → nothing to send.
    assert out["status"] == "skipped" and out["skip_reason"] == "no_email"


def test_send_email_suppressed_recipient_no_smtp(app_env, monkeypatch):
    """End-to-end: MCP enqueues through the REAL path; the durable handler skips a
    suppressed recipient and NEVER calls SMTP (mocked). Proves suppression."""
    from apps.api.services.mcp import auth as mcp_auth
    from apps.api.services.mcp import tools as mcp_tools
    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.outreach.store import get_outreach_store
    from apps.api.services.outreach import sender as sender_mod
    from apps.api.services.outreach import sending as sending_mod
    seeded = app_env
    monkeypatch.setattr(mcp_tools.settings, "AUTOMATIONS_ALLOW_LEGACY_OUTREACH", True)

    email = seeded[W1]["email"]
    with workspace_scope(W1):
        get_outreach_store(W1).add_suppression(email, reason="manual", source="test",
                                               locked=False)

    # Mock SMTP — must never be invoked for a suppressed recipient.
    sent = []

    async def _never(*a, **k):
        sent.append(1)
        from apps.api.services.outreach.sender import SendResult
        return SendResult(success=True, message_id="x")

    monkeypatch.setattr(sender_mod, "send_email", _never)

    ctx = _ctx(W1, [mcp_auth.CAP_OUTREACH_SEND])
    out = _exec(ctx, "send_email", {"lead_id": seeded[W1]["lead_id"],
                                    "subject": "Hi", "body_html": "<p>x</p>"})
    assert out["status"] in ("success", "skipped"), out

    # Drain the enqueued send job through the real handler.
    job = _count_app(W1, "SELECT id FROM jobs WHERE type='send' ORDER BY id DESC LIMIT 1")
    if job is not None:
        eng = create_engine(_app_url())
        with eng.connect() as c:
            c.execute(text("SELECT set_config('app.workspace_id', :w, true)"), {"w": W1})
            payload = c.execute(text("SELECT payload FROM jobs WHERE id=:i"),
                                {"i": job}).scalar()
        eng.dispose()
        if isinstance(payload, str):
            payload = json.loads(payload)
        _run(sending_mod.handle_send(job, payload))

    assert sent == []  # SMTP never called for a suppressed recipient
    # The send is recorded as skipped/suppressed (not sent).
    n_sent = _count_app(W1, "SELECT count(*) FROM outreach_sends WHERE status='sent'")
    assert n_sent == 0
