"""MCP v2 write tools — offline gate tests (SQLite, no Postgres/network/SMTP).

Pins the v2 invariants on top of the Phase-2 guarantees already asserted in
tests/test_mcp_writes.py. Each v2 op is its own capability and stays behind BOTH
off-switches (``MCP_WRITE_ENABLED`` + the per-token grant):

  * ``create_automation`` (``automations:write``) — routes through the SAME
    ``routers.automations._validate_rule`` as REST (trigger/action/condition +
    the legacy-outreach 409); ADMIN-only; gated by ``AUTOMATIONS_ENABLED``.
  * ``send_email`` (``outreach:send``) — enqueues through the EXISTING send path
    (``actions._act_send_email``); refused (audited) when
    ``AUTOMATIONS_ALLOW_LEGACY_OUTREACH`` is off; ADMIN-only; never sends inline.
  * ``delete_lead`` / ``delete_workbook`` (``leads:delete`` / ``workbooks:delete``)
    — ADMIN-only, idempotent, a cross-tenant/unknown id is a no-op miss.

Every attempt + denial writes exactly one ``mcp_audit_log`` row. Cross-tenant
RLS proofs against a live Postgres live in tests/test_mcp_v2_rls.py.
"""
import os
import sys
import json
import asyncio

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
from apps.api.services.automations.models import TriggerCapReservation, Trigger  # noqa: E402
from apps.api.services.workbook.models import (  # noqa: E402
    Workbook, WorkbookEnrichment, WorkbookRow, WorkbookView,
)

WS = "ws_alpha"


def _run(coro):
    return asyncio.run(coro)


# ── fakes ─────────────────────────────────────────────────────────────────────

class _Lead:
    def __init__(self, **kw):
        self.__dict__.update(kw)


class _FakeLeadStore:
    def __init__(self):
        self.deleted = []
        self.leads = {}

    def get_lead(self, lead_id):
        return self.leads.get(lead_id)

    def delete_lead(self, lead_id):
        self.deleted.append(lead_id)
        self.leads.pop(lead_id, None)


# ── fixtures ──────────────────────────────────────────────────────────────────

@pytest.fixture()
def db(monkeypatch):
    eng = create_engine(
        "sqlite://", connect_args={"check_same_thread": False}, poolclass=StaticPool,
    )
    Base.metadata.create_all(eng, tables=[
        MCPToken.__table__, MCPAuditLog.__table__,
        TriggerCapReservation.__table__, Workbook.__table__, Trigger.__table__,
        WorkbookEnrichment.__table__, WorkbookRow.__table__, WorkbookView.__table__,
    ])
    SL = sessionmaker(bind=eng, autoflush=False)
    import apps.api.database as database
    monkeypatch.setattr(database, "SessionLocal", SL, raising=False)
    monkeypatch.setattr(mcp_tools.settings, "MCP_WRITE_ENABLED", True)
    monkeypatch.setattr(mcp_tools.settings, "MCP_MAX_WRITES_PER_DAY", 0)  # unlimited
    # v2 admin tools need an admin role by default; downgrade per-test as needed.
    monkeypatch.setattr(mcp_auth.ws_manager, "member_role", lambda ws, uid: "admin")
    # Automations master switch on; legacy outreach off by default (kill switch).
    monkeypatch.setattr(mcp_tools.settings, "AUTOMATIONS_ENABLED", True)
    monkeypatch.setattr(mcp_tools.settings, "AUTOMATIONS_ALLOW_LEGACY_OUTREACH", False)
    return SL


@pytest.fixture()
def store(monkeypatch):
    s = _FakeLeadStore()
    monkeypatch.setattr(mcp_tools, "get_lead_store", lambda ws, slug: s)
    return s


def _ctx(caps, *, ws=WS, user_id=7):
    return mcp_auth.MCPCtx(
        workspace_id=ws, slug=ws, capabilities=frozenset(caps),
        user_id=user_id, token_id="tok_test",
    )


def _exec(ctx, name, args):
    return json.loads(_run(mcp_tools.execute_tool(ctx, name, args)))


def _audits(SL, **filt):
    with SL() as s:
        q = s.query(MCPAuditLog)
        for k, v in filt.items():
            q = q.filter(getattr(MCPAuditLog, k) == v)
        return q.all()


# ══════════════════════════ create_automation ══════════════════════════════════

def test_create_automation_flag_off_denied_audited(db, store, monkeypatch):
    monkeypatch.setattr(mcp_tools.settings, "MCP_WRITE_ENABLED", False)
    ctx = _ctx([mcp_auth.CAP_AUTOMATIONS_WRITE])
    out = _exec(ctx, "create_automation", {"name": "R", "trigger_type": "on_row_added"})
    assert out == {"error": "mcp writes disabled"}
    rows = _audits(db, tool_name="create_automation")
    assert len(rows) == 1 and rows[0].result_status == "denied"


def test_create_automation_without_capability_denied(db, store):
    ctx = _ctx([mcp_auth.CAP_LEADS_WRITE])  # wrong cap
    out = _exec(ctx, "create_automation", {"name": "R", "trigger_type": "on_row_added"})
    assert out["error"] == "capability not granted"
    assert _audits(db, tool_name="create_automation")[0].result_status == "denied"


def test_create_automation_requires_admin(db, store, monkeypatch):
    monkeypatch.setattr(mcp_auth.ws_manager, "member_role", lambda ws, uid: "member")
    ctx = _ctx([mcp_auth.CAP_AUTOMATIONS_WRITE])
    out = _exec(ctx, "create_automation", {"name": "R", "trigger_type": "on_row_added"})
    assert out["error"] == "insufficient workspace role"
    assert _audits(db, tool_name="create_automation")[0].result_status == "denied"


def test_create_automation_disabled_flag_denied(db, store, monkeypatch):
    monkeypatch.setattr(mcp_tools.settings, "AUTOMATIONS_ENABLED", False)
    ctx = _ctx([mcp_auth.CAP_AUTOMATIONS_WRITE])
    out = _exec(ctx, "create_automation", {"name": "R", "trigger_type": "on_row_added"})
    assert out == {"error": "automations disabled"}
    assert _audits(db, tool_name="create_automation")[0].result_status == "denied"


def test_create_automation_happy_path_persists(db, store):
    ctx = _ctx([mcp_auth.CAP_AUTOMATIONS_WRITE])
    out = _exec(ctx, "create_automation", {
        "name": "Tag hot leads", "trigger_type": "on_row_added",
        "condition": "", "actions": [],
    })
    assert out["status"] == "created", out
    with db() as s:
        trig = s.query(Trigger).filter(Trigger.id == out["automation_id"]).first()
    assert trig is not None and trig.workspace_id == WS and trig.name == "Tag hot leads"
    rows = _audits(db, tool_name="create_automation")
    assert len(rows) == 1 and rows[0].result_status == "ok"


def test_create_automation_invalid_trigger_type_audited_error(db, store):
    ctx = _ctx([mcp_auth.CAP_AUTOMATIONS_WRITE])
    out = _exec(ctx, "create_automation", {"name": "R", "trigger_type": "bogus"})
    assert "invalid trigger_type" in out["error"]
    with db() as s:
        assert s.query(Trigger).count() == 0
    assert _audits(db, tool_name="create_automation")[0].result_status == "error"


def test_create_automation_legacy_outreach_action_409(db, store):
    # legacy outreach off → a send_email action is rejected by the SAME validation
    # the REST router uses (409 legacy_outreach_disabled). No rule is created.
    ctx = _ctx([mcp_auth.CAP_AUTOMATIONS_WRITE])
    out = _exec(ctx, "create_automation", {
        "name": "Blast", "trigger_type": "on_row_added",
        "actions": [{"type": "send_email", "config": {}}],
    })
    assert out["error"] == "legacy_outreach_disabled"
    assert out.get("status_code") == 409
    with db() as s:
        assert s.query(Trigger).count() == 0


# ══════════════════════════ send_email ═════════════════════════════════════════

def test_send_email_refused_when_legacy_outreach_off(db, store):
    ctx = _ctx([mcp_auth.CAP_OUTREACH_SEND])
    out = _exec(ctx, "send_email", {"lead_id": 1})
    assert out == {"error": "legacy_outreach_disabled"}
    rows = _audits(db, tool_name="send_email")
    assert len(rows) == 1 and rows[0].result_status == "denied"


def test_send_email_without_capability_denied(db, store, monkeypatch):
    monkeypatch.setattr(mcp_tools.settings, "AUTOMATIONS_ALLOW_LEGACY_OUTREACH", True)
    ctx = _ctx([mcp_auth.CAP_LEADS_WRITE])
    out = _exec(ctx, "send_email", {"lead_id": 1})
    assert out["error"] == "capability not granted"
    assert _audits(db, tool_name="send_email")[0].result_status == "denied"


def test_send_email_requires_admin(db, store, monkeypatch):
    monkeypatch.setattr(mcp_tools.settings, "AUTOMATIONS_ALLOW_LEGACY_OUTREACH", True)
    monkeypatch.setattr(mcp_auth.ws_manager, "member_role", lambda ws, uid: "member")
    ctx = _ctx([mcp_auth.CAP_OUTREACH_SEND])
    out = _exec(ctx, "send_email", {"lead_id": 1})
    assert out["error"] == "insufficient workspace role"
    assert _audits(db, tool_name="send_email")[0].result_status == "denied"


def test_send_email_routes_through_existing_send_path(db, store, monkeypatch):
    """Happy path enqueues via actions._act_send_email — NO parallel sender."""
    monkeypatch.setattr(mcp_tools.settings, "AUTOMATIONS_ALLOW_LEGACY_OUTREACH", True)
    store.leads[1] = _Lead(id=1, email="ok@x.com", company="Acme")

    calls = []
    from apps.api.services.automations import actions as actmod

    class _Res:
        status, summary, skip_reason, error = "success", "send enqueued", None, None

    def _spy(ws_id, cfg, lead_id, lead_data, idem):
        calls.append({"ws": ws_id, "cfg": cfg, "lead_id": lead_id, "lead_data": lead_data})
        return _Res()

    monkeypatch.setattr(actmod, "_act_send_email", _spy)
    ctx = _ctx([mcp_auth.CAP_OUTREACH_SEND])
    out = _exec(ctx, "send_email", {"lead_id": 1, "subject": "Hi", "body_html": "<p>x</p>"})
    assert out["status"] == "success"
    assert len(calls) == 1
    # The workspace is the AUTHENTICATED token's, the lead's email is resolved
    # from the scoped store, and the cfg carries the message.
    assert calls[0]["ws"] == WS
    assert calls[0]["lead_data"]["email"] == "ok@x.com"
    assert calls[0]["cfg"]["subject"] == "Hi"
    assert _audits(db, tool_name="send_email")[0].result_status == "ok"


def test_send_email_cross_tenant_lead_is_skipped(db, store, monkeypatch):
    """A lead_id not in the token's workspace resolves to nothing → no email →
    skipped (the real _act_send_email short-circuits before any send)."""
    monkeypatch.setattr(mcp_tools.settings, "AUTOMATIONS_ALLOW_LEGACY_OUTREACH", True)
    # store has no lead 999 (simulates a cross-tenant / RLS miss).
    ctx = _ctx([mcp_auth.CAP_OUTREACH_SEND])
    out = _exec(ctx, "send_email", {"lead_id": 999})
    assert out["status"] == "skipped"
    assert out["skip_reason"] == "no_email"


def test_send_email_idempotent_replay(db, store, monkeypatch):
    monkeypatch.setattr(mcp_tools.settings, "AUTOMATIONS_ALLOW_LEGACY_OUTREACH", True)
    store.leads[1] = _Lead(id=1, email="ok@x.com")
    from apps.api.services.automations import actions as actmod

    class _Res:
        status, summary, skip_reason, error = "success", "send enqueued", None, None

    n = []
    monkeypatch.setattr(actmod, "_act_send_email",
                        lambda *a, **k: (n.append(1), _Res())[1])
    ctx = _ctx([mcp_auth.CAP_OUTREACH_SEND])
    a = _exec(ctx, "send_email", {"lead_id": 1, "idempotency_key": "s1"})
    b = _exec(ctx, "send_email", {"lead_id": 1, "idempotency_key": "s1"})
    assert a["status"] == "success"
    assert b["status"] == "duplicate"
    assert len(n) == 1  # the second call did NOT re-enqueue


# ══════════════════════════ delete_lead ════════════════════════════════════════

def test_delete_lead_requires_admin(db, store, monkeypatch):
    monkeypatch.setattr(mcp_auth.ws_manager, "member_role", lambda ws, uid: "member")
    store.leads[5] = _Lead(id=5)
    ctx = _ctx([mcp_auth.CAP_LEADS_DELETE])
    out = _exec(ctx, "delete_lead", {"lead_id": 5})
    assert out["error"] == "insufficient workspace role"
    assert store.deleted == []
    assert _audits(db, tool_name="delete_lead")[0].result_status == "denied"


def test_delete_lead_without_capability_denied(db, store):
    ctx = _ctx([mcp_auth.CAP_LEADS_WRITE])  # write != delete
    out = _exec(ctx, "delete_lead", {"lead_id": 5})
    assert out["error"] == "capability not granted"
    assert store.deleted == []


def test_delete_lead_happy_and_idempotent(db, store):
    store.leads[5] = _Lead(id=5)
    ctx = _ctx([mcp_auth.CAP_LEADS_DELETE])
    out1 = _exec(ctx, "delete_lead", {"lead_id": 5})
    assert out1 == {"status": "deleted", "lead_id": 5}
    assert store.deleted == [5]
    # Re-delete (no key) → idempotent no-op miss, not an error/crash.
    out2 = _exec(ctx, "delete_lead", {"lead_id": 5})
    assert out2 == {"status": "not_found", "lead_id": 5}
    assert store.deleted == [5]  # second call did NOT re-delete
    assert len(_audits(db, tool_name="delete_lead")) == 2


def test_delete_lead_cross_tenant_unknown_is_no_op(db, store):
    ctx = _ctx([mcp_auth.CAP_LEADS_DELETE])
    out = _exec(ctx, "delete_lead", {"lead_id": 12345})
    assert out["status"] == "not_found"
    assert store.deleted == []


# ══════════════════════════ delete_workbook ════════════════════════════════════

def test_delete_workbook_happy(db, store):
    with db() as s, s.begin():
        s.add(Workbook(id="wb1", name="W", workspace_id=WS, source_type="empty",
                       source_config={}, filter_criteria={}, columns_config=[]))
    ctx = _ctx([mcp_auth.CAP_WORKBOOKS_DELETE])
    out = _exec(ctx, "delete_workbook", {"workbook_id": "wb1"})
    assert out == {"status": "deleted", "workbook_id": "wb1"}
    with db() as s:
        assert s.query(Workbook).filter(Workbook.id == "wb1").first() is None
    assert _audits(db, tool_name="delete_workbook")[0].result_status == "ok"


def test_delete_workbook_cross_tenant_is_no_op(db, store):
    # A workbook owned by another workspace is invisible (filtered by ws_id) → miss.
    with db() as s, s.begin():
        s.add(Workbook(id="wb_other", name="W", workspace_id="ws_beta",
                       source_type="empty", source_config={}, filter_criteria={},
                       columns_config=[]))
    ctx = _ctx([mcp_auth.CAP_WORKBOOKS_DELETE])
    out = _exec(ctx, "delete_workbook", {"workbook_id": "wb_other"})
    assert out["status"] == "not_found"
    with db() as s:  # untouched
        assert s.query(Workbook).filter(Workbook.id == "wb_other").first() is not None


def test_delete_workbook_requires_admin(db, store, monkeypatch):
    monkeypatch.setattr(mcp_auth.ws_manager, "member_role", lambda ws, uid: "member")
    ctx = _ctx([mcp_auth.CAP_WORKBOOKS_DELETE])
    out = _exec(ctx, "delete_workbook", {"workbook_id": "wb1"})
    assert out["error"] == "insufficient workspace role"


# ══════════════════════════ catalog visibility ═════════════════════════════════

def test_tools_list_shows_v2_tools_with_flag_and_caps(db):
    ctx = _ctx([
        mcp_auth.CAP_AUTOMATIONS_WRITE, mcp_auth.CAP_OUTREACH_SEND,
        mcp_auth.CAP_LEADS_DELETE, mcp_auth.CAP_WORKBOOKS_DELETE,
    ])
    names = {t["name"] for t in mcp_tools.list_tools(ctx)}
    assert {"create_automation", "send_email", "delete_lead", "delete_workbook"} <= names


def test_tools_list_hides_v2_tools_without_caps(db):
    ctx = _ctx([mcp_auth.CAP_LEADS_READ])
    names = {t["name"] for t in mcp_tools.list_tools(ctx)}
    assert not ({"create_automation", "send_email", "delete_lead", "delete_workbook"} & names)


def test_tools_list_hides_v2_tools_when_flag_off(db, monkeypatch):
    monkeypatch.setattr(mcp_tools.settings, "MCP_WRITE_ENABLED", False)
    ctx = _ctx([mcp_auth.CAP_AUTOMATIONS_WRITE, mcp_auth.CAP_OUTREACH_SEND,
                mcp_auth.CAP_LEADS_DELETE, mcp_auth.CAP_WORKBOOKS_DELETE])
    names = {t["name"] for t in mcp_tools.list_tools(ctx)}
    assert not ({"create_automation", "send_email", "delete_lead", "delete_workbook"} & names)
