"""Inbound rows API tests (SQLite, offline).

Covers: flag OFF → 404, JWT-session happy path, ingest-token happy path +
bad/rotated/cross-workbook token → 401, the 500-row cap (413), case-insensitive
column mapping (display name + lead field; unknown keys kept + reported),
Idempotency-Key replay, dedup against existing rows, and workspace isolation
(other tenant's workbook → 404).

Auth/workspace deps are overridden the same way tests/test_intent_poller_api.py
does, so we exercise the router's mapping/dedup/idempotency/token logic without
a real auth stack. resolve_ingest_token runs for real against the in-memory DB
(apps.api.database.SessionLocal is monkeypatched).
"""

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.core.config import settings
from apps.api.core.tenancy import WorkspaceCtx
from apps.api.database import Base, get_db
from apps.api.services.workbook.models import Workbook, WorkbookRow
from apps.api.services.workbook.ingest import (
    WorkbookIngestIdempotency, WorkbookIngestToken,
)
from apps.api.routers.ingest import (
    router as ingest_router, require_editor, optional_session_workspace,
)

W1 = "ws_ingest_alpha"
W2 = "ws_ingest_beta"
WB1 = "wb-ingest-1"
WB2 = "wb-ingest-2"  # other tenant's workbook

COLUMNS = [
    {"id": "c1", "name": "Company", "type": "lead_field", "lead_field": "company"},
    {"id": "c2", "name": "Website", "type": "lead_field", "lead_field": "website"},
    {"id": "c3", "name": "Work Email", "type": "lead_field", "lead_field": "email"},
]


class _User:
    id = "user-1"


def _ctx(ws_id=W1):
    return WorkspaceCtx(user=_User(), workspace_id=ws_id, slug="ingest-test")


@pytest.fixture()
def harness(monkeypatch):
    """(client_factory, Session). client_factory(session_ctx=..., enabled=...)."""
    monkeypatch.setattr(settings, "INGEST_API_ENABLED", True, raising=False)

    engine = create_engine(
        "sqlite:///:memory:", connect_args={"check_same_thread": False},
        poolclass=StaticPool,
    )
    Base.metadata.create_all(engine, tables=[
        Workbook.__table__, WorkbookRow.__table__,
        WorkbookIngestToken.__table__, WorkbookIngestIdempotency.__table__,
    ])
    Session = sessionmaker(bind=engine)

    # resolve_ingest_token opens its own session off apps.api.database.
    import apps.api.database as database
    monkeypatch.setattr(database, "SessionLocal", Session, raising=False)

    with Session() as s:
        s.add(Workbook(id=WB1, name="Inbound", workspace_id=W1,
                       columns_config=COLUMNS, source_type="empty"))
        s.add(Workbook(id=WB2, name="Other tenant", workspace_id=W2,
                       columns_config=COLUMNS, source_type="empty"))
        s.commit()

    def make_client(session_ctx=None, enabled=True):
        monkeypatch.setattr(settings, "INGEST_API_ENABLED", enabled, raising=False)
        app = FastAPI()
        app.include_router(ingest_router)

        def _override_db():
            s = Session()
            try:
                yield s
            finally:
                s.close()

        app.dependency_overrides[get_db] = _override_db
        app.dependency_overrides[require_editor] = lambda: _ctx()
        if session_ctx is not None:
            app.dependency_overrides[optional_session_workspace] = lambda: session_ctx
        return TestClient(app)

    return make_client, Session


def _rows(Session, wb_id=WB1):
    with Session() as s:
        return s.query(WorkbookRow).filter(WorkbookRow.workbook_id == wb_id).all()


# ── flag gate ─────────────────────────────────────────────────────────────────

def test_flag_off_404(harness):
    make_client, _ = harness
    tc = make_client(session_ctx=_ctx(), enabled=False)
    r = tc.post(f"/api/v2/workbooks/{WB1}/rows/ingest", json={"rows": [{"company": "Acme"}]})
    assert r.status_code == 404
    assert tc.post(f"/api/v2/workbooks/{WB1}/ingest-token").status_code == 404


# ── session/JWT path ──────────────────────────────────────────────────────────

def test_jwt_happy_path_and_column_mapping(harness):
    make_client, Session = harness
    tc = make_client(session_ctx=_ctx())
    r = tc.post(f"/api/v2/workbooks/{WB1}/rows/ingest", json={"rows": [
        # column display name (any case), lead field (any case), unknown key
        {"Company": "Acme", "WEBSITE": "https://acme.com", "work email": "a@acme.com",
         "custom_thing": "kept"},
        {"company": "Globex", "email": "g@globex.com"},
    ]})
    assert r.status_code == 200, r.text
    body = r.json()
    assert body["added"] == 2
    assert body["skipped_duplicates"] == 0
    assert body["total_rows"] == 2
    assert body["unmapped_keys"] == ["custom_thing"]
    assert len(body["row_ids"]) == 2

    rows = _rows(Session)
    assert len(rows) == 2
    by_company = {r.data["company"]: r for r in rows}
    acme = by_company["Acme"]
    # mapped to canonical lead_field names; unknown key kept verbatim
    assert acme.data == {"company": "Acme", "website": "https://acme.com",
                         "email": "a@acme.com", "custom_thing": "kept"}
    # denormalized tenant stamped on every row (RLS depends on it)
    assert all(r.workspace_id == W1 for r in rows)
    assert [r.position for r in sorted(rows, key=lambda r: r.position)] == [1, 2]
    # total_rows refreshed on the workbook
    with Session() as s:
        assert s.get(Workbook, WB1).total_rows == 2


def test_workspace_isolation_other_tenant_workbook_404(harness):
    make_client, Session = harness
    tc = make_client(session_ctx=_ctx(W1))
    r = tc.post(f"/api/v2/workbooks/{WB2}/rows/ingest", json={"rows": [{"company": "X"}]})
    assert r.status_code == 404
    assert _rows(Session, WB2) == []


def test_no_credentials_401(harness):
    make_client, _ = harness
    tc = make_client()  # real optional_session_workspace: no auth header → None
    r = tc.post(f"/api/v2/workbooks/{WB1}/rows/ingest", json={"rows": [{"company": "X"}]})
    assert r.status_code == 401


# ── ingest-token path ─────────────────────────────────────────────────────────

def test_token_mint_auth_rotate_and_bad_token(harness):
    make_client, Session = harness
    tc = make_client()  # machine caller: no session override

    # mint (role-checked dep overridden; plaintext returned once)
    r = tc.post(f"/api/v2/workbooks/{WB1}/ingest-token")
    assert r.status_code == 200, r.text
    minted = r.json()
    token = minted["token"]
    assert token.startswith("wbi_")
    assert minted["workbook_id"] == WB1 and minted["workspace_id"] == W1
    with Session() as s:
        stored = s.query(WorkbookIngestToken).filter_by(id=minted["id"]).one()
        assert stored.token_hash != token  # only the sha256 hash is stored
        assert token.startswith(stored.prefix) and len(stored.prefix) < len(token)

    # happy path via Authorization: Bearer
    r = tc.post(
        f"/api/v2/workbooks/{WB1}/rows/ingest",
        json={"rows": [{"company": "TokenCo", "website": "tokenco.io"}]},
        headers={"Authorization": f"Bearer {token}"},
    )
    assert r.status_code == 200, r.text
    assert r.json()["added"] == 1
    assert _rows(Session)[0].workspace_id == W1

    # happy path via X-Ingest-Token
    r = tc.post(
        f"/api/v2/workbooks/{WB1}/rows/ingest",
        json={"rows": [{"company": "HeaderCo", "website": "headerco.io"}]},
        headers={"X-Ingest-Token": token},
    )
    assert r.status_code == 200

    # bad token → 401
    r = tc.post(
        f"/api/v2/workbooks/{WB1}/rows/ingest",
        json={"rows": [{"company": "Nope"}]},
        headers={"Authorization": "Bearer wbi_definitely-not-a-token"},
    )
    assert r.status_code == 401

    # token bound to WB1 must not work on another workbook → 401
    r = tc.post(
        f"/api/v2/workbooks/{WB2}/rows/ingest",
        json={"rows": [{"company": "Cross"}]},
        headers={"Authorization": f"Bearer {token}"},
    )
    assert r.status_code == 401
    assert _rows(Session, WB2) == []

    # rotate → exactly one live token; the old plaintext stops working
    r = tc.post(f"/api/v2/workbooks/{WB1}/ingest-token")
    assert r.status_code == 200
    assert r.json()["rotated"] == 1
    new_token = r.json()["token"]
    r = tc.post(
        f"/api/v2/workbooks/{WB1}/rows/ingest",
        json={"rows": [{"company": "OldToken"}]},
        headers={"Authorization": f"Bearer {token}"},
    )
    assert r.status_code == 401
    r = tc.post(
        f"/api/v2/workbooks/{WB1}/rows/ingest",
        json={"rows": [{"company": "NewToken", "website": "newtoken.io"}]},
        headers={"Authorization": f"Bearer {new_token}"},
    )
    assert r.status_code == 200


def test_ingest_token_endpoint_scoped_to_tenant(harness):
    make_client, _ = harness
    tc = make_client()  # require_editor override binds ctx to W1
    assert tc.post(f"/api/v2/workbooks/{WB2}/ingest-token").status_code == 404


# ── row cap / validation ──────────────────────────────────────────────────────

def test_row_cap_413(harness):
    make_client, Session = harness
    tc = make_client(session_ctx=_ctx())
    rows = [{"company": f"c{i}", "website": f"c{i}.com"} for i in range(501)]
    r = tc.post(f"/api/v2/workbooks/{WB1}/rows/ingest", json={"rows": rows})
    assert r.status_code == 413
    assert _rows(Session) == []
    # exactly at the cap is accepted
    r = tc.post(f"/api/v2/workbooks/{WB1}/rows/ingest", json={"rows": rows[:500]})
    assert r.status_code == 200 and r.json()["added"] == 500


def test_empty_rows_422_and_empty_dicts_skipped(harness):
    make_client, _ = harness
    tc = make_client(session_ctx=_ctx())
    assert tc.post(f"/api/v2/workbooks/{WB1}/rows/ingest", json={"rows": []}).status_code == 422
    r = tc.post(f"/api/v2/workbooks/{WB1}/rows/ingest",
                json={"rows": [{}, {"company": ""}, {"company": "Real"}]})
    assert r.status_code == 200
    assert r.json()["added"] == 1 and r.json()["skipped_empty"] == 2


# ── dedup ─────────────────────────────────────────────────────────────────────

def test_dedup_against_existing_and_within_batch(harness):
    make_client, Session = harness
    tc = make_client(session_ctx=_ctx())
    r = tc.post(f"/api/v2/workbooks/{WB1}/rows/ingest",
                json={"rows": [{"company": "Acme", "website": "acme.com"}]})
    assert r.status_code == 200 and r.json()["added"] == 1

    # same domain again (existing) + an in-batch repeat + one new
    r = tc.post(f"/api/v2/workbooks/{WB1}/rows/ingest", json={"rows": [
        {"company": "ACME Inc", "website": "https://www.acme.com/"},
        {"company": "Fresh", "website": "fresh.io"},
        {"company": "Fresh Again", "website": "fresh.io"},
    ]})
    assert r.status_code == 200
    body = r.json()
    assert body["added"] == 1 and body["skipped_duplicates"] == 2
    assert len(_rows(Session)) == 2

    # dedupe=false appends blindly
    r = tc.post(f"/api/v2/workbooks/{WB1}/rows/ingest",
                json={"rows": [{"company": "Acme", "website": "acme.com"}],
                      "dedupe": False})
    assert r.status_code == 200 and r.json()["added"] == 1
    assert len(_rows(Session)) == 3


# ── idempotency ───────────────────────────────────────────────────────────────

def test_idempotency_replay(harness):
    make_client, Session = harness
    tc = make_client(session_ctx=_ctx())
    payload = {"rows": [{"company": "Idem", "website": "idem.dev"}]}
    r1 = tc.post(f"/api/v2/workbooks/{WB1}/rows/ingest", json=payload,
                 headers={"Idempotency-Key": "k-123"})
    assert r1.status_code == 200 and r1.json()["added"] == 1

    # replay: same stored result, no header duplication of rows
    r2 = tc.post(f"/api/v2/workbooks/{WB1}/rows/ingest", json=payload,
                 headers={"Idempotency-Key": "k-123"})
    assert r2.status_code == 200
    assert r2.headers.get("idempotent-replay") == "true"
    assert r2.json() == r1.json()
    assert len(_rows(Session)) == 1

    # a different key ingests normally (dedupe off to isolate idempotency)
    r3 = tc.post(f"/api/v2/workbooks/{WB1}/rows/ingest",
                 json={**payload, "dedupe": False},
                 headers={"Idempotency-Key": "k-456"})
    assert r3.status_code == 200 and r3.json()["added"] == 1
    assert len(_rows(Session)) == 2
