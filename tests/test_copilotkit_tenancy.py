"""Offline tenancy tests for the legacy chat-agent tool path (copilotkit.py).

These run on the default SQLite backend (no Postgres, no network) and pin the
spec's app-layer invariants:

  * _execute_tool uses the INJECTED store — never constructs a bare LeadDB().
  * _resolve_chat_workspace: self-host returns the `main` workspace keyless;
    cloud (CHAT_REQUIRE_AUTH on) FAILS CLOSED — 401 with no/invalid auth.
  * Workbook ORM tools reject a workbook_id from another workspace_id
    (returns not-found, no existence leak / cross-tenant mutation).
  * execute_plan runs each step through the tenant-BOUND callback carrying the
    right workspace_id (Autopilot inherits the chat's tenant scope).

PG cross-tenant (RLS) coverage lives in tests/test_copilotkit_rls.py (gated).
"""
import os
import sys
import json
import asyncio

import pytest

os.environ.setdefault("DATABASE_URL", "sqlite:///./data/_pytest.db")

PROJECT_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
if PROJECT_ROOT not in sys.path:
    sys.path.insert(0, PROJECT_ROOT)

from apps.api.routers import copilotkit as ck  # noqa: E402


# ── _execute_tool uses the injected store, never a bare LeadDB() ──────────────

def test_execute_tool_uses_injected_store_not_leaddb(monkeypatch):
    """If _execute_tool constructed its own LeadDB() it would read the global,
    cross-tenant file. Monkeypatch the class to explode, then assert the tool
    runs purely off the injected store."""
    import apps.api.services.leadgen.db as dbmod

    def _boom(*a, **k):
        raise AssertionError("bare LeadDB() constructed in the chat tool path")

    monkeypatch.setattr(dbmod, "LeadDB", _boom)

    class _FakeStore:
        def __init__(self):
            self.calls = []

        def get_stats(self):
            self.calls.append("get_stats")
            return {"total": 7, "by_tier": {"hot": 1}}

        def get_leads(self, **kw):
            self.calls.append(("get_leads", kw))
            return []

    store = _FakeStore()
    out = json.loads(asyncio.run(
        ck._execute_tool("get_lead_stats", {}, store=store, workspace_id="W1", slug="s1")
    ))
    assert out == {"total": 7, "by_tier": {"hot": 1}}

    out2 = json.loads(asyncio.run(
        ck._execute_tool("search_leads", {"query": "x"}, store=store, workspace_id="W1", slug="s1")
    ))
    assert out2 == {"leads": [], "count": 0}
    assert "get_stats" in store.calls

    # _build_system_prompt must also read the tenant store, not a bare LeadDB().
    prompt = ck._build_system_prompt(store)
    assert "Total Leads:** 7" in prompt


# ── AC8: self-host `main` store == the legacy global config.DB_PATH ───────────

def test_self_host_main_store_is_legacy_db_path():
    """On SQLite, get_lead_store('main', 'main') must resolve to the SAME file as
    the legacy global config.DB_PATH, so existing self-host chat data doesn't
    vanish when chat moves onto the scoped store."""
    from apps.api.database import IS_SQLITE
    if not IS_SQLITE:
        pytest.skip("AC8 equivalence is the SQLite/self-host path")
    from apps.api.services.leadgen.store import get_lead_store
    from apps.api.services.leadgen.config import DB_PATH
    store = get_lead_store("main", "main")
    assert str(store.db_path) == str(DB_PATH)


# ── _resolve_chat_workspace: self-host keyless, cloud fail-closed ─────────────

def _req(headers=None):
    """Minimal stand-in for a Starlette Request exposing .headers."""
    from starlette.datastructures import Headers

    class _R:
        def __init__(self, h):
            self.headers = Headers(h or {})

    return _R(headers)


def test_resolve_chat_workspace_self_host_keyless(monkeypatch):
    monkeypatch.setattr(ck.settings, "CHAT_REQUIRE_AUTH", False)
    monkeypatch.setattr(ck.ws_manager, "_get_active_workspace_id", lambda: "ws-main")
    monkeypatch.setattr(ck.ws_manager, "workspace_slug", lambda wid: "main")

    ws_id, user_id, slug = ck._resolve_chat_workspace(_req())
    assert ws_id == "ws-main"
    assert user_id is None
    assert slug == "main"


def test_resolve_chat_workspace_cloud_no_auth_401(monkeypatch):
    from fastapi import HTTPException

    monkeypatch.setattr(ck.settings, "CHAT_REQUIRE_AUTH", True)
    with pytest.raises(HTTPException) as ei:
        ck._resolve_chat_workspace(_req())  # no Authorization header
    assert ei.value.status_code == 401


def test_resolve_chat_workspace_cloud_bad_token_401(monkeypatch):
    from fastapi import HTTPException

    monkeypatch.setattr(ck.settings, "CHAT_REQUIRE_AUTH", True)
    with pytest.raises(HTTPException) as ei:
        ck._resolve_chat_workspace(_req({"Authorization": "Bearer not-a-jwt"}))
    assert ei.value.status_code == 401


def test_resolve_chat_workspace_cloud_non_member_403(monkeypatch):
    """A valid token for a real user, but the requested workspace is not theirs."""
    from fastapi import HTTPException
    from jose import jwt

    monkeypatch.setattr(ck.settings, "CHAT_REQUIRE_AUTH", True)

    class _User:
        id = 42
        is_active = True

    class _Sess:
        def __enter__(self):
            return self

        def __exit__(self, *a):
            return False

        def query(self, *a):
            return self

        def filter(self, *a):
            return self

        def first(self):
            return _User()

    monkeypatch.setattr("apps.api.database.SessionLocal", lambda: _Sess())
    monkeypatch.setattr(ck.ws_manager, "is_member", lambda ws, uid: False)

    token = jwt.encode(
        {"sub": "alice"}, ck.settings.SECRET_KEY, algorithm=ck.settings.ALGORITHM
    )
    with pytest.raises(HTTPException) as ei:
        ck._resolve_chat_workspace(_req({
            "Authorization": f"Bearer {token}",
            "X-Workspace-Id": "ws-not-mine",
        }))
    assert ei.value.status_code == 403


# ── Workbook ORM tools reject a foreign workspace_id (IDOR blocked) ──────────

def test_add_agent_column_rejects_foreign_workbook(monkeypatch):
    """A W1-scoped call against a W2 workbook returns not-found and mutates
    nothing — app-layer filter is the only guard (workbooks have no RLS)."""
    from apps.api.services.workbook.models import Workbook

    captured = {"filters": None}

    class _Query:
        def filter(self, *crit):
            captured["filters"] = crit
            return self

        def first(self):
            # No row matches (id AND workspace_id) → cross-tenant miss.
            return None

    class _Sess:
        def __enter__(self):
            return self

        def __exit__(self, *a):
            return False

        def query(self, model):
            assert model is Workbook
            return _Query()

    monkeypatch.setattr("apps.api.database.SessionLocal", lambda: _Sess())

    out = json.loads(asyncio.run(ck._execute_tool(
        "add_agent_column",
        {"workbook_id": "wb-from-W2", "column_name": "Email",
         "goal": "find email", "target_field": "email"},
        store=object(), workspace_id="W1", slug="s1",
    )))
    assert out == {"error": "Workbook not found"}
    # The lookup must include a workspace_id filter (2 criteria: id + workspace).
    assert captured["filters"] is not None and len(captured["filters"]) == 2


# ── start_collection stamps workspace_id on the job (thread path) ────────────

def test_start_collection_stamps_workspace_on_job(monkeypatch, tmp_path):
    """The job row is created + stamped with workspace_id in the request thread
    (before the daemon worker thread starts), so sourced leads land in-tenant."""
    import threading
    import apps.api.services.leadgen.config as cfg
    import apps.api.services.leadgen.job_runner as jr
    from apps.api.services.leadgen.db import LeadDB

    # Redirect the leadgen job/lead file to a temp DB so we don't touch real data.
    db_file = str(tmp_path / "leads.db")
    monkeypatch.setattr(cfg, "DB_PATH", db_file)

    started = threading.Event()

    class _StubRunner:
        def __init__(self, *a, **k):
            pass

        async def _process_job(self, job):
            started.set()  # worker would source here — no-op in the test

    monkeypatch.setattr(jr, "JobRunner", _StubRunner)

    out = json.loads(asyncio.run(ck._execute_tool(
        "start_collection", {"query": "IT staffing in Pune"},
        store=object(), workspace_id="ws-tenant-9", slug="main",
    )))
    assert out["ok"] is True
    job_id = out["job_id"]

    # Job row exists + is stamped, independent of the worker thread.
    db = LeadDB(db_file)
    row = db.conn.execute(
        "SELECT workspace_id FROM jobs WHERE id = ?", (job_id,)
    ).fetchone()
    db.close()
    assert row is not None, "job row not created"
    assert row[0] == "ws-tenant-9", f"job not stamped: {row[0]!r}"


# ── execute_plan recurses through a tenant-bound callback ─────────────────────

def test_execute_plan_uses_tenant_bound_callback(monkeypatch):
    """draft→store→execute: execute_plan must receive a callback BOUND to the
    resolved workspace (store + workspace_id + slug) so the Autopilot recursion
    into _execute_tool cannot reach another tenant."""
    import functools
    from apps.api.services.agent import autopilot, autopilot_plan_store

    captured = {"cb": None}

    async def fake_execute_plan(plan, execute_tool):
        captured["cb"] = execute_tool
        return {"ok": True, "workbook_id": "wb-1"}

    monkeypatch.setattr(autopilot, "execute_plan", fake_execute_plan)

    sentinel_store = object()
    plan = {
        "goal": "g",
        "estimated_rows": 10,
        "steps": [
            {"kind": "create_source_workbook", "description": "c",
             "params": {"icp_description": "x"}},
        ],
    }
    plan_id, nonce = autopilot_plan_store.put("W7", None, plan)

    out = json.loads(asyncio.run(ck._execute_tool(
        "execute_plan", {"plan_id": plan_id, "nonce": nonce},
        store=sentinel_store, workspace_id="W7", slug="s7",
    )))
    assert out.get("workbook_id") == "wb-1"

    cb = captured["cb"]
    assert isinstance(cb, functools.partial), "execute_plan must get a bound partial"
    assert cb.func is ck._execute_tool
    assert cb.keywords == {"store": sentinel_store, "workspace_id": "W7", "slug": "s7"}
