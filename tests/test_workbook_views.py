"""Workbook saved views + single-cell force re-run — router tests (SQLite, offline).

Auth/workspace deps are overridden (pattern: test_automations_api.py) so we
exercise validation + CRUD + tenancy logic without a real auth stack. Covers:

  * views CRUD (create/list/rename/update-config/delete) under /api/v2
  * config JSON round-trip (filters + sort + hidden_columns survive verbatim)
  * workspace isolation — a view of a workbook in another workspace is 404
    (list/create/update/delete), never a leak
  * invalid view config (unknown filter op) → 422
  * single-cell run endpoint: force=true bypasses the output run-once
    success-skip gate (provider layer mocked); force=false keeps it
  * single-cell re-run of an already-complete enrichment cell replaces the
    value (mocked provider chain)
"""

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.core.ratelimit import limiter
from apps.api.core.tenancy import WorkspaceCtx, current_workspace
from apps.api.database import Base, get_db
from apps.api.services.workbook.models import (
    Workbook, WorkbookRow, WorkbookEnrichment, WorkbookView,
)
from apps.api.services.workbook.planner_models import ProviderStat
from apps.api.routers.workbooks import router as workbooks_router, views_router

WS1 = "ws_views_alpha"
WS2 = "ws_views_beta"


class _User:
    id = "user-1"


def _ctx(ws: str) -> WorkspaceCtx:
    return WorkspaceCtx(user=_User(), workspace_id=ws, slug=ws)


@pytest.fixture()
def client():
    engine = create_engine(
        "sqlite:///:memory:", connect_args={"check_same_thread": False}, poolclass=StaticPool,
    )
    Base.metadata.create_all(engine, tables=[
        Workbook.__table__, WorkbookRow.__table__,
        WorkbookEnrichment.__table__, WorkbookView.__table__,
        ProviderStat.__table__,
    ])
    Session = sessionmaker(bind=engine)

    app = FastAPI()
    app.state.limiter = limiter  # /run + cell-run endpoints are @limiter-decorated
    app.include_router(workbooks_router)
    app.include_router(views_router)

    def _override_db():
        s = Session()
        try:
            yield s
        finally:
            s.close()

    app.dependency_overrides[get_db] = _override_db
    app.dependency_overrides[current_workspace] = lambda: _ctx(WS1)

    return TestClient(app), Session, app


def _mk_workbook(Session, columns, ws=WS1):
    s = Session()
    wb = Workbook(name="WB", workspace_id=ws, columns_config=columns)
    s.add(wb)
    s.commit()
    wid = wb.id
    s.close()
    return wid


def _mk_row(Session, wid, data, enrichments=None, ws=WS1, lead_id=None):
    s = Session()
    r = WorkbookRow(
        workbook_id=wid, workspace_id=ws, position=0,
        data=data, enrichments=enrichments or {}, lead_id=lead_id,
    )
    s.add(r)
    s.commit()
    rid = r.id
    s.close()
    return rid


# ── Views CRUD ────────────────────────────────────────────────────────────

def test_views_crud(client):
    tc, Session, _ = client
    wid = _mk_workbook(Session, [{"id": "company", "name": "Company", "type": "lead_field"}])

    # empty list first
    r = tc.get(f"/api/v2/workbooks/{wid}/views")
    assert r.status_code == 200 and r.json() == {"views": [], "total": 0}

    # create
    r = tc.post(f"/api/v2/workbooks/{wid}/views", json={"name": "Hot leads"})
    assert r.status_code == 201, r.text
    view = r.json()
    vid = view["id"]
    assert view["name"] == "Hot leads"
    assert view["workbook_id"] == wid
    assert view["config"] == {"filters": [], "sort": [], "hidden_columns": []}

    # list
    r = tc.get(f"/api/v2/workbooks/{wid}/views")
    assert r.status_code == 200
    assert r.json()["total"] == 1
    assert r.json()["views"][0]["id"] == vid

    # rename
    r = tc.put(f"/api/v2/workbooks/{wid}/views/{vid}", json={"name": "Hot leads v2"})
    assert r.status_code == 200 and r.json()["name"] == "Hot leads v2"

    # update config only (name untouched)
    cfg = {"filters": [{"column": "company", "op": "contains", "value": "acme"}],
           "sort": [], "hidden_columns": []}
    r = tc.put(f"/api/v2/workbooks/{wid}/views/{vid}", json={"config": cfg})
    assert r.status_code == 200
    assert r.json()["name"] == "Hot leads v2"
    assert r.json()["config"] == cfg

    # delete
    r = tc.delete(f"/api/v2/workbooks/{wid}/views/{vid}")
    assert r.status_code == 200 and r.json()["status"] == "deleted"
    assert tc.get(f"/api/v2/workbooks/{wid}/views").json()["total"] == 0


def test_view_config_round_trip(client):
    """The full config JSON (filters + sort + hidden_columns) survives verbatim."""
    tc, Session, _ = client
    wid = _mk_workbook(Session, [{"id": "email", "name": "Email", "type": "lead_field"}])

    cfg = {
        "filters": [
            {"column": "email", "op": "not_empty", "value": None},
            {"column": "company", "op": "equals", "value": "Acme"},
            {"column": "score", "op": "not_contains", "value": "0"},
        ],
        "sort": [
            {"column": "score", "dir": "desc"},
            {"column": "company", "dir": "asc"},
        ],
        "hidden_columns": ["notes", "source"],
    }
    r = tc.post(f"/api/v2/workbooks/{wid}/views", json={"name": "RT", "config": cfg})
    assert r.status_code == 201, r.text
    vid = r.json()["id"]
    assert r.json()["config"] == cfg

    # read back from the DB through the API — still byte-identical
    got = tc.get(f"/api/v2/workbooks/{wid}/views").json()["views"][0]
    assert got["id"] == vid
    assert got["config"] == cfg


def test_view_invalid_config_rejected(client):
    tc, Session, _ = client
    wid = _mk_workbook(Session, [])
    r = tc.post(f"/api/v2/workbooks/{wid}/views", json={
        "name": "bad",
        "config": {"filters": [{"column": "email", "op": "regex_match", "value": ".*"}]},
    })
    assert r.status_code == 422


def test_view_workspace_isolation(client):
    """A view of a workbook in another workspace is a 404 on every verb."""
    tc, Session, app = client
    wid = _mk_workbook(Session, [], ws=WS1)
    r = tc.post(f"/api/v2/workbooks/{wid}/views", json={"name": "mine"})
    assert r.status_code == 201
    vid = r.json()["id"]

    # switch the caller to WS2 — same ids, different tenant
    app.dependency_overrides[current_workspace] = lambda: _ctx(WS2)
    try:
        assert tc.get(f"/api/v2/workbooks/{wid}/views").status_code == 404
        assert tc.post(f"/api/v2/workbooks/{wid}/views", json={"name": "x"}).status_code == 404
        assert tc.put(f"/api/v2/workbooks/{wid}/views/{vid}", json={"name": "x"}).status_code == 404
        assert tc.delete(f"/api/v2/workbooks/{wid}/views/{vid}").status_code == 404
    finally:
        app.dependency_overrides[current_workspace] = lambda: _ctx(WS1)

    # untouched for the real owner
    got = tc.get(f"/api/v2/workbooks/{wid}/views").json()
    assert got["total"] == 1 and got["views"][0]["name"] == "mine"


def test_view_direct_view_id_cross_workbook_404(client):
    """A valid view id under a DIFFERENT (same-tenant) workbook is still 404."""
    tc, Session, _ = client
    wid_a = _mk_workbook(Session, [])
    wid_b = _mk_workbook(Session, [])
    vid = tc.post(f"/api/v2/workbooks/{wid_a}/views", json={"name": "a"}).json()["id"]
    assert tc.put(f"/api/v2/workbooks/{wid_b}/views/{vid}", json={"name": "x"}).status_code == 404
    assert tc.delete(f"/api/v2/workbooks/{wid_b}/views/{vid}").status_code == 404


# ── Single-cell force re-run ─────────────────────────────────────────────

def test_cell_run_force_bypasses_output_run_once(client, monkeypatch):
    """Output columns are run-once; force=true (and only force) re-pushes."""
    import apps.api.services.workbook.enrichment as enr

    tc, Session, _ = client
    wid = _mk_workbook(Session, [
        {"id": "push", "name": "Push", "type": "output",
         "destination": "webhook", "destination_config": {"url": "https://x.example/hook"}},
    ])
    rid = _mk_row(Session, wid, {"company": "Acme"}, lead_id=101)

    # Prior COMPLETE output cell — the run-once gate keys off this.
    s = Session()
    s.add(WorkbookEnrichment(
        workbook_id=wid, workspace_id=WS1, lead_id=101, column_id="push",
        value="sent", status="complete", provider="webhook",
    ))
    s.commit()
    s.close()

    calls = []

    async def _fake_output(**kwargs):
        calls.append(kwargs)
        return {"value": "pushed-again", "error": None}

    monkeypatch.setattr(enr, "execute_output_column", _fake_output)

    # Without force → the success-skip gate holds; destination NOT re-hit.
    r = tc.post(f"/api/workbooks/{wid}/rows/{rid}/cells/push/run", json={"force": False})
    assert r.status_code == 200, r.text
    body = r.json()
    assert body["skipped"] is True and body["status"] == "skipped"
    assert body["value"] == "sent"
    assert calls == []

    # With force → gate bypassed; the destination IS re-hit exactly once.
    r = tc.post(f"/api/workbooks/{wid}/rows/{rid}/cells/push/run", json={"force": True})
    assert r.status_code == 200, r.text
    body = r.json()
    assert body["skipped"] is False and body["status"] == "complete"
    assert body["value"] == "pushed-again" and body["forced"] is True
    assert len(calls) == 1

    # And the cell value was actually replaced.
    s = Session()
    cell = s.query(WorkbookEnrichment).filter_by(workbook_id=wid, column_id="push").one()
    assert cell.value == "pushed-again" and cell.status == "complete"
    s.close()


def test_cell_run_force_reruns_complete_enrichment_cell(client, monkeypatch):
    """A complete waterfall cell re-runs the provider chain and takes the new value."""
    import apps.api.services.workbook.enrichment as enr

    tc, Session, _ = client
    wid = _mk_workbook(Session, [
        {"id": "find_email", "name": "Find Email", "type": "waterfall",
         "target_field": "email", "waterfall": ["mock_provider"], "verify": False},
    ])
    rid = _mk_row(
        Session, wid, {"company": "Acme", "website": "acme.com"},
        enrichments={"find_email": {"value": "old@acme.com", "status": "complete"}},
        lead_id=202,
    )
    s = Session()
    s.add(WorkbookEnrichment(
        workbook_id=wid, workspace_id=WS1, lead_id=202, column_id="find_email",
        value="old@acme.com", status="complete", provider="mock_provider",
    ))
    s.commit()
    s.close()

    class _DummyProvider:
        default_confidence = 0.9

    provider_calls = []

    async def _fake_run_provider(name, lead, timeout=10.0):
        provider_calls.append(name)
        return {"provider": name, "success": True, "confidence": 0.9,
                "fields": {"email": "new@acme.com"}}

    monkeypatch.setattr(enr, "get_provider", lambda name: _DummyProvider())
    monkeypatch.setattr(enr, "run_provider", _fake_run_provider)

    r = tc.post(f"/api/workbooks/{wid}/rows/{rid}/cells/find_email/run", json={"force": True})
    assert r.status_code == 200, r.text
    body = r.json()
    assert body["status"] == "complete"
    assert body["value"] == "new@acme.com"
    assert provider_calls, "provider chain must be re-invoked despite the complete cell"

    # Persisted: overlay row AND the inline WorkbookRow.enrichments mirror.
    s = Session()
    cell = s.query(WorkbookEnrichment).filter_by(workbook_id=wid, column_id="find_email").one()
    assert cell.value == "new@acme.com"
    row = s.query(WorkbookRow).filter_by(id=rid).one()
    assert row.enrichments["find_email"]["value"] == "new@acme.com"
    s.close()


def test_cell_run_404s(client):
    tc, Session, _ = client
    wid = _mk_workbook(Session, [
        {"id": "col_a", "name": "A", "type": "waterfall", "target_field": "email"},
        {"id": "company", "name": "Company", "type": "lead_field"},
    ])
    rid = _mk_row(Session, wid, {"company": "Acme"})

    # unknown / non-enrichment column → 404
    assert tc.post(f"/api/workbooks/{wid}/rows/{rid}/cells/nope/run", json={}).status_code == 404
    assert tc.post(f"/api/workbooks/{wid}/rows/{rid}/cells/company/run", json={}).status_code == 404
    # unknown workbook → 404
    assert tc.post(f"/api/workbooks/zzz/rows/{rid}/cells/col_a/run", json={}).status_code == 404


def test_cell_run_other_workspace_workbook_404(client):
    """Cell run on a workbook owned by another tenant → 404 (no leak)."""
    tc, Session, app = client
    wid = _mk_workbook(Session, [
        {"id": "col_a", "name": "A", "type": "waterfall", "target_field": "email"},
    ], ws=WS1)
    rid = _mk_row(Session, wid, {"company": "Acme"})

    app.dependency_overrides[current_workspace] = lambda: _ctx(WS2)
    try:
        r = tc.post(f"/api/workbooks/{wid}/rows/{rid}/cells/col_a/run", json={"force": True})
        assert r.status_code == 404
    finally:
        app.dependency_overrides[current_workspace] = lambda: _ctx(WS1)
