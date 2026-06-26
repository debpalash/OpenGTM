"""MCP Phase-1 security — offline unit tests (SQLite, no Postgres/network).

Pins the spec's auth + default-safe invariants for the MCP bridge:

  * Unauthenticated / invalid / revoked / expired tokens are rejected (cloud).
  * A token is bound to ONE workspace + a capability set; a user removed from the
    workspace after mint is denied (live membership re-check).
  * tools/list is filtered to the token's capabilities; write tools stay hidden
    (and inert) while MCP_WRITE_ENABLED is False — two independent off-switches.
  * No tool constructs a bare LeadDB() — reads route through the scoped store.
  * Self-host (MCP_REQUIRE_AUTH false) stays keyless on the `main` workspace.
  * The SSE/HTTP transport rejects an unauthenticated /message with 401.

PG cross-tenant (RLS) coverage lives in tests/test_mcp_rls.py (gated).
"""
import os
import sys
import json
import asyncio
from datetime import datetime, timedelta, timezone

import pytest
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

os.environ.setdefault("DATABASE_URL", "sqlite:///./data/_pytest.db")

PROJECT_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
if PROJECT_ROOT not in sys.path:
    sys.path.insert(0, PROJECT_ROOT)

from apps.api.database import Base  # noqa: E402
from apps.api.services.mcp import auth as mcp_auth  # noqa: E402
from apps.api.services.mcp import tools as mcp_tools  # noqa: E402
from apps.api.services.mcp.models import MCPToken, MCPAuditLog  # noqa: E402

WS = "ws_alpha"
WS2 = "ws_beta"


def _run(coro):
    return asyncio.run(coro)


@pytest.fixture()
def Session(monkeypatch):
    """In-memory SQLite with the MCP tables, bound into database.SessionLocal."""
    eng = create_engine(
        "sqlite://", connect_args={"check_same_thread": False}, poolclass=StaticPool,
    )
    Base.metadata.create_all(eng, tables=[MCPToken.__table__, MCPAuditLog.__table__])
    SL = sessionmaker(bind=eng, autoflush=False)
    import apps.api.database as database
    monkeypatch.setattr(database, "SessionLocal", SL, raising=False)
    return SL


@pytest.fixture()
def cloud(monkeypatch):
    """Force the cloud (auth-required) posture + a member-of-WS user."""
    monkeypatch.setattr(mcp_auth.settings, "MCP_REQUIRE_AUTH", True)
    monkeypatch.setattr(mcp_auth.ws_manager, "is_member", lambda ws, uid: ws == WS)
    monkeypatch.setattr(mcp_auth.ws_manager, "member_role", lambda ws, uid: "admin")
    monkeypatch.setattr(mcp_auth.ws_manager, "workspace_slug", lambda ws: ws)


def _mint(caps, *, ttl_days=None, user_id=7, workspace_id=WS):
    raw, tok = mcp_auth.create_token(
        user_id=user_id, workspace_id=workspace_id, capabilities=caps,
        name="t", ttl_days=ttl_days,
    )
    return raw, tok


# ── token minting: hash stored, plaintext never persisted (AC10) ──────────────

def test_create_token_stores_hash_not_plaintext(Session, cloud):
    raw, tok = _mint([mcp_auth.CAP_LEADS_READ])
    assert raw.startswith("ycp_")
    with Session() as s:
        row = s.query(MCPToken).filter(MCPToken.id == tok.id).first()
        assert row.token_hash == mcp_auth.hash_token(raw)
        assert raw not in (row.token_hash, row.prefix)
        assert row.workspace_id == WS
        assert row.capabilities == [mcp_auth.CAP_LEADS_READ]


# ── resolution: valid / missing / invalid / revoked / expired ─────────────────

def test_resolve_valid_token(Session, cloud):
    raw, _ = _mint([mcp_auth.CAP_LEADS_READ])
    ctx = mcp_auth.resolve_mcp_token(raw)
    assert ctx.workspace_id == WS
    assert ctx.user_id == 7
    assert ctx.has(mcp_auth.CAP_LEADS_READ)


def test_resolve_missing_token_rejected(Session, cloud):
    with pytest.raises(mcp_auth.MCPAuthError):
        mcp_auth.resolve_mcp_token(None)


def test_resolve_invalid_token_rejected(Session, cloud):
    with pytest.raises(mcp_auth.MCPAuthError):
        mcp_auth.resolve_mcp_token("ycp_not_a_real_token")


def test_resolve_revoked_token_rejected(Session, cloud):
    raw, tok = _mint([mcp_auth.CAP_LEADS_READ])
    with Session() as s, s.begin():
        s.query(MCPToken).filter(MCPToken.id == tok.id).update(
            {"revoked_at": datetime.now(timezone.utc)}
        )
    with pytest.raises(mcp_auth.MCPAuthError):
        mcp_auth.resolve_mcp_token(raw)


def test_resolve_expired_token_rejected(Session, cloud):
    raw, tok = _mint([mcp_auth.CAP_LEADS_READ])
    with Session() as s, s.begin():
        s.query(MCPToken).filter(MCPToken.id == tok.id).update(
            {"expires_at": datetime.now(timezone.utc) - timedelta(days=1)}
        )
    with pytest.raises(mcp_auth.MCPAuthError):
        mcp_auth.resolve_mcp_token(raw)


def test_resolve_non_member_rejected(Session, cloud, monkeypatch):
    """User removed from the workspace after mint → live re-check denies."""
    raw, _ = _mint([mcp_auth.CAP_LEADS_READ])
    monkeypatch.setattr(mcp_auth.ws_manager, "is_member", lambda ws, uid: False)
    with pytest.raises(mcp_auth.MCPAuthError):
        mcp_auth.resolve_mcp_token(raw)


def test_resolve_updates_last_used(Session, cloud):
    raw, tok = _mint([mcp_auth.CAP_LEADS_READ])
    mcp_auth.resolve_mcp_token(raw)
    with Session() as s:
        row = s.query(MCPToken).filter(MCPToken.id == tok.id).first()
        assert row.last_used_at is not None


# ── self-host keyless ─────────────────────────────────────────────────────────

def test_self_host_keyless(monkeypatch):
    monkeypatch.setattr(mcp_auth.settings, "MCP_REQUIRE_AUTH", False)
    monkeypatch.setattr(mcp_auth.ws_manager, "_get_active_workspace_id", lambda: "ws-main")
    monkeypatch.setattr(mcp_auth.ws_manager, "workspace_slug", lambda ws: "main")
    ctx = mcp_auth.resolve_mcp_token(None)  # no token needed
    assert ctx.workspace_id == "ws-main"
    assert ctx.user_id is None
    assert ctx.has(mcp_auth.CAP_LEADS_READ)


# ── tools/list capability + write-flag filtering (AC1) ────────────────────────

def _ctx(caps):
    return mcp_auth.MCPCtx(workspace_id=WS, slug=WS, capabilities=frozenset(caps), user_id=7)


def test_tools_list_read_only_hides_write_tools(monkeypatch):
    monkeypatch.setattr(mcp_tools.settings, "MCP_WRITE_ENABLED", False)
    names = {t["name"] for t in mcp_tools.list_tools(_ctx([mcp_auth.CAP_LEADS_READ]))}
    assert "find_leads" in names
    assert "create_workbook" not in names  # write-gated, flag off


def test_tools_list_write_tool_hidden_until_flag(monkeypatch):
    """Even WITH the write cap, the write tool stays hidden while the flag is off."""
    monkeypatch.setattr(mcp_tools.settings, "MCP_WRITE_ENABLED", False)
    names = {t["name"] for t in mcp_tools.list_tools(
        _ctx([mcp_auth.CAP_LEADS_READ, mcp_auth.CAP_WORKBOOKS_WRITE]))}
    assert "create_workbook" not in names
    # Flag on + cap held → now visible.
    monkeypatch.setattr(mcp_tools.settings, "MCP_WRITE_ENABLED", True)
    names2 = {t["name"] for t in mcp_tools.list_tools(
        _ctx([mcp_auth.CAP_LEADS_READ, mcp_auth.CAP_WORKBOOKS_WRITE]))}
    assert "create_workbook" in names2


def test_tools_list_no_cap_sees_nothing(monkeypatch):
    assert mcp_tools.list_tools(_ctx([])) == []


# ── execute_tool gates: capability + write flag (AC1) ─────────────────────────

def test_execute_without_capability_denied(monkeypatch):
    out = json.loads(_run(mcp_tools.execute_tool(_ctx([]), "find_leads", {"query": "x"})))
    assert out["error"] == "capability not granted"


def test_execute_write_tool_disabled_by_default(monkeypatch):
    monkeypatch.setattr(mcp_tools.settings, "MCP_WRITE_ENABLED", False)
    ctx = _ctx([mcp_auth.CAP_WORKBOOKS_WRITE])
    out = json.loads(_run(mcp_tools.execute_tool(ctx, "create_workbook", {"description": "x"})))
    assert out == {"error": "mcp writes disabled"}


# ── reads route through the scoped store, never a bare LeadDB() (AC2) ──────────

def test_execute_uses_scoped_store_not_leaddb(monkeypatch):
    import apps.api.services.leadgen.db as dbmod

    def _boom(*a, **k):
        raise AssertionError("bare LeadDB() constructed in the MCP tool path")

    monkeypatch.setattr(dbmod, "LeadDB", _boom)

    class _Lead:
        id, company, city, email, phone, score = 1, "Alpha", "SF", "a@x.com", "", 80
        score_tier, status, website, specialization = "hot", "new", "x", "y"
        company_size, contact_person = "10", "Jo"

    class _FakeStore:
        def __init__(self):
            self.calls = []

        def get_leads(self, **kw):
            self.calls.append(kw)
            return [_Lead()]

    store = _FakeStore()
    monkeypatch.setattr(mcp_tools, "get_lead_store", lambda ws, slug: store)

    out = json.loads(_run(mcp_tools.execute_tool(
        _ctx([mcp_auth.CAP_LEADS_READ]), "find_leads", {"query": "alpha", "limit": 5})))
    assert out["count"] == 1
    assert out["leads"][0]["company"] == "Alpha"
    assert store.calls and store.calls[0]["search"] == "alpha"


# ── SSE transport rejects unauthenticated /message with 401 (AC8) ─────────────

def test_sse_message_unauthenticated_401(monkeypatch):
    from fastapi.testclient import TestClient
    import apps.mcp.server as server

    monkeypatch.setattr(server, "resolve_mcp_token", lambda raw: (_ for _ in ()).throw(
        mcp_auth.MCPAuthError("missing MCP token")) if not raw else _ctx([mcp_auth.CAP_LEADS_READ]))

    client = TestClient(server.build_sse_app())
    # No Authorization header → 401, never reaches a tool.
    resp = client.post("/message", json={"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
    assert resp.status_code == 401


def test_sse_message_authenticated_lists_tools(monkeypatch):
    from fastapi.testclient import TestClient
    import apps.mcp.server as server

    monkeypatch.setattr(server, "resolve_mcp_token",
                        lambda raw: _ctx([mcp_auth.CAP_LEADS_READ]) if raw else
                        (_ for _ in ()).throw(mcp_auth.MCPAuthError("missing")))
    monkeypatch.setattr(mcp_tools.settings, "MCP_WRITE_ENABLED", False)

    client = TestClient(server.build_sse_app())
    resp = client.post(
        "/message",
        headers={"Authorization": "Bearer ycp_valid"},
        json={"jsonrpc": "2.0", "id": 1, "method": "tools/list"},
    )
    assert resp.status_code == 200
    names = {t["name"] for t in resp.json()["result"]["tools"]}
    assert "find_leads" in names and "create_workbook" not in names
