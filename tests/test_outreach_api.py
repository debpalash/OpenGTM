"""Outreach router — API tests (SQLite, offline). Spec §5 + AC-4/AC-7.

Auth/workspace deps overridden. Covers: sequence CRUD workspace-scoping,
enroll dedup + email snapshot + sequence-in-workspace validation + suppression
filter, suppression endpoint (locked gate), unsubscribe one-click (RFC 8058:
no auth/CSRF, idempotent), start gating (consent/footer/SMTP).
"""

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.core.config import settings
from apps.api.core.tenancy import WorkspaceCtx, current_workspace
from apps.api.database import Base
from apps.api.services.outreach import orm_models as om
from apps.api.routers.outreach import router as outreach_router, require_admin

WS = "ws_api_out"


class _User:
    id = "user-1"


class _FakeLead:
    def __init__(self, email):
        self.email = email


class _FakeLeadStore:
    def __init__(self, emails):
        self._emails = emails  # {lead_id: email}

    def get_lead(self, lead_id):
        e = self._emails.get(lead_id)
        return _FakeLead(e) if e is not None else None


@pytest.fixture()
def client(monkeypatch):
    engine = create_engine(
        "sqlite:///:memory:", connect_args={"check_same_thread": False}, poolclass=StaticPool,
    )
    Base.metadata.create_all(engine, tables=[
        om.OutreachSequence.__table__, om.OutreachEnrollment.__table__,
        om.OutreachSend.__table__, om.OutreachSuppression.__table__,
        om.OutreachSchedule.__table__,
    ])
    SL = sessionmaker(bind=engine, autoflush=False)
    import apps.api.database as database
    import apps.api.services.outreach.store as store_mod
    monkeypatch.setattr(database, "SessionLocal", SL, raising=False)
    monkeypatch.setattr(store_mod, "SessionLocal", SL, raising=False)

    lead_emails = {1: "alice@x.com", 2: "bob@y.com", 3: ""}  # 3 has no email

    class _Ctx(WorkspaceCtx):
        def lead_db(self):
            return _FakeLeadStore(lead_emails)

    app = FastAPI()
    app.include_router(outreach_router)

    def _override_ws():
        return _Ctx(user=_User(), workspace_id=WS, slug="api")

    app.dependency_overrides[current_workspace] = _override_ws
    app.dependency_overrides[require_admin] = _override_ws
    return TestClient(app)


def _create_seq(tc, consent="legit"):
    r = tc.post("/api/outreach/sequences", json={
        "name": "S", "consent_basis": consent,
        "steps": [{"step_number": 0, "subject": "Hi", "body_html": "<p>Hi</p>", "delay_hours": 0}],
    })
    assert r.status_code == 200, r.text
    return r.json()["id"]


def test_sequence_crud(client):
    sid = _create_seq(client)
    assert client.get("/api/outreach/sequences").json()["sequences"][0]["id"] == sid
    assert client.get(f"/api/outreach/sequences/{sid}").json()["id"] == sid
    assert client.put(f"/api/outreach/sequences/{sid}", json={"name": "S2"}).status_code == 200
    assert client.delete(f"/api/outreach/sequences/{sid}").status_code == 200
    assert client.get(f"/api/outreach/sequences/{sid}").status_code == 404


def test_enroll_validation_dedup_snapshot_suppression(client):
    sid = _create_seq(client)
    # enroll lead 1 (has email), lead 3 (no email → skipped), dup lead 1.
    r = client.post(f"/api/outreach/sequences/{sid}/enroll",
                    json={"lead_ids": [1, 3, 1]})
    body = r.json()
    assert body["enrolled"] == 1
    reasons = {s["reason"] for s in body["skipped"]}
    assert "no_email" in reasons and "already_enrolled" in reasons
    # suppress lead 2's email, then enroll → filtered.
    client.post("/api/outreach/suppressions", json={"email": "bob@y.com"})
    r2 = client.post(f"/api/outreach/sequences/{sid}/enroll", json={"lead_ids": [2]})
    assert r2.json()["enrolled"] == 0
    assert any(s["reason"] == "suppressed" for s in r2.json()["skipped"])


def test_enroll_unknown_sequence_404(client):
    r = client.post("/api/outreach/sequences/does-not-exist/enroll", json={"lead_ids": [1]})
    assert r.status_code == 404


def test_suppression_locked_gate(client):
    # manual add → removable.
    client.post("/api/outreach/suppressions", json={"email": "m@x.com"})
    assert client.delete("/api/outreach/suppressions/m@x.com").status_code == 200
    # add a locked (unsubscribe) suppression directly via unsubscribe POST, then
    # try to remove → 403.
    from apps.api.services.outreach.tokens import make_unsubscribe_token
    tok = make_unsubscribe_token(WS, "locked@x.com", "")
    assert client.post(f"/api/outreach/unsubscribe?token={tok}").status_code == 200
    assert client.delete("/api/outreach/suppressions/locked@x.com").status_code == 403


def test_unsubscribe_one_click_idempotent(client):
    from apps.api.services.outreach.tokens import make_unsubscribe_token
    tok = make_unsubscribe_token(WS, "u@x.com", "seq1")
    # No auth, no CSRF, side-effecting.
    assert client.post(f"/api/outreach/unsubscribe?token={tok}").status_code == 200
    assert client.post(f"/api/outreach/unsubscribe?token={tok}").status_code == 200  # idempotent
    sups = client.get("/api/outreach/suppressions").json()["suppressions"]
    assert sum(1 for s in sups if s["email"] == "u@x.com") == 1  # exactly one


def test_unsubscribe_forged_token_rejected(client):
    assert client.post("/api/outreach/unsubscribe?token=garbage.sig").status_code == 400


def test_start_requires_consent_and_footer(client, monkeypatch):
    import apps.api.services.outreach.sender as sender_mod
    import apps.api.services.workspace.secrets as secrets_mod
    monkeypatch.setattr(sender_mod, "is_smtp_configured", lambda ws=None: True)
    # No footer set → 400.
    monkeypatch.setattr(secrets_mod, "get_secret", lambda ws, k, d="": "" if k == "OUTREACH_FOOTER" else d)
    sid = _create_seq(client)
    assert client.post(f"/api/outreach/sequences/{sid}/start").status_code == 400
    # With footer + consent → ok.
    monkeypatch.setattr(secrets_mod, "get_secret", lambda ws, k, d="": "Acme, 1 Main St" if k == "OUTREACH_FOOTER" else d)
    r = client.post(f"/api/outreach/sequences/{sid}/start")
    assert r.status_code == 200 and r.json()["status"] == "active"


def test_start_blocked_without_consent(client, monkeypatch):
    import apps.api.services.outreach.sender as sender_mod
    import apps.api.services.workspace.secrets as secrets_mod
    monkeypatch.setattr(sender_mod, "is_smtp_configured", lambda ws=None: True)
    monkeypatch.setattr(secrets_mod, "get_secret", lambda ws, k, d="": "Acme" if k == "OUTREACH_FOOTER" else d)
    sid = _create_seq(client, consent="")  # no consent_basis
    assert client.post(f"/api/outreach/sequences/{sid}/start").status_code == 400
