"""MCP Phase-2 write tools — offline gate tests (SQLite, no Postgres/network).

Pins the write-path invariants from docs/specs/research-write-capable-mcp-spec.md:

  * Two independent off-switches: ``MCP_WRITE_ENABLED`` AND the per-token cap.
  * A LIVE workspace-role re-check denies a downgraded user (privilege-freeze).
  * A per-workspace daily write cap (reusing the automations reservation ledger)
    refuses + audits the over-cap write.
  * Idempotency: a retried write with the same key is a no-op (no double-apply).
  * Exactly one ``mcp_audit_log`` row per write attempt, INCLUDING every denial
    (capability / flag / role / cap / error).
  * The workspace comes from the AUTHENTICATED token — args naming another
    workspace are ignored (confused-deputy defense).
  * ``enroll_leads`` preserves suppression skip + consent recording.
  * ``create_workbook`` persists a real RLS-scoped workbook (not a stub).

Cross-tenant RLS write proofs against a live Postgres live in
tests/test_mcp_rls.py (gated on TEST_DATABASE_URL).
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
from apps.api.services.automations.models import TriggerCapReservation  # noqa: E402
from apps.api.services.workbook.models import Workbook  # noqa: E402

WS = "ws_alpha"


def _run(coro):
    return asyncio.run(coro)


# ── fakes ─────────────────────────────────────────────────────────────────────

class _Lead:
    def __init__(self, **kw):
        self.__dict__.update(kw)


class _FakeLeadStore:
    """Records calls; stands in for the RLS-scoped PgLeadStore (offline)."""

    def __init__(self):
        self.upserts = []
        self.updates = []
        self.statuses = []
        self.leads = {}

    def upsert_lead(self, lead):
        self.upserts.append(lead)
        lead.id = 101
        self.leads[101] = lead
        return 101

    def get_lead(self, lead_id):
        return self.leads.get(lead_id)

    def update_lead_fields(self, lead_id, fields):
        self.updates.append((lead_id, dict(fields)))

    def update_status(self, lead_id, status, note=""):
        self.statuses.append((lead_id, status))


class _FakeOutreachStore:
    def __init__(self, *, exists=True, suppressed=frozenset()):
        self._exists = exists
        self._suppressed = set(suppressed)
        self.enrolled = []

    def sequence_exists(self, seq_id):
        return self._exists

    def is_suppressed(self, email):
        return email in self._suppressed

    def enroll(self, seq_id, lead_id, email, consent_source="", consent_at=None):
        self.enrolled.append({
            "seq_id": seq_id, "lead_id": lead_id, "email": email,
            "consent_source": consent_source, "consent_at": consent_at,
        })
        return len(self.enrolled)


# ── fixtures ──────────────────────────────────────────────────────────────────

@pytest.fixture()
def db(monkeypatch):
    """In-memory SQLite with the MCP + cap + workbook tables, write flag ON."""
    eng = create_engine(
        "sqlite://", connect_args={"check_same_thread": False}, poolclass=StaticPool,
    )
    Base.metadata.create_all(eng, tables=[
        MCPToken.__table__, MCPAuditLog.__table__,
        TriggerCapReservation.__table__, Workbook.__table__,
    ])
    SL = sessionmaker(bind=eng, autoflush=False)
    import apps.api.database as database
    monkeypatch.setattr(database, "SessionLocal", SL, raising=False)
    # Writes enabled (the OFF case is asserted explicitly below).
    monkeypatch.setattr(mcp_tools.settings, "MCP_WRITE_ENABLED", True)
    monkeypatch.setattr(mcp_tools.settings, "MCP_MAX_WRITES_PER_DAY", 0)  # unlimited
    # Live role re-check passes by default (member); tests override to downgrade.
    monkeypatch.setattr(mcp_auth.ws_manager, "member_role", lambda ws, uid: "member")
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


# ── off-switch #1: global flag ────────────────────────────────────────────────

def test_write_disabled_when_flag_off(db, store, monkeypatch):
    monkeypatch.setattr(mcp_tools.settings, "MCP_WRITE_ENABLED", False)
    ctx = _ctx([mcp_auth.CAP_LEADS_WRITE])
    out = _exec(ctx, "create_lead", {"company": "Acme"})
    assert out == {"error": "mcp writes disabled"}
    assert store.upserts == []
    # Denial is audited.
    rows = _audits(db, tool_name="create_lead")
    assert len(rows) == 1 and rows[0].result_status == "denied"


# ── off-switch #2: per-token capability ───────────────────────────────────────

def test_write_without_capability_denied_and_audited(db, store):
    ctx = _ctx([mcp_auth.CAP_LEADS_READ])  # no leads:write
    out = _exec(ctx, "create_lead", {"company": "Acme"})
    assert out["error"] == "capability not granted"
    assert store.upserts == []
    rows = _audits(db, tool_name="create_lead")
    assert len(rows) == 1 and rows[0].result_status == "denied"


# ── live role re-check (privilege-freeze defense) ─────────────────────────────

def test_role_downgrade_denies_write(db, store, monkeypatch):
    monkeypatch.setattr(mcp_auth.ws_manager, "member_role", lambda ws, uid: "viewer")
    ctx = _ctx([mcp_auth.CAP_LEADS_WRITE])
    out = _exec(ctx, "create_lead", {"company": "Acme"})
    assert out["error"] == "insufficient workspace role"
    assert store.upserts == []
    rows = _audits(db, tool_name="create_lead")
    assert len(rows) == 1 and rows[0].result_status == "denied"


# ── create_lead happy path + audit (AC4) ──────────────────────────────────────

def test_create_lead_writes_and_audits(db, store):
    ctx = _ctx([mcp_auth.CAP_LEADS_WRITE])
    out = _exec(ctx, "create_lead", {"company": "Acme", "email": "a@acme.com"})
    assert out["status"] == "created" and out["lead_id"] == 101
    assert len(store.upserts) == 1
    rows = _audits(db, tool_name="create_lead")
    assert len(rows) == 1
    r = rows[0]
    assert r.result_status == "ok"
    assert r.token_id == "tok_test"
    assert r.workspace_id == WS
    # email is redacted in the audit args.
    assert r.arguments_redacted.get("email") == "***redacted***"


# ── workspace comes from the token, NOT args (confused-deputy) ────────────────

def test_create_lead_ignores_workspace_in_args(db, store):
    ctx = _ctx([mcp_auth.CAP_LEADS_WRITE])
    _exec(ctx, "create_lead", {"company": "Acme", "workspace_id": "ws_evil"})
    assert len(store.upserts) == 1
    # workspace_id from args is dropped by the allowlist; the Lead never carries it.
    assert getattr(store.upserts[0], "workspace_id", "") in ("", None)


# ── update_lead: cross-tenant / unknown id is a miss, no write ─────────────────

def test_update_lead_unknown_id_not_found(db, store):
    ctx = _ctx([mcp_auth.CAP_LEADS_WRITE])
    out = _exec(ctx, "update_lead", {"lead_id": 999, "city": "SF"})
    assert out == {"error": "Lead not found"}
    assert store.updates == []


def test_update_lead_updates_allowlisted_fields(db, store):
    store.leads[5] = _Lead(id=5, email="old@x.com")
    ctx = _ctx([mcp_auth.CAP_LEADS_WRITE])
    out = _exec(ctx, "update_lead", {"lead_id": 5, "city": "SF", "status": "qualified",
                                     "id": 999, "score": 100})
    assert out["status"] == "updated"
    # id/score are NOT in the allowlist → never written.
    assert store.updates == [(5, {"city": "SF"})]
    assert store.statuses == [(5, "qualified")]


# ── idempotency: a retried write does not double-apply (AC5) ──────────────────

def test_idempotent_create_no_double_apply(db, store):
    ctx = _ctx([mcp_auth.CAP_LEADS_WRITE])
    a = _exec(ctx, "create_lead", {"company": "Acme", "idempotency_key": "k1"})
    b = _exec(ctx, "create_lead", {"company": "Acme", "idempotency_key": "k1"})
    assert a["status"] == "created"
    assert b["status"] == "duplicate"
    assert len(store.upserts) == 1  # second call did NOT re-create
    # One reservation row for the key; both attempts audited.
    with db() as s:
        res = s.query(TriggerCapReservation).filter(
            TriggerCapReservation.idempotency_key == "k1").all()
    assert len(res) == 1
    assert len(_audits(db, tool_name="create_lead")) == 2


# ── daily write cap refuses + audits the over-cap write (AC6) ─────────────────

def test_daily_write_cap_enforced(db, store, monkeypatch):
    monkeypatch.setattr(mcp_tools.settings, "MCP_MAX_WRITES_PER_DAY", 2)
    ctx = _ctx([mcp_auth.CAP_LEADS_WRITE])
    assert _exec(ctx, "create_lead", {"company": "A"})["status"] == "created"
    assert _exec(ctx, "create_lead", {"company": "B"})["status"] == "created"
    capped = _exec(ctx, "create_lead", {"company": "C"})
    assert capped == {"error": "mcp write cap exceeded"}
    assert len(store.upserts) == 2  # the 3rd write never ran
    assert any(r.result_status == "capped" for r in _audits(db, tool_name="create_lead"))


# ── create_workbook persists a real workbook (not a stub) ─────────────────────

def test_create_workbook_persists(db):
    ctx = _ctx([mcp_auth.CAP_WORKBOOKS_WRITE])
    out = _exec(ctx, "create_workbook", {"description": "SaaS CTOs in SF with email"})
    assert out["status"] == "created"
    with db() as s:
        wb = s.query(Workbook).filter(Workbook.id == out["workbook_id"]).first()
    assert wb is not None
    assert wb.workspace_id == WS
    assert wb.source_type == "empty"
    assert any(c["id"] == "email" for c in wb.columns_config)


# ── enroll: suppression skip + consent recording (AC7) ────────────────────────

def test_enroll_skips_suppressed_records_consent(db, store, monkeypatch):
    store.leads = {
        1: _Lead(id=1, email="ok@x.com"),
        2: _Lead(id=2, email="supp@x.com"),
        3: _Lead(id=3, email=""),  # no email
    }
    ostore = _FakeOutreachStore(exists=True, suppressed={"supp@x.com"})
    import apps.api.services.outreach.store as ostore_mod
    monkeypatch.setattr(ostore_mod, "get_outreach_store", lambda ws: ostore)

    ctx = _ctx([mcp_auth.CAP_SEQUENCES_ENROLL])
    out = _exec(ctx, "enroll_leads", {"sequence_id": "seq1", "lead_ids": [1, 2, 3]})
    assert out["enrolled"] == 1
    reasons = {s["lead_id"]: s["reason"] for s in out["skipped"]}
    assert reasons == {2: "suppressed", 3: "no_email"}
    assert ostore.enrolled[0]["consent_source"] == "mcp_enroll"
    assert ostore.enrolled[0]["consent_at"] is not None


def test_enroll_sequence_not_in_workspace(db, store, monkeypatch):
    ostore = _FakeOutreachStore(exists=False)
    import apps.api.services.outreach.store as ostore_mod
    monkeypatch.setattr(ostore_mod, "get_outreach_store", lambda ws: ostore)
    ctx = _ctx([mcp_auth.CAP_SEQUENCES_ENROLL])
    out = _exec(ctx, "enroll_leads", {"sequence_id": "seq_from_other_ws", "lead_ids": [1]})
    assert out == {"error": "Sequence not found"}
    assert ostore.enrolled == []


# ── tools/list exposes the write tools with the flag on + caps ────────────────

def test_tools_list_shows_write_tools_with_flag_and_caps(db):
    ctx = _ctx([
        mcp_auth.CAP_LEADS_READ, mcp_auth.CAP_LEADS_WRITE,
        mcp_auth.CAP_SEQUENCES_ENROLL, mcp_auth.CAP_WORKBOOKS_WRITE,
    ])
    names = {t["name"] for t in mcp_tools.list_tools(ctx)}
    assert {"create_lead", "update_lead", "enroll_leads", "create_workbook"} <= names
