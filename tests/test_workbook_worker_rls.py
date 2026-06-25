"""Workbook WORKER under FORCE RLS — the cutover proof (PG-gated). Spec §9.

The job runner has no request context: it learns its tenant from the job PAYLOAD
(OD-4) and enters ``workspace_scope`` before any query. These tests run the real
worker handlers against a NON-super, NON-BYPASSRLS connection (``app_rls_test``
granted ``yupcha_app``) with the production ``after_begin`` GUC hook installed, so
FORCE RLS is genuinely enforced. They prove:

  * handle_source_workbook materializes rows scoped to the PAYLOAD workspace, and
    those rows are visible ONLY under that tenant (criterion 6);
  * a job for tenant W2 cannot touch tenant W1's workbook (RLS hides it);
  * a job missing workspace_id fails loud (never an unscoped/global run);
  * the enrichment write path stamps + isolates workspace_id (criterion 6);
  * handle_signal_scan enumerates workspaces and runs each tenant's trigger pass
    under its own scope, writing per-tenant activity that obeys WITH CHECK
    (criterion 7).

GATED on TEST_DATABASE_URL; SKIPs otherwise. The owner (superuser) only seeds.

Run:
    TEST_DATABASE_URL='postgresql+psycopg://user4@localhost:5432/<throwaway>' \
    PYTHONPATH=. uv run --group dev --with 'psycopg[binary]' \
    python -m pytest tests/test_workbook_worker_rls.py -q
"""

import asyncio
import json
import os
import uuid

import pytest
from sqlalchemy import create_engine, event, text
from sqlalchemy.orm import sessionmaker

TEST_DATABASE_URL = os.getenv("TEST_DATABASE_URL")

pytestmark = pytest.mark.skipif(
    not TEST_DATABASE_URL,
    reason="TEST_DATABASE_URL not set (Postgres worker-RLS tests skipped)",
)

APP_LOGIN_ROLE = "app_rls_test"
W1 = "ws_wkr_alpha"
W2 = "ws_wkr_beta"


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
        # The production runtime role is the schema OWNER (FORCE binds RLS to it)
        # and so also reaches the non-RLS `jobs` queue. app_rls_test is only a
        # yupcha_app member, so grant it `jobs` DML explicitly to model that
        # effective privilege (the signal_scan handler enqueues refresh jobs).
        c.execute(text(f"GRANT SELECT, INSERT, UPDATE, DELETE ON jobs TO {APP_LOGIN_ROLE}"))
        c.execute(text(f"GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO {APP_LOGIN_ROLE}"))
    yield


@pytest.fixture(scope="module")
def app_engine(schema):
    eng = create_engine(_app_url())
    yield eng
    eng.dispose()


def _make_app_session(app_engine):
    """A sessionmaker bound to the app role + the production after_begin GUC hook,
    so every transaction is scoped to the active workspace_scope contextvar."""
    import apps.api.core.tenancy as tenancy

    AppSession = sessionmaker(bind=app_engine, autoflush=False)

    @event.listens_for(AppSession, "after_begin")
    def _guc(session, transaction, connection):  # noqa: ANN001
        ws = tenancy.current_workspace_var.get()
        if ws:
            connection.exec_driver_sql(
                "SELECT set_config('app.workspace_id', %s, true)", (ws,)
            )

    return AppSession


def _set_ws(conn, ws):
    conn.execute(text("SELECT set_config('app.workspace_id', :w, true)"), {"w": ws})


def _seed_source_workbook(owner_engine, ws, column_id="src1"):
    """Owner-seed a workbook carrying one source column under tenant ``ws``."""
    wid = str(uuid.uuid4())
    cols = [{"id": column_id, "name": "Source", "type": "source",
             "icp": {"description": "b2b saas companies"}, "target_rows": 0}]
    with owner_engine.begin() as c:
        _set_ws(c, ws)
        c.execute(text(
            "INSERT INTO workbooks (id, workspace_id, name, columns_config) "
            "VALUES (:id, :w, 'WB', CAST(:cfg AS json))"
        ), {"id": wid, "w": ws, "cfg": json.dumps(cols)})
    return wid


# ── helpers / fakes for the source engine ───────────────────────────────────

class _Ent:
    def __init__(self, company):
        self.id = f"ent:{company}"
        self.corroboration_count = 1


class _FakeStore:
    def __init__(self, leads):
        self._leads = leads

    def get_leads(self, *a, **k):
        return list(self._leads)

    def close(self):
        pass


def _patch_source_engine(monkeypatch, app_engine, leads):
    """Wire the source engine to the RLS app session + stub its externals."""
    import apps.api.services.workbook.source_engine as se
    import apps.api.services.leadgen.job_runner as jr_mod
    import apps.api.services.leadgen.store as store_mod
    import apps.api.services.workspace.manager as wsm
    import apps.api.services.automations.events as ev

    AppSession = _make_app_session(app_engine)
    monkeypatch.setattr(se, "SessionLocal", AppSession)
    monkeypatch.setattr(se, "_make_redis", lambda: None)
    monkeypatch.setattr(se, "resolve_company",
                        lambda db, d, **k: (_Ent(d.get("company")), True))

    class _FakeRunner:
        async def submit(self, *a, **k):
            return "jobX"

    monkeypatch.setattr(jr_mod, "JobRunner", _FakeRunner)
    monkeypatch.setattr(store_mod, "get_lead_store", lambda ws, slug: _FakeStore(leads))
    monkeypatch.setattr(wsm, "workspace_slug", lambda ws: "slug")
    monkeypatch.setattr(ev, "emit_row_added", lambda *a, **k: None)
    return se


# ── 1. source worker scopes from payload; rows visible only under that tenant ─

def test_handle_source_workbook_scoped_to_payload(app_engine, owner_engine, monkeypatch):
    wid = _seed_source_workbook(owner_engine, W1)
    leads = [
        {"company": "Acme Inc", "id": 501, "website": "acme.com", "source": "job:jobX"},
        {"company": "Globex", "id": 502, "website": "globex.com", "source": "job:jobX"},
    ]
    se = _patch_source_engine(monkeypatch, app_engine, leads)

    asyncio.run(se.handle_source_workbook(
        1, {"workbook_id": wid, "column_id": "src1", "workspace_id": W1}
    ))

    # Visible (and correctly stamped) under W1.
    with app_engine.connect() as c, c.begin():
        _set_ws(c, W1)
        rows = c.execute(text(
            "SELECT workspace_id FROM workbook_rows WHERE workbook_id = :wid"
        ), {"wid": wid}).fetchall()
    assert len(rows) == 2, f"expected 2 sourced rows under W1, got {len(rows)}"
    assert all(r[0] == W1 for r in rows)

    # Invisible under W2 (RLS isolation).
    with app_engine.connect() as c, c.begin():
        _set_ws(c, W2)
        n = c.execute(text(
            "SELECT count(*) FROM workbook_rows WHERE workbook_id = :wid"
        ), {"wid": wid}).scalar()
    assert n == 0, "sourced rows leaked into W2"


# ── 2. a W2 job cannot reach W1's workbook (RLS hides the row) ───────────────

def test_handle_source_workbook_cross_tenant_not_found(app_engine, owner_engine, monkeypatch):
    wid = _seed_source_workbook(owner_engine, W1, column_id="srcX")
    leads = [{"company": "ShouldNotAppear", "id": 999}]
    se = _patch_source_engine(monkeypatch, app_engine, leads)

    result = asyncio.run(se.handle_source_workbook(
        2, {"workbook_id": wid, "column_id": "srcX", "workspace_id": W2}
    ))
    # The W1 workbook is invisible under W2 → handler reports not found, writes 0.
    assert result is None or result.get("error") == "workbook_not_found" or result.get("added", 0) == 0
    with app_engine.connect() as c, c.begin():
        _set_ws(c, W1)
        n = c.execute(text(
            "SELECT count(*) FROM workbook_rows WHERE workbook_id = :wid"
        ), {"wid": wid}).scalar()
    assert n == 0, "a cross-tenant W2 job must not create rows in W1's workbook"


# ── 3. missing workspace_id fails loud (no silent unscoped run) ──────────────

def test_handle_source_workbook_missing_ws_fails_loud(app_engine, owner_engine, monkeypatch):
    wid = _seed_source_workbook(owner_engine, W1, column_id="srcM")
    se = _patch_source_engine(monkeypatch, app_engine, [])
    with pytest.raises(ValueError):
        asyncio.run(se.handle_source_workbook(3, {"workbook_id": wid, "column_id": "srcM"}))


# ── 4. enrichment write path stamps + isolates workspace_id ─────────────────

def test_set_enrichment_write_scoped(app_engine, owner_engine, monkeypatch):
    import apps.api.services.workbook.enrichment as enr
    import apps.api.core.tenancy as tenancy

    # Seed a workbook + one row under W1.
    wid = str(uuid.uuid4())
    with owner_engine.begin() as c:
        _set_ws(c, W1)
        c.execute(text("INSERT INTO workbooks (id, workspace_id, name) VALUES (:id, :w, 'E')"),
                  {"id": wid, "w": W1})
        c.execute(text(
            "INSERT INTO workbook_rows (workspace_id, workbook_id, position, data, lead_id) "
            "VALUES (:w, :wid, 0, '{}', 7001)"
        ), {"w": W1, "wid": wid})

    AppSession = _make_app_session(app_engine)
    monkeypatch.setattr(enr, "SessionLocal", AppSession, raising=False)

    with tenancy.workspace_scope(W1):
        with AppSession() as db:
            enr._set_enrichment(db, wid, 7001, "col_e", "found@x.com", "complete",
                                provider="test")
            db.commit()

    # Stamped W1 + visible only under W1.
    with app_engine.connect() as c, c.begin():
        _set_ws(c, W1)
        row = c.execute(text(
            "SELECT workspace_id, value FROM workbook_enrichments "
            "WHERE workbook_id = :wid AND column_id = 'col_e'"
        ), {"wid": wid}).first()
    assert row is not None and row[0] == W1 and row[1] == "found@x.com"

    with app_engine.connect() as c, c.begin():
        _set_ws(c, W2)
        n = c.execute(text(
            "SELECT count(*) FROM workbook_enrichments WHERE workbook_id = :wid"
        ), {"wid": wid}).scalar()
    assert n == 0, "enrichment leaked into W2"


# ── 5. signal_scan enumerates workspaces; per-tenant scope + activity ────────

def test_handle_signal_scan_enumerates_under_rls(app_engine, owner_engine, monkeypatch):
    import apps.api.services.workbook.refresh as rf
    import apps.api.services.workspace.manager as wsm

    # Clean any living workbooks left in these tenants by prior runs (the
    # throwaway DB persists across invocations), then seed exactly one
    # hiring-subscribed living workbook per tenant. The FK cascade clears rows.
    with owner_engine.begin() as c:
        c.execute(text("DELETE FROM workbooks WHERE workspace_id IN (:a, :b)"),
                  {"a": W1, "b": W2})
    wids = {}
    for ws in (W1, W2):
        wid = str(uuid.uuid4())
        wids[ws] = wid
        with owner_engine.begin() as c:
            _set_ws(c, ws)
            c.execute(text(
                "INSERT INTO workbooks (id, workspace_id, name, refresh_policy) "
                "VALUES (:id, :w, 'L', CAST(:rp AS json))"
            ), {"id": wid, "w": ws,
                "rp": json.dumps({"enabled": True, "on_signal": ["hiring"]})})
    # Clear prior refresh jobs for a clean assertion.
    with owner_engine.begin() as c:
        c.execute(text("DELETE FROM jobs WHERE type IN ('refresh_workbook','signal_scan')"))

    AppSession = _make_app_session(app_engine)
    monkeypatch.setattr(rf, "SessionLocal", AppSession)

    async def _fake_scan():
        return {"signals_found": 0}

    monkeypatch.setattr("apps.api.services.signals.monitor.run_signal_scan", _fake_scan)

    class _WS:
        def __init__(self, i):
            self.id = i
            self.slug = i

    monkeypatch.setattr(wsm, "list_workspaces", lambda: [_WS(W1), _WS(W2)])

    asyncio.run(rf.handle_signal_scan(9, {"signal_types": ["hiring"]}))

    # One refresh_workbook job per tenant, each stamped with its own workspace_id.
    with owner_engine.begin() as c:
        jobs = c.execute(text(
            "SELECT payload FROM jobs WHERE type='refresh_workbook'"
        )).fetchall()
    by_wb = {}
    for (payload,) in jobs:
        p = payload if isinstance(payload, dict) else json.loads(payload)
        by_wb[p.get("workbook_id")] = p
    assert set(by_wb) == {wids[W1], wids[W2]}, by_wb
    assert by_wb[wids[W1]]["workspace_id"] == W1
    assert by_wb[wids[W2]]["workspace_id"] == W2

    # Per-tenant activity rows were written and obey RLS (WITH CHECK passed).
    with app_engine.connect() as c, c.begin():
        _set_ws(c, W1)
        n1 = c.execute(text(
            "SELECT count(*) FROM workbook_activity WHERE workbook_id = :wid AND kind='signal'"
        ), {"wid": wids[W1]}).scalar()
    assert n1 == 1
