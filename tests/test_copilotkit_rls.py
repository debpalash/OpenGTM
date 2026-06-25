"""PG-gated cross-tenant tests for the legacy chat-agent tool path.

Proves the spec's core invariant against a LIVE Postgres with Row-Level Security:
the chat tools (`_execute_tool`) scoped to workspace W1 cannot read or write
W2's leads or workbooks — IDOR by id is blocked by the app-layer workspace
filter (and RLS for leads/signals as a DB backstop).

GATED on TEST_DATABASE_URL (an owner Postgres URL). Skips otherwise so the
default SQLite suite stays green. Connects for the assertions as a dedicated
NON-super, NON-BYPASSRLS login role so the policies are exercised for real.

Run (NEVER against the real `yupcha` db — use a throwaway):
    TEST_DATABASE_URL='postgresql+psycopg://user4@localhost:5432/<throwaway>' \
    PYTHONPATH=. uv run --group dev --with 'psycopg[binary]' \
    python -m pytest tests/test_copilotkit_rls.py -q
"""
import os
import sys
import json
import time
import uuid
import asyncio
import functools

import pytest
from sqlalchemy import create_engine, text, event
from sqlalchemy.orm import sessionmaker

PROJECT_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
if PROJECT_ROOT not in sys.path:
    sys.path.insert(0, PROJECT_ROOT)

TEST_DATABASE_URL = os.getenv("TEST_DATABASE_URL")

pytestmark = pytest.mark.skipif(
    not TEST_DATABASE_URL,
    reason="TEST_DATABASE_URL not set (Postgres copilotkit RLS tests skipped)",
)

APP_LOGIN_ROLE = "app_ck_rls_test"
W1 = "ws_ck_alpha"
W2 = "ws_ck_beta"


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
        c.execute(text(f"GRANT yupcha_app TO {APP_LOGIN_ROLE}"))
        c.execute(text(f"GRANT USAGE ON SCHEMA public TO {APP_LOGIN_ROLE}"))
        # workbooks has NO RLS; grant the app role write access (mirrors a
        # functional deployment where the chat authors workbooks). The tenancy
        # guard for workbooks is the app-layer workspace_id filter, tested below.
        c.execute(text(f"GRANT SELECT, INSERT, UPDATE, DELETE ON workbooks TO {APP_LOGIN_ROLE}"))

    # Seed leads for each tenant (owner conn, GUC set so FORCE RLS allows insert).
    seeded = {}
    with owner_engine.begin() as c:
        c.execute(text("DELETE FROM signals"))
        c.execute(text("DELETE FROM leads"))
    for ws, co in ((W1, "Alpha Corp"), (W2, "Beta LLC")):
        with owner_engine.begin() as c:
            c.execute(text("SELECT set_config('app.workspace_id', :w, true)"), {"w": ws})
            rid = c.execute(text(
                "INSERT INTO leads (workspace_id, company, city, score, status, email) "
                "VALUES (:w, :co, 'SF', 80, 'new', :em) RETURNING id"
            ), {"w": ws, "co": co, "em": f"hello@{ws}.com"}).scalar()
            seeded[ws] = {"lead_id": rid, "company": co, "email": f"hello@{ws}.com"}

    # Seed a workbook for W2 (the IDOR target).
    wb2_id = str(uuid.uuid4())
    with owner_engine.begin() as c:
        c.execute(text(
            "INSERT INTO workbooks (id, name, description, status, workspace_id, "
            "source_type, source_config, columns_config) "
            "VALUES (:id, 'W2 book', '', 'draft', :w, 'empty', "
            "CAST(:sc AS json), CAST(:cc AS json))"
        ), {"id": wb2_id, "w": W2, "sc": json.dumps({"workspace_id": W2}),
            "cc": json.dumps([{"id": "company", "name": "Company", "type": "lead_field"}])})
    seeded[W2]["workbook_id"] = wb2_id
    yield seeded


@pytest.fixture()
def app_env(schema, monkeypatch):
    """Bind store + ORM SessionLocal to an APP-ROLE engine with the RLS GUC hook.

    Returns (seeded, make_store). Every PG txn opened by PgLeadStore and by the
    workbook ORM tools runs as the non-super app role, so RLS is live."""
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
    monkeypatch.setattr(store_mod, "SessionLocal", AppSession)
    monkeypatch.setattr(database_mod, "SessionLocal", AppSession)

    from apps.api.services.leadgen.store import PgLeadStore

    def make_store(ws):
        return PgLeadStore(ws)

    yield schema, make_store
    app_eng.dispose()


def _run(coro):
    return asyncio.run(coro)


def _exec(name, args, store, ws):
    from apps.api.routers import copilotkit as ck
    from apps.api.core.tenancy import workspace_scope
    with workspace_scope(ws):
        return json.loads(_run(ck._execute_tool(
            name, args, store=store, workspace_id=ws, slug=ws
        )))


# ── Lead reads scoped to W1 ──────────────────────────────────────────────────

def test_search_leads_only_w1(app_env):
    seeded, make_store = app_env
    store = make_store(W1)
    out = _exec("search_leads", {"query": "", "limit": 50}, store, W1)
    companies = {l["company"] for l in out["leads"]}
    assert seeded[W1]["company"] in companies
    assert seeded[W2]["company"] not in companies


def test_get_lead_stats_counts_only_w1(app_env):
    seeded, make_store = app_env
    store = make_store(W1)
    out = _exec("get_lead_stats", {}, store, W1)
    assert out["total"] == 1, out


def test_get_enrichment_gaps_only_w1(app_env):
    seeded, make_store = app_env
    store = make_store(W1)
    out = _exec("get_enrichment_gaps", {"missing_field": "phone", "min_score": 0}, store, W1)
    ids = {g["id"] for g in out["gaps"]}
    assert seeded[W2]["lead_id"] not in ids


# ── IDOR by id: W1-scoped tools cannot read/write W2's lead ──────────────────

def test_get_lead_detail_w2_id_not_found(app_env):
    seeded, make_store = app_env
    store = make_store(W1)
    out = _exec("get_lead_detail", {"lead_id": seeded[W2]["lead_id"]}, store, W1)
    assert out == {"error": "Lead not found"}


def test_update_lead_status_w2_id_is_noop(app_env, owner_engine):
    seeded, make_store = app_env
    store = make_store(W1)
    _exec("update_lead_status",
          {"lead_id": seeded[W2]["lead_id"], "status": "dead"}, store, W1)
    # Verify from an OWNER connection that W2's lead is unchanged.
    with owner_engine.connect() as c:
        c.execute(text("SELECT set_config('app.workspace_id', :w, true)"), {"w": W2})
        status = c.execute(text("SELECT status FROM leads WHERE id = :i"),
                           {"i": seeded[W2]["lead_id"]}).scalar()
    assert status == "new", f"W2 lead was mutated cross-tenant: status={status}"


def test_enrich_lead_w2_id_not_found(app_env):
    seeded, make_store = app_env
    store = make_store(W1)
    out = _exec("enrich_lead", {"lead_id": seeded[W2]["lead_id"]}, store, W1)
    assert out == {"error": "Lead not found"}


# ── Workbook IDOR: W1-scoped tool cannot mutate W2's workbook ────────────────

def test_add_agent_column_w2_workbook_not_found(app_env, owner_engine):
    seeded, make_store = app_env
    store = make_store(W1)
    out = _exec("add_agent_column", {
        "workbook_id": seeded[W2]["workbook_id"], "column_name": "Email",
        "goal": "find email", "target_field": "email",
    }, store, W1)
    assert out == {"error": "Workbook not found"}
    # W2 workbook config untouched.
    with owner_engine.connect() as c:
        cfg = c.execute(text("SELECT columns_config FROM workbooks WHERE id = :i"),
                        {"i": seeded[W2]["workbook_id"]}).scalar()
    cfg = cfg if isinstance(cfg, list) else json.loads(cfg)
    assert not any(col.get("type") == "agent" for col in cfg), "W2 workbook was mutated"


def test_set_workbook_refresh_w2_workbook_not_found(app_env):
    seeded, make_store = app_env
    store = make_store(W1)
    out = _exec("set_workbook_refresh",
                {"workbook_id": seeded[W2]["workbook_id"], "interval": "daily"}, store, W1)
    assert out == {"error": "Workbook not found"}


# ── Autopilot e2e: bound execute_plan stamps workspace_id on the workbook ────

def test_autopilot_create_stamps_w1(app_env, owner_engine):
    from apps.api.routers import copilotkit as ck
    from apps.api.services.agent import autopilot
    from apps.api.core.tenancy import workspace_scope
    seeded, make_store = app_env
    store = make_store(W1)

    bound = functools.partial(ck._execute_tool, store=store, workspace_id=W1, slug=W1)
    plan = {"goal": "g", "estimated_rows": 5, "steps": [
        {"kind": "create_source_workbook", "description": "c",
         "params": {"icp_description": "IT staffing in Pune", "auto_run": False}},
    ]}
    with workspace_scope(W1):
        res = _run(autopilot.execute_plan(plan, bound))
    wb_id = res["workbook_id"]
    assert wb_id

    with owner_engine.connect() as c:
        row = c.execute(text(
            "SELECT workspace_id, source_config FROM workbooks WHERE id = :i"
        ), {"i": wb_id}).first()
    assert row is not None
    ws_id, src_cfg = row
    src_cfg = src_cfg if isinstance(src_cfg, dict) else json.loads(src_cfg)
    assert ws_id == W1
    assert src_cfg.get("workspace_id") == W1
