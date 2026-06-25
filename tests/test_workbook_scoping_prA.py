"""PR-A workbook tenant-scoping — behavioural tests (SQLite, always-on).

These prove the scoping refactor that PR-B's FORCE-RLS cutover depends on, while
RLS is still OFF (so the suite runs on the default SQLite engine and the changes
are a behavioural no-op):

  * every workbook-job worker handler enters ``workspace_scope`` taken from the
    job PAYLOAD (OD-4) before any workbook query;
  * a payload missing ``workspace_id`` FAILS LOUD (never runs unscoped/global);
  * ``handle_signal_scan`` enumerates workspaces and triggers each tenant's
    workbooks under that tenant's scope (no global ``Workbook.all()``), stamping
    ``workspace_id`` into every enqueued ``refresh_workbook`` payload;
  * the worker-side re-enqueue helpers stamp ``workspace_id``;
  * the websocket authorizer authorizes against the out-of-band ``workspace_id``
    query param (OD-5);
  * every enqueue site's source includes ``workspace_id`` in the payload
    (regression guard).

The PG-gated worker-under-RLS proof (visibility/fail-closed) belongs to PR-B's
``tests/test_workbook_worker_rls.py`` once the migration + child columns exist.
"""

import asyncio
import inspect

import pytest
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.database import Base
from apps.api.core.tenancy import current_workspace_var
from apps.api.models import Job, User
from apps.api.services.workbook.models import Workbook, WorkbookRow
from apps.api.services.workbook.activity_models import WorkbookActivity

W1 = "ws_one"
W2 = "ws_two"


# ── engine / session helpers ──────────────────────────────────────────────────

@pytest.fixture()
def session_factory():
    engine = create_engine(
        "sqlite:///:memory:", connect_args={"check_same_thread": False}, poolclass=StaticPool,
    )
    Base.metadata.create_all(engine, tables=[
        Job.__table__, User.__table__,
        Workbook.__table__, WorkbookRow.__table__, WorkbookActivity.__table__,
    ])
    return sessionmaker(bind=engine, autoflush=False, autocommit=False)


def _bind_sessionlocal(monkeypatch, factory, *modnames):
    for mod in modnames:
        monkeypatch.setattr(mod + ".SessionLocal", factory, raising=True)


# ── worker handlers: scope-from-payload + fail-loud ───────────────────────────

def test_handle_source_workbook_scopes_from_payload(monkeypatch):
    """handle_source_workbook enters workspace_scope(payload['workspace_id'])
    before doing any work, and passes that workspace through to the engine."""
    import apps.api.services.workbook.source_engine as se

    seen = {}

    async def _fake_materialize(workbook_id, column_id, workspace_id):
        seen["scope"] = current_workspace_var.get()
        seen["arg"] = workspace_id
        return {"added": 0}

    monkeypatch.setattr(se, "materialize_source", _fake_materialize)

    asyncio.run(se.handle_source_workbook(1, {
        "workbook_id": "wb1", "column_id": "c1", "workspace_id": W1,
    }))
    assert seen["scope"] == W1
    assert seen["arg"] == W1


def test_handle_source_workbook_missing_ws_fails_loud(monkeypatch):
    import apps.api.services.workbook.source_engine as se

    called = {"n": 0}

    async def _fake_materialize(*a, **k):
        called["n"] += 1
        return {"added": 0}

    monkeypatch.setattr(se, "materialize_source", _fake_materialize)

    with pytest.raises(ValueError):
        asyncio.run(se.handle_source_workbook(1, {"workbook_id": "wb1", "column_id": "c1"}))
    assert called["n"] == 0, "must not process rows when workspace_id is missing"


def test_handle_run_workbook_scopes_from_payload(monkeypatch):
    import apps.api.services.workbook.enrichment as enr

    seen = {}

    async def _fake_run(**kwargs):
        seen["scope"] = current_workspace_var.get()
        seen["ws_kw"] = kwargs.get("workspace_id")
        return {"completed": 0}

    monkeypatch.setattr(enr, "run_workbook_enrichment", _fake_run)
    # provider pool sizing is best-effort; let it run (no-op) or be skipped.

    asyncio.run(enr.handle_run_workbook(1, {"workbook_id": "wb1", "workspace_id": W2}))
    assert seen["scope"] == W2
    assert seen["ws_kw"] == W2


def test_handle_run_workbook_missing_ws_fails_loud(monkeypatch):
    import apps.api.services.workbook.enrichment as enr

    async def _fake_run(**kwargs):
        raise AssertionError("should not be reached")

    monkeypatch.setattr(enr, "run_workbook_enrichment", _fake_run)
    with pytest.raises(ValueError):
        asyncio.run(enr.handle_run_workbook(1, {"workbook_id": "wb1"}))


def test_handle_refresh_workbook_scopes_and_fails_loud(monkeypatch, session_factory):
    import apps.api.services.workbook.refresh as rf
    _bind_sessionlocal(monkeypatch, session_factory, "apps.api.services.workbook.refresh")

    seen = {}

    async def _fake_refresh(workbook_id, reason="scheduled", workspace_id=None):
        seen["scope"] = current_workspace_var.get()
        seen["ws_kw"] = workspace_id
        return {}

    monkeypatch.setattr(rf, "refresh_workbook", _fake_refresh)

    # Seed the workbook so the post-refresh re-enqueue lookup finds it.
    s = session_factory()
    s.add(Workbook(id="wbR", name="r", workspace_id=W1, refresh_policy={}))
    s.commit(); s.close()

    asyncio.run(rf.handle_refresh_workbook(1, {"workbook_id": "wbR", "workspace_id": W1}))
    assert seen["scope"] == W1
    assert seen["ws_kw"] == W1

    with pytest.raises(ValueError):
        asyncio.run(rf.handle_refresh_workbook(1, {"workbook_id": "wbR"}))


# ── run_workbook_enrichment / materialize_source / refresh_workbook fail loud ──

def test_public_funcs_require_workspace(monkeypatch, session_factory):
    """The public entrypoints raise (don't run unscoped) when workspace_id is
    missing — the wrapper enters workspace_scope first."""
    import apps.api.services.workbook.enrichment as enr
    import apps.api.services.workbook.refresh as rf

    with pytest.raises(ValueError):
        asyncio.run(enr.run_workbook_enrichment("wb1"))  # workspace_id=None
    with pytest.raises(ValueError):
        asyncio.run(rf.refresh_workbook("wb1", workspace_id=None))


# ── signal_scan: per-workspace enumeration + stamped refresh payloads ──────────

def test_handle_signal_scan_enumerates_per_workspace(monkeypatch, session_factory):
    import apps.api.services.workbook.refresh as rf
    import apps.api.services.workspace.manager as ws_mgr
    _bind_sessionlocal(monkeypatch, session_factory, "apps.api.services.workbook.refresh")

    # run_signal_scan is exercised elsewhere; stub it out here.
    async def _fake_scan():
        return {"signals_found": 0}

    monkeypatch.setattr("apps.api.services.signals.monitor.run_signal_scan", _fake_scan)

    class _WS:
        def __init__(self, i): self.id = i; self.slug = i

    monkeypatch.setattr(ws_mgr, "list_workspaces", lambda: [_WS(W1), _WS(W2)])

    # Two workbooks subscribed to "hiring" in different tenants + one that isn't.
    s = session_factory()
    s.add(Workbook(id="wb1", name="a", workspace_id=W1,
                   refresh_policy={"enabled": True, "on_signal": ["hiring"]}))
    s.add(Workbook(id="wb2", name="b", workspace_id=W2,
                   refresh_policy={"enabled": True, "on_signal": ["hiring"]}))
    s.add(Workbook(id="wb3", name="c", workspace_id=W1,
                   refresh_policy={"enabled": True, "on_signal": ["funding"]}))
    s.commit(); s.close()

    asyncio.run(rf.handle_signal_scan(1, {"signal_types": ["hiring"]}))

    s = session_factory()
    refresh_jobs = s.query(Job).filter(Job.type == "refresh_workbook").all()
    by_wb = {(j.payload or {}).get("workbook_id"): (j.payload or {}) for j in refresh_jobs}
    s.close()

    # Exactly the two hiring-subscribed workbooks were triggered, each stamped
    # with ITS OWN workspace_id (proves per-workspace scoping, not a global scan).
    assert set(by_wb) == {"wb1", "wb2"}, by_wb
    assert by_wb["wb1"]["workspace_id"] == W1
    assert by_wb["wb2"]["workspace_id"] == W2
    assert all("reason" in p for p in by_wb.values())


def test_enqueue_helpers_stamp_workspace_id(monkeypatch, session_factory):
    import apps.api.services.workbook.refresh as rf
    _bind_sessionlocal(monkeypatch, session_factory, "apps.api.services.workbook.refresh")

    s = session_factory()
    rf._enqueue_next(s, "wbN", 60, W1)
    rf._enqueue_next_now(s, "wbM", "signal", W2)
    s.commit()
    jobs = {(j.payload or {}).get("workbook_id"): (j.payload or {}) for j in s.query(Job).all()}
    s.close()
    assert jobs["wbN"]["workspace_id"] == W1
    assert jobs["wbM"]["workspace_id"] == W2


def test_set_refresh_policy_stamps_workspace_from_workbook(monkeypatch, session_factory):
    import apps.api.services.workbook.refresh as rf
    _bind_sessionlocal(monkeypatch, session_factory, "apps.api.services.workbook.refresh")

    s = session_factory()
    s.add(Workbook(id="wbP", name="p", workspace_id=W2))
    s.commit()
    rf.set_refresh_policy(s, "wbP", {"enabled": True, "interval": "daily"})
    s.commit()
    job = s.query(Job).filter(Job.type == "refresh_workbook").first()
    payload = job.payload or {}
    s.close()
    assert payload["workspace_id"] == W2


# ── websocket authorizer uses the out-of-band workspace_id param (OD-5) ────────

def test_ws_authorize_uses_workspace_param(monkeypatch, session_factory):
    import apps.api.routers.workbooks as wbr
    import apps.api.services.workspace.manager as ws_mgr
    from apps.api.auth import create_access_token

    # _ws_authorize does a local `from apps.api.database import SessionLocal`, so
    # patch it at the source module.
    monkeypatch.setattr("apps.api.database.SessionLocal", session_factory, raising=True)
    monkeypatch.setattr(ws_mgr, "is_member", lambda ws, uid: ws == W1)

    s = session_factory()
    s.add(User(id=1, username="alice", is_active=True))
    s.add(Workbook(id="wbWS", name="ws", workspace_id=W1))
    s.commit(); s.close()

    token = create_access_token({"sub": "alice"})

    # No token / no workspace_id → denied (param is required + used).
    assert wbr._ws_authorize(None, "wbWS", W1) is False
    assert wbr._ws_authorize(token, "wbWS", None) is False

    # Correct workspace + member → authorized.
    assert wbr._ws_authorize(token, "wbWS", W1) is True

    # Wrong workspace (workbook belongs to W1, client claims W2) → denied.
    assert wbr._ws_authorize(token, "wbWS", W2) is False


# ── enqueue-site source regression guard (spec §9 test 11) ─────────────────────

def test_every_enqueue_site_stamps_workspace_id():
    """Each workbook-job enqueue site must include workspace_id in the payload.
    Source-level guard so a future edit that drops the stamp fails CI."""
    import apps.api.routers.workbooks as wbr
    import apps.api.routers.copilotkit as ck
    import apps.api.services.workbook.source_engine as se

    checks = [
        (wbr.run_workbook, "run_workbook enqueue"),
        (wbr.run_source_column, "source_workbook enqueue"),
        (wbr.refresh_now, "refresh_workbook enqueue"),
        (se.handle_source_workbook, "chained run_workbook enqueue"),
    ]
    for fn, label in checks:
        src = inspect.getsource(fn)
        assert "add_job" in src, f"{label}: no add_job in {fn.__name__}"
        assert "workspace_id" in src, f"{label}: payload missing workspace_id"

    # copilotkit create_source_workbook lives in a big dispatch; assert the
    # source_workbook add_job there carries workspace_id.
    ck_src = inspect.getsource(ck)
    idx = ck_src.find('"source_workbook"')
    assert idx != -1
    assert "workspace_id" in ck_src[idx:idx + 400], "copilotkit source_workbook missing workspace_id"
