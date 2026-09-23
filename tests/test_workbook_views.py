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

import csv
import io

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.core.ratelimit import limiter
from apps.api.core.tenancy import WorkspaceCtx, current_workspace
from apps.api.database import Base, get_db
from apps.api.models import Job
from apps.api.services.workbook.models import (
    Workbook, WorkbookRow, WorkbookEnrichment, WorkbookView,
)
from apps.api.services.workbook.planner_models import ProviderStat
from apps.api.routers.workbooks import (
    router as workbooks_router,
    views_router,
    require_editor,
)

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
        ProviderStat.__table__, Job.__table__,
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
    app.dependency_overrides[require_editor] = lambda: _ctx(WS1)

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


def test_row_edit_enqueues_only_reactive_downstream_chain(client):
    tc, Session, _ = client
    columns = [
        {"id": "company", "name": "Company", "type": "input", "lead_field": "company"},
        {"id": "summary", "type": "ai_formula", "prompt": "Summarize {Company}"},
        {"id": "score", "type": "formula", "formula": "len({summary})"},
        {"id": "push", "type": "output", "destination_config": {"body": "{score}"}},
        {"id": "other", "type": "formula", "formula": "2 + 2"},
    ]
    wid = _mk_workbook(Session, columns)
    rid = _mk_row(Session, wid, {"company": "Before"})

    response = tc.patch(f"/api/workbooks/{wid}/rows/{rid}", json={"company": "After"})

    assert response.status_code == 200, response.text
    payload = response.json()
    assert payload["reactive_columns"] == ["summary", "score"]
    assert payload["recompute"]["status"] == "started"
    with Session() as session:
        job = session.query(Job).filter(Job.type == "run_workbook").one()
        assert job.payload["row_ids"] == [rid]
        assert job.payload["column_ids"] == ["summary", "score"]
        assert job.payload["force"] is True


@pytest.mark.parametrize("bulk", [False, True])
def test_unchanged_row_edit_does_not_enqueue_paid_recompute(client, bulk):
    tc, Session, _ = client
    wid = _mk_workbook(Session, [
        {"id": "company", "type": "input"},
        {"id": "summary", "type": "ai_formula", "prompt": "{company}"},
    ])
    rid = _mk_row(Session, wid, {"company": "Same"})
    endpoint = f"/api/workbooks/{wid}/rows"
    body = {"updates": [{"row_id": rid, "fields": {"company": "Same"}}]} if bulk else {"company": "Same"}
    response = tc.patch(endpoint if bulk else f"{endpoint}/{rid}", json=body)
    assert response.status_code == 200, response.text
    assert response.json()["reactive_columns"] == []
    assert response.json()["recompute"] is None
    with Session() as db:
        assert db.query(Job).count() == 0


def test_bulk_recompute_excludes_unchanged_rows(client):
    tc, Session, _ = client
    wid = _mk_workbook(Session, [
        {"id": "company", "type": "input"},
        {"id": "summary", "type": "ai_formula", "prompt": "{company}"},
    ])
    unchanged = _mk_row(Session, wid, {"company": "Same"})
    changed = _mk_row(Session, wid, {"company": "Before"})
    response = tc.patch(f"/api/workbooks/{wid}/rows", json={"updates": [
        {"row_id": unchanged, "fields": {"company": "Same"}},
        {"row_id": changed, "fields": {"company": "After"}},
    ]})
    assert response.status_code == 200, response.text
    assert response.json()["updated_rows"] == 2  # acknowledged input rows
    with Session() as db:
        assert db.query(Job).one().payload["row_ids"] == [changed]


@pytest.mark.parametrize("selection", [{"row_ids": []}, {"lead_ids": []}, {"row_ids": [999999]}])
@pytest.mark.parametrize("has_rows", [False, True])
def test_explicit_empty_run_scope_never_expands(client, selection, has_rows):
    from apps.api.services.workbook.enrichment import _load_workbook_leads
    tc, Session, _ = client
    wid = _mk_workbook(Session, [{"id": "formula", "name": "Formula", "type": "formula", "formula": "1 + 1"}])
    if has_rows:
        _mk_row(Session, wid, {"company": "Do not process"})
    response = tc.post(f"/api/workbooks/{wid}/run", json=selection)
    assert response.status_code == 200, response.text
    assert response.json()["status"] == "skipped"
    with Session() as db:
        assert db.query(Job).count() == 0
        assert _load_workbook_leads(db, db.get(Workbook, wid), **selection) == []


def test_bulk_edit_queues_and_prices_exact_cells(client, monkeypatch):
    from apps.api.services.billing import service as billing
    tc, Session, _ = client
    wid = _mk_workbook(Session, [
        {"id": "company", "type": "input"}, {"id": "email", "type": "input"},
        {"id": "summary", "type": "ai_formula", "prompt": "{company}"},
        {"id": "domain", "type": "formula", "formula": "{email}"},
    ])
    first = _mk_row(Session, wid, {"company": "Before", "email": "same"})
    second = _mk_row(Session, wid, {"company": "same", "email": "before"})
    prices, debits = [], []
    monkeypatch.setattr(billing, "billing_enabled", lambda: True)
    def price(count, providers, *, column_counts=None):
        prices.append((count, set(providers), column_counts))
        return sum(column_counts.values())
    monkeypatch.setattr(billing, "projected_platform_cost", price)
    monkeypatch.setattr(billing, "check_and_debit", lambda db, ws, amount, **kw: debits.append(amount))
    response = tc.patch(f"/api/workbooks/{wid}/rows", json={"updates": [
        {"row_id": first, "fields": {"company": "After"}},
        {"row_id": second, "fields": {"email": "after"}},
    ]})
    assert response.status_code == 200, response.text
    assert response.json()["recompute"]["total_jobs"] == 2
    assert prices == [(2, {"summary", "domain"}, {"summary": 1, "domain": 1})]
    assert debits == [2]
    history = tc.get(f"/api/workbooks/{wid}/runs").json()["runs"]
    assert history[0]["selected_cell_count"] == 2
    with Session() as db:
        payload = db.query(Job).one().payload
        assert payload["row_columns"] == {str(first): ["summary"], str(second): ["domain"]}
        assert payload["row_ids"] == [first, second]


@pytest.mark.parametrize("scope", [{"01": ["summary"]}, {"1": ["missing"]}, {}])
def test_run_rejects_invalid_exact_cell_scope(client, scope):
    tc, Session, _ = client
    wid = _mk_workbook(Session, [{"id": "summary", "type": "ai_formula", "prompt": "Research"}])
    rid = _mk_row(Session, wid, {"company": "Keep"})
    response = tc.post(f"/api/workbooks/{wid}/run", json={"row_ids": [rid], "row_columns": scope})
    assert response.status_code == 422, response.text
    with Session() as db:
        assert db.query(Job).count() == 0


def test_cell_data_cannot_override_execution_identities(client, monkeypatch):
    from apps.api.services.workbook import enrichment as engine
    tc, Session, _ = client
    wid = _mk_workbook(Session, [{"id": "formula", "type": "formula", "formula": "1"}])
    rid = _mk_row(Session, wid, {"id": 900, "__row_id": 901, "__lead_id": 902, "company": "Keep"})
    with Session() as db:
        loaded = engine._load_workbook_leads(db, db.get(Workbook, wid), [rid])
        assert loaded == [{"id": rid, "__row_id": rid, "__lead_id": None, "company": "Keep"}]
    calls = []
    async def execute(**kwargs):
        calls.append(kwargs["lead_data"])
        return {"success": True, "value": "1"}
    monkeypatch.setattr(engine, "enrich_cell", execute)
    response = tc.post(f"/api/workbooks/{wid}/rows/{rid}/cells/formula/run", json={})
    assert response.status_code == 200, response.text
    assert calls == loaded
    response = tc.post(f"/api/workbooks/{wid}/run", json={"row_ids": [rid], "row_columns": {str(rid): ["formula"]}})
    assert response.status_code == 200, response.text
    with Session() as db:
        assert db.query(Job).one().payload["row_ids"] == [rid]


@pytest.mark.parametrize("source_type", ["empty", "csv", "agent", "unknown"])
def test_empty_v2_full_run_never_reads_legacy_leads(client, monkeypatch, source_type):
    from apps.api.services.workbook import enrichment as engine
    from apps.api.services.leadgen import store
    tc, Session, _ = client
    wid = _mk_workbook(Session, [{"id": "formula", "type": "formula", "formula": "1"}])
    with Session() as db:
        db.get(Workbook, wid).source_type = source_type
        db.commit()
    def forbidden(*args, **kwargs):
        raise AssertionError("An empty v2 workbook must not query legacy leads")
    monkeypatch.setattr(WorkspaceCtx, "lead_db", forbidden)
    monkeypatch.setattr(store, "get_lead_store", forbidden)
    response = tc.post(f"/api/workbooks/{wid}/run", json={})
    assert response.status_code == 200, response.text
    assert response.json()["status"] == "skipped"
    assert response.json()["total_jobs"] == 0
    with Session() as db:
        assert engine._load_workbook_leads(db, db.get(Workbook, wid)) == []
        assert db.query(Job).count() == 0


def test_explicit_legacy_workbook_keeps_resolved_lead_scope(client, monkeypatch):
    from types import SimpleNamespace
    from apps.api.routers import workbooks as router
    tc, Session, _ = client
    wid = _mk_workbook(Session, [{"id": "formula", "type": "formula", "formula": "1"}])
    with Session() as db:
        workbook = db.get(Workbook, wid)
        workbook.source_type = "leads_filter"
        workbook.filter_criteria = {"specialization": "SaaS"}
        db.commit()
    monkeypatch.setattr(WorkspaceCtx, "lead_db", lambda self: SimpleNamespace(close=lambda: None))
    def query(store, criteria, **kwargs):
        assert criteria == {"specialization": "SaaS"}
        return [{"id": 71, "company": "Matched"}], 1
    monkeypatch.setattr(router, "_query_leads", query)
    response = tc.post(f"/api/workbooks/{wid}/run", json={})
    assert response.status_code == 200, response.text
    with Session() as db:
        payload = db.query(Job).one().payload
        assert payload["lead_ids"] == [71]
        assert payload["row_ids"] is None


@pytest.mark.parametrize("source_type", ["empty", "csv", "agent"])
def test_empty_v2_readback_does_not_show_legacy_rows(client, monkeypatch, source_type):
    from types import SimpleNamespace
    from apps.api.routers import workbooks as router
    tc, Session, _ = client
    wid = _mk_workbook(Session, [{"id": "company", "type": "input"}])
    with Session() as db:
        db.get(Workbook, wid).source_type = source_type
        db.commit()
    monkeypatch.setattr(WorkspaceCtx, "lead_db", lambda self: SimpleNamespace(close=lambda: None))
    def forbidden(*args, **kwargs):
        raise AssertionError("V2 display must not query legacy records")
    legacy_reads = []
    def capture_forbidden(*args, **kwargs):
        legacy_reads.append(True)
        return forbidden(*args, **kwargs)
    monkeypatch.setattr(router, "_query_leads", capture_forbidden)
    response = tc.get(f"/api/workbooks/{wid}")
    assert response.status_code == 200, response.text
    assert response.json()["rows"] == []
    assert response.json()["total_rows"] == 0
    response = tc.get("/api/workbooks/")
    assert response.status_code == 200, response.text
    workbook = next(item for item in response.json()["workbooks"] if item["id"] == wid)
    assert workbook["total_rows"] == 0
    assert legacy_reads == []


@pytest.mark.parametrize("query_delete", [False, True])
def test_deleting_last_snapshot_row_does_not_reactivate_legacy_source(client, monkeypatch, query_delete):
    from apps.api.services.workbook import enrichment as engine
    tc, Session, _ = client
    wid = _mk_workbook(Session, [{"id": "formula", "type": "formula", "formula": "1"}])
    rid = _mk_row(Session, wid, {"company": "Last snapshot"})
    with Session() as db:
        wb = db.get(Workbook, wid)
        wb.source_type = "leads_filter"
        wb.source_config = {"keep": "metadata"}
        db.commit()
    if query_delete:
        response = tc.post(f"/api/workbooks/{wid}/rows/delete-query", json={"expected_count": 1, "confirmation": "DELETE 1 ROWS"})
    else:
        response = tc.request("DELETE", f"/api/workbooks/{wid}/rows", json={"row_ids": [rid]})
    assert response.status_code == 200, response.text
    def forbidden(*args, **kwargs):
        raise AssertionError("Deleted snapshots must not reactivate legacy data")
    monkeypatch.setattr(WorkspaceCtx, "lead_db", forbidden)
    assert tc.get(f"/api/workbooks/{wid}").json()["rows"] == []
    assert tc.post(f"/api/workbooks/{wid}/run", json={}).json()["status"] == "skipped"
    assert tc.post(f"/api/workbooks/{wid}/rows/{rid}/cells/formula/run", json={}).status_code == 404
    with Session() as db:
        wb = db.get(Workbook, wid)
        assert wb.source_config == {"keep": "metadata", "row_storage_version": 2}
        assert engine._load_workbook_leads(db, wb) == []


def test_row_edit_can_disable_recompute(client):
    tc, Session, _ = client
    wid = _mk_workbook(Session, [
        {"id": "company", "type": "input", "lead_field": "company"},
        {"id": "summary", "type": "ai_formula", "prompt": "{company}"},
    ])
    rid = _mk_row(Session, wid, {"company": "Before"})
    response = tc.patch(
        f"/api/workbooks/{wid}/rows/{rid}?recompute=false", json={"company": "After"},
    )
    assert response.status_code == 200
    assert response.json()["reactive_columns"] == []
    with Session() as session:
        assert session.query(Job).count() == 0


def test_bulk_row_edit_is_atomic_and_recomputes_once(client):
    tc, Session, _ = client
    wid = _mk_workbook(Session, [
        {"id": "company", "type": "input", "lead_field": "company"},
        {"id": "domain", "type": "input", "lead_field": "website"},
        {"id": "summary", "type": "ai_formula", "prompt": "{company} {domain}"},
    ])
    first = _mk_row(Session, wid, {"company": "Before A"})
    second = _mk_row(Session, wid, {"company": "Before B"})

    response = tc.patch(f"/api/workbooks/{wid}/rows", json={"updates": [
        {"row_id": first, "fields": {"company": "After A", "website": "a.example"}},
        {"row_id": second, "fields": {"company": "After B", "website": "b.example"}},
    ]})

    assert response.status_code == 200, response.text
    assert response.json()["updated_rows"] == 2
    assert response.json()["reactive_columns"] == ["summary"]
    with Session() as session:
        assert session.get(WorkbookRow, first).data["company"] == "After A"
        assert session.get(WorkbookRow, second).data["website"] == "b.example"
        job = session.query(Job).filter(Job.type == "run_workbook").one()
        assert job.payload["row_ids"] == [first, second]
        assert job.payload["column_ids"] == ["summary"]


def test_bulk_row_edit_rejects_entire_request_on_non_editable_field(client):
    tc, Session, _ = client
    wid = _mk_workbook(Session, [{"id": "company", "type": "input", "lead_field": "company"}])
    first = _mk_row(Session, wid, {"company": "Before A"})
    second = _mk_row(Session, wid, {"company": "Before B"})

    response = tc.patch(f"/api/workbooks/{wid}/rows?recompute=false", json={"updates": [
        {"row_id": first, "fields": {"company": "After A"}},
        {"row_id": second, "fields": {"not_editable": "bad"}},
    ]})

    assert response.status_code == 400
    with Session() as session:
        assert session.get(WorkbookRow, first).data["company"] == "Before A"
        assert session.get(WorkbookRow, second).data["company"] == "Before B"


@pytest.mark.parametrize("columns", [
    [{"id": "a", "name": "A", "type": "formula", "formula": "{a}"}],
    [{"id": "a", "name": "A", "type": "formula", "formula": "{b}"},
     {"id": "b", "name": "B", "type": "formula", "formula": "{a}"}],
    [{"id": "a", "name": "A", "type": "input"}, {"id": "a", "name": "Duplicate", "type": "input"}],
])
def test_create_rejects_invalid_graph_before_saving_workbook_or_rows(client, columns):
    tc, Session, _ = client
    response = tc.post("/api/workbooks", json={"name": "Invalid graph", "source": "csv",
        "source_config": {"rows": [{"company": "Must not import"}]}, "columns_config": columns})
    assert response.status_code == 422, response.text
    with Session() as db:
        assert db.query(Workbook).count() == 0
        assert db.query(WorkbookRow).count() == 0
        assert db.query(Job).count() == 0


def test_normalized_column_reference_blocks_delete_and_breaking_rename(client):
    tc, Session, _ = client
    columns = [{"id": "source_id", "name": "Company Name", "type": "input"},
               {"id": "derived", "name": "Derived", "type": "formula", "formula": "{company_name}"}]
    wid = _mk_workbook(Session, columns)
    endpoint = f"/api/workbooks/{wid}/columns/source_id"
    assert tc.delete(endpoint).status_code == 409
    renamed = tc.patch(endpoint + "/settings", json={"changes": {"name": "Different"}, "expected": {"name": "Company Name"}})
    assert renamed.status_code == 409, renamed.text
    with Session() as db:
        assert db.get(Workbook, wid).columns_config == columns


def test_column_edits_reject_new_cycles_and_allow_repair(client):
    tc, Session, _ = client
    columns = [{"id": "a", "name": "A", "type": "formula", "formula": "1"},
               {"id": "b", "name": "B", "type": "formula", "formula": "{a}"}]
    wid = _mk_workbook(Session, columns)
    endpoint = f"/api/workbooks/{wid}/columns/a/settings"
    response = tc.patch(endpoint, json={"changes": {"formula": "{b}"}, "expected": {"formula": "1"}})
    assert response.status_code == 422 and "circular" in response.json()["detail"]
    replacement = [{**columns[0], "formula": "{b}"}, columns[1]]
    assert tc.put(f"/api/workbooks/{wid}", json={"name": "Do not change", "columns_config": replacement}).status_code == 422
    assert tc.post(f"/api/workbooks/{wid}/columns", json={"column": {"id": "self", "name": "Self", "type": "formula", "formula": "{self}"}}).status_code == 422
    with Session() as db:
        wb = db.get(Workbook, wid)
        assert wb.columns_config == columns
        assert wb.name != "Do not change"
        wb.columns_config = replacement  # Simulate an existing legacy cycle.
        db.commit()
    repaired = tc.patch(endpoint, json={"changes": {"formula": "1"}, "expected": {"formula": "{b}"}})
    assert repaired.status_code == 200, repaired.text
    with Session() as db:
        assert db.get(Workbook, wid).columns_config == columns


def test_column_settings_patch_preserves_unrelated_changes_and_rejects_conflict(client):
    tc, Session, _ = client
    columns = [{"id": "agent", "name": "Agent", "type": "agent", "width": 180,
                "prompt": "Before", "tools": [], "future_option": {"keep": True}},
               {"id": "company", "name": "Company", "type": "input"}]
    wid = _mk_workbook(Session, columns)
    endpoint = f"/api/workbooks/{wid}/columns/agent/settings"
    # Simulate another client changing width, an unrelated field.
    assert tc.patch(f"/api/workbooks/{wid}/columns/agent/width", json={"width": 400}).status_code == 200
    body = {"changes": {"prompt": "After", "reactive": False}, "expected": {"prompt": "Before", "reactive": None}}
    response = tc.patch(endpoint, json=body)
    assert response.status_code == 200, response.text
    assert response.json() == {"column_id": "agent", "changes": body["changes"]}
    assert tc.patch(endpoint, json=body).status_code == 409
    with Session() as db:
        assert db.get(Workbook, wid).columns_config == [
            {**columns[0], "width": 400, "prompt": "After", "reactive": False}, columns[1]]
        assert db.query(Job).count() == 0
    assert tc.get(f"/api/workbooks/{wid}").json()["workbook"]["columns_config"][0]["prompt"] == "After"
    other = _mk_workbook(Session, columns, ws=WS2)
    assert tc.patch(f"/api/workbooks/{other}/columns/agent/settings", json=body).status_code == 404
    assert tc.patch(f"/api/workbooks/{wid}/columns/missing/settings", json=body).status_code == 404


@pytest.mark.parametrize("changes,expected", [
    ({}, {}), ({"name": "New"}, {}), ({"id": "other"}, {"id": "agent"}),
    ({"type": "output"}, {"type": "agent"}), ({"width": 300}, {"width": 180}),
    ({"unknown": True}, {"unknown": None}), ({"name": " "}, {"name": "Agent"}),
    ({"max_tokens": 0}, {"max_tokens": None}), ({"reactive": "false"}, {"reactive": None}),
    ({"policy": {"max_cost_usd": -1}}, {"policy": None}),
])
def test_column_settings_patch_rejects_invalid_without_mutation(client, changes, expected):
    tc, Session, _ = client
    columns = [{"id": "agent", "name": "Agent", "type": "agent", "width": 180}]
    wid = _mk_workbook(Session, columns)
    response = tc.patch(f"/api/workbooks/{wid}/columns/agent/settings", json={"changes": changes, "expected": expected})
    assert response.status_code == 422, response.text
    with Session() as db:
        assert db.get(Workbook, wid).columns_config == columns
        assert db.query(Job).count() == 0


@pytest.mark.parametrize("operation", ["patch", "replace"])
@pytest.mark.parametrize("reference,blocked", [("Company Name", True), ("company_id", False), (" COMPANY NAME ", True)])
def test_rename_preserves_agent_reference_identity(client, operation, reference, blocked):
    tc, Session, _ = client
    columns = [{"id": "company_id", "name": "Company Name", "type": "input"},
               {"id": "research", "name": "Research", "type": "ai_formula", "prompt": f"Find {{{reference}}}"}]
    wid = _mk_workbook(Session, columns)
    if operation == "patch":
        response = tc.patch(f"/api/workbooks/{wid}/columns/company_id/settings", json={
            "changes": {"name": "Organization"}, "expected": {"name": "Company Name"}})
    else:
        response = tc.put(f"/api/workbooks/{wid}", json={"columns_config": [
            {**columns[0], "name": "Organization"}, columns[1]]})
    assert response.status_code == (409 if blocked else 200), response.text
    with Session() as db:
        stored = db.get(Workbook, wid).columns_config
        assert stored[0]["name"] == ("Company Name" if blocked else "Organization")
        assert stored[1]["prompt"] == columns[1]["prompt"]
        assert db.query(Job).count() == 0


def test_column_settings_patch_keeps_explicit_zero_false_and_empty(client):
    tc, Session, _ = client
    columns = [{"id": "agent", "name": "Agent", "type": "agent", "tools": ["paid"]}]
    wid = _mk_workbook(Session, columns)
    changes = {"tools": [], "reactive": False, "goal": "", "policy": {"max_steps": 0, "max_cost_usd": 0}}
    response = tc.patch(f"/api/workbooks/{wid}/columns/agent/settings", json={
        "changes": changes, "expected": {"tools": ["paid"], "reactive": None, "goal": None, "policy": None}})
    assert response.status_code == 200, response.text
    assert response.json()["changes"] == changes
    with Session() as db:
        assert db.get(Workbook, wid).columns_config == [{**columns[0], **changes}]
        assert db.query(Job).count() == 0


def test_add_column_preserves_current_configuration_and_rejects_duplicate_identity(client):
    tc, Session, _ = client
    original = [{"id": "company", "name": "Company", "type": "input", "width": 180,
                 "future_setting": {"keep": True}},
                {"id": "research", "name": "Research", "type": "research", "prompt": "Current prompt"}]
    wid = _mk_workbook(Session, original)
    row = _mk_row(Session, wid, {"company": "Acme"}, enrichments={"research": {"value": "Keep evidence"}})
    assert tc.patch(f"/api/workbooks/{wid}/columns/company/width", json={"width": 400}).status_code == 200
    endpoint = f"/api/workbooks/{wid}/columns"
    body = {"column": {"id": "agent", "name": "Agent", "type": "agent", "tools": [], "reactive": False,
                       "policy": {"max_steps": 0, "max_cost_usd": 0}}}
    response = tc.post(endpoint, json=body)
    assert response.status_code == 200, response.text
    expected = response.json()["columns_config"]
    assert expected[:2] == [{**original[0], "width": 400}, original[1]]
    assert expected[2]["tools"] == []
    assert expected[2]["reactive"] is False
    assert expected[2]["policy"] == {"max_steps": 0, "max_cost_usd": 0}
    assert tc.post(endpoint, json=body).status_code == 409
    assert tc.post(endpoint, json={"column": {**body["column"], "name": "Different"}}).status_code == 409
    other = _mk_workbook(Session, original, ws=WS2)
    assert tc.post(f"/api/workbooks/{other}/columns", json=body).status_code == 404
    with Session() as db:
        assert db.get(Workbook, wid).columns_config == expected
        assert db.get(Workbook, other).columns_config == original
        assert db.get(WorkbookRow, row).enrichments == {"research": {"value": "Keep evidence"}}
        assert db.query(Job).count() == 0


@pytest.mark.parametrize("overrides", [{"id": " "}, {"name": " "}, {"width": 79}, {"width": 601}])
def test_add_column_invalid_metadata_does_not_mutate(client, overrides):
    tc, Session, _ = client
    wid = _mk_workbook(Session, [])
    response = tc.post(f"/api/workbooks/{wid}/columns", json={"column": {
        "id": "company", "name": "Company", "type": "input", **overrides}})
    assert response.status_code == 422
    with Session() as db:
        assert db.get(Workbook, wid).columns_config == []
        assert db.query(Job).count() == 0


def test_remove_column_preserves_other_settings_and_scopes_legacy_cleanup(client):
    tc, Session, _ = client
    columns = [{"id": "a", "name": "A", "type": "ai_formula", "prompt": "Prompt"},
               {"id": "b", "name": "B", "type": "input", "width": 400, "future_setting": True}]
    wid = _mk_workbook(Session, columns)
    other = _mk_workbook(Session, columns, ws=WS2)
    row = _mk_row(Session, wid, {"a": "Retained", "b": "Other"}, enrichments={"a": {"value": "Evidence"}})
    with Session() as db:
        for book, workspace, column in [(wid, WS1, "a"), (wid, WS1, "b"), (other, WS2, "a")]:
            db.add(WorkbookEnrichment(workbook_id=book, workspace_id=workspace, lead_id=1, column_id=column, value="Legacy evidence"))
        db.commit()
    assert tc.delete(f"/api/workbooks/{other}/columns/a").status_code == 404
    response = tc.delete(f"/api/workbooks/{wid}/columns/a")
    assert response.status_code == 200, response.text
    assert response.json()["columns_config"] == [columns[1]]
    assert tc.delete(f"/api/workbooks/{wid}/columns/a").status_code == 404
    with Session() as db:
        assert db.get(Workbook, wid).columns_config == [columns[1]]
        assert db.get(Workbook, other).columns_config == columns
        assert db.get(WorkbookRow, row).data == {"a": "Retained", "b": "Other"}
        assert db.get(WorkbookRow, row).enrichments == {"a": {"value": "Evidence"}}
        assert {(entry.workbook_id, entry.column_id) for entry in db.query(WorkbookEnrichment)} == {(wid, "b"), (other, "a")}
        assert db.query(Job).count() == 0


def test_remove_column_rejects_stale_confirmation_before_cleanup(client):
    tc, Session, _ = client
    column = {"id": "a", "name": "A", "type": "ai_formula", "prompt": "Original", "width": 200}
    wid = _mk_workbook(Session, [column])
    endpoint = f"/api/workbooks/{wid}/columns/a"
    with Session() as db:
        db.get(Workbook, wid).columns_config = [{**column, "prompt": "Updated elsewhere"}]
        db.add(WorkbookEnrichment(workbook_id=wid, workspace_id=WS1, lead_id=1, column_id="a", value="Evidence"))
        db.commit()
    response = tc.request("DELETE", endpoint, json={"expected_column": column})
    assert response.status_code == 409, response.text
    with Session() as db:
        assert db.get(Workbook, wid).columns_config == [{**column, "prompt": "Updated elsewhere"}]
        assert db.query(WorkbookEnrichment).filter_by(workbook_id=wid).count() == 1
    response = tc.request("DELETE", endpoint, json={"expected_column": {**column, "prompt": "Updated elsewhere"}})
    assert response.status_code == 200, response.text
    with Session() as db:
        assert db.get(Workbook, wid).columns_config == []
        assert db.query(WorkbookEnrichment).filter_by(workbook_id=wid).count() == 0


@pytest.mark.parametrize("reference", [
    {"prompt": "Read {source_id}"}, {"formula": "{ SOURCE NAME }"},
    {"goal": "Check {Source Name}"}, {"condition": "{source_id}"},
    {"input_columns": ["source_id"]}, {"http_url": "https://example.test/{source_id}"},
    {"http_headers": {"X-Test": "{source_id}"}},
    {"http_body": {"nested": [{"value": "{source_id}"}]}},
    {"destination_config": {"body": {"value": "{source_id}"}}},
])
def test_remove_referenced_column_rejects_before_cleanup(client, reference):
    tc, Session, _ = client
    source = {"id": "source_id", "name": "Source Name", "type": "research"}
    dependent = {"id": "consumer", "name": "Consumer", "type": "ai_formula", **reference}
    wid = _mk_workbook(Session, [dependent, source])
    with Session() as db:
        db.add(WorkbookEnrichment(workbook_id=wid, workspace_id=WS1, lead_id=1, column_id="source_id", value="Keep evidence"))
        db.commit()
    response = tc.request("DELETE", f"/api/workbooks/{wid}/columns/source_id", json={"expected_column": source})
    assert response.status_code == 409, response.text
    assert "Consumer" in response.json()["detail"]
    with Session() as db:
        assert db.get(Workbook, wid).columns_config == [dependent, source]
        assert db.query(WorkbookEnrichment).filter_by(workbook_id=wid).count() == 1
        assert db.query(Job).count() == 0


@pytest.mark.parametrize("config", [
    {"filters": [{"column": "a", "op": "not_empty"}]},
    {"sort": [{"column": "a", "dir": "asc"}]}, {"hidden_columns": ["a"]},
])
def test_remove_column_referenced_by_saved_view_is_rejected(client, config):
    tc, Session, _ = client
    column = {"id": "a", "name": "A", "type": "input"}
    wid = _mk_workbook(Session, [column])
    created = tc.post(f"/api/v2/workbooks/{wid}/views", json={"name": "Important view", "config": config})
    assert created.status_code == 201
    response = tc.delete(f"/api/workbooks/{wid}/columns/a")
    assert response.status_code == 409
    assert "Important view" in response.json()["detail"]
    with Session() as db:
        assert db.get(Workbook, wid).columns_config == [column]
        assert db.get(WorkbookView, created.json()["id"]).config == created.json()["config"]
    assert tc.delete(f"/api/v2/workbooks/{wid}/views/{created.json()['id']}").status_code == 200
    assert tc.delete(f"/api/workbooks/{wid}/columns/a").status_code == 200


@pytest.mark.parametrize("config", [
    {"filters": [{"column": "missing", "op": "not_empty"}]},
    {"sort": [{"column": "missing", "dir": "asc"}]},
])
def test_missing_view_columns_do_not_silently_expand_action_scope(client, config):
    tc, Session, _ = client
    wid = _mk_workbook(Session, [{"id": "company", "name": "Company", "type": "input"},
                              {"id": "summary", "name": "Summary", "type": "ai_formula", "prompt": "Summarize"}])
    rid = _mk_row(Session, wid, {"company": "Keep"})
    # Legacy stored views can predate reference validation.
    with Session() as db:
        stale = WorkbookView(workbook_id=wid, workspace_id=WS1, name="Stale view", config=config)
        db.add(stale)
        db.commit()
        view = stale.id
    for path in (f"/api/workbooks/{wid}", f"/api/workbooks/{wid}/export.csv", f"/api/workbooks/{wid}/run/estimate"):
        response = tc.get(path, params={"view_id": view})
        assert response.status_code == 409, response.text
    response = tc.post(f"/api/workbooks/{wid}/run", json={"view_id": view})
    assert response.status_code == 409, response.text
    response = tc.post(f"/api/workbooks/{wid}/rows/delete-query", json={
        "view_id": view, "expected_count": 1, "confirmation": "DELETE 1 ROWS"})
    assert response.status_code == 409, response.text
    with Session() as db:
        assert db.get(WorkbookRow, rid).data == {"company": "Keep"}
        assert db.query(Job).count() == 0


def test_remove_column_rejects_ambiguous_identity_without_mutation(client):
    tc, Session, _ = client
    columns = [{"id": "a", "name": "First"}, {"id": "a", "name": "Second"}]
    wid = _mk_workbook(Session, columns)
    assert tc.delete(f"/api/workbooks/{wid}/columns/a").status_code == 409
    with Session() as db:
        assert db.get(Workbook, wid).columns_config == columns


def test_column_width_patch_preserves_execution_config_and_scope(client):
    tc, Session, _ = client
    columns = [{"id": "research", "name": "Research", "type": "research", "width": 180,
                "prompt": "Find {company}", "cell_budget_usd": 0.1},
               {"id": "company", "type": "input", "width": 200}]
    wid = _mk_workbook(Session, columns)
    _mk_row(Session, wid, {"company": "Acme"})
    response = tc.patch(f"/api/workbooks/{wid}/columns/research/width", json={"width": 320})
    assert response.status_code == 200, response.text
    assert response.json() == {"column_id": "research", "width": 320}
    with Session() as db:
        assert db.get(Workbook, wid).columns_config == [{**columns[0], "width": 320}, columns[1]]
        assert db.query(Job).count() == 0
    assert tc.get(f"/api/workbooks/{wid}").json()["workbook"]["columns_config"][0]["width"] == 320
    other = _mk_workbook(Session, columns, ws=WS2)
    assert tc.patch(f"/api/workbooks/{other}/columns/research/width", json={"width": 320}).status_code == 404
    assert tc.patch(f"/api/workbooks/{wid}/columns/missing/width", json={"width": 320}).status_code == 404
    for width in [0, 79, 601, True, 100.5, "120"]:
        assert tc.patch(f"/api/workbooks/{wid}/columns/research/width", json={"width": width}).status_code == 422


def test_targeted_column_order_preserves_latest_settings_and_rejects_stale_order(client):
    tc, Session, _ = client
    columns = [{"id": "a", "name": "A", "width": 320, "policy": {"max_cost_usd": 0.01},
                "future_setting": {"keep": True}}, {"id": "b", "name": "B", "width": 100}]
    wid = _mk_workbook(Session, columns)
    endpoint = f"/api/workbooks/{wid}/columns/order"
    body = {"expected_column_ids": ["a", "b"], "column_ids": ["b", "a"]}
    # A separate edit after the client read must not be reverted by its reorder.
    assert tc.patch(f"/api/workbooks/{wid}/columns/a/width", json={"width": 400}).status_code == 200
    response = tc.patch(endpoint, json=body)
    assert response.status_code == 200, response.text
    assert response.json() == {"column_ids": ["b", "a"]}
    expected = [columns[1], {**columns[0], "width": 400}]
    with Session() as db:
        assert db.get(Workbook, wid).columns_config == expected
        assert db.query(Job).count() == 0
    assert tc.patch(endpoint, json=body).status_code == 409
    for ids in [["a"], ["a", "a"], ["a", "foreign"]]:
        assert tc.patch(endpoint, json={"expected_column_ids": ["b", "a"], "column_ids": ids}).status_code == 422
    assert tc.patch(endpoint, json={**body, "columns_config": columns}).status_code == 422
    other = _mk_workbook(Session, columns, ws=WS2)
    assert tc.patch(f"/api/workbooks/{other}/columns/order", json=body).status_code == 404
    with Session() as db:
        assert db.get(Workbook, wid).columns_config == expected
        assert db.get(Workbook, other).columns_config == columns


def test_column_reorder_persists_configuration_without_mutating_rows(client):
    tc, Session, _ = client
    columns = [
        {"id": "company", "name": "Company", "type": "input", "width": 320},
        {"id": "research", "name": "Research", "type": "research", "width": 100,
         "prompt": "Find {company}", "cell_budget_usd": 0.1},
    ]
    wid = _mk_workbook(Session, columns)
    evidence = {"research": {"value": "Evidence-backed answer", "status": "done"}}
    row_id = _mk_row(Session, wid, {"company": "Acme"}, enrichments=evidence)
    reordered = list(reversed(columns))
    response = tc.put(f"/api/workbooks/{wid}", json={"columns_config": reordered})
    assert response.status_code == 200, response.text
    with Session() as db:
        stored = db.get(Workbook, wid).columns_config
        assert [column["id"] for column in stored] == ["research", "company"]
        for actual, expected in zip(stored, reordered):
            assert {key: actual[key] for key in expected} == expected
        row = db.get(WorkbookRow, row_id)
        assert row.data == {"company": "Acme"}
        assert row.enrichments == evidence
        assert db.query(Job).count() == 0
    assert tc.get(f"/api/workbooks/{wid}").json()["workbook"]["columns_config"] == stored
    for invalid_budget in [-0.01, "NaN", "Infinity"]:
        invalid = [{**reordered[0], "cell_budget_usd": invalid_budget}, reordered[1]]
        assert tc.put(f"/api/workbooks/{wid}", json={"columns_config": invalid}).status_code == 422
    with Session() as db:
        assert db.get(Workbook, wid).columns_config == stored
    other = _mk_workbook(Session, columns, ws=WS2)
    assert tc.put(f"/api/workbooks/{other}", json={"columns_config": reordered}).status_code == 404
    with Session() as db:
        assert db.get(Workbook, other).columns_config == columns


@pytest.mark.parametrize("config", [
    {"type": "agent", "goal": "Find current partnership leader", "tools": ["provider_b", "provider_a"],
     "policy": {"max_steps": 3, "max_cost_usd": 0.02, "prefer": "quality"}},
    {"type": "agent", "goal": "Disabled budget", "tools": [],
     "policy": {"max_steps": 0, "max_cost_usd": 0}},
    {"type": "agent", "policy": {}},
    {"type": "output", "run_once": False, "destination": "webhook",
     "destination_config": {"url": "https://example.com/webhook"}},
    {"type": "ai_formula", "prompt": "Summarize {company}", "max_tokens": 256},
])
@pytest.mark.parametrize("operation", ["create", "update"])
def test_execution_column_settings_survive_save(client, config, operation):
    tc, Session, _ = client
    column = {"id": "execution", "name": "Execution", "width": 180, **config}
    if operation == "create":
        response = tc.post("/api/workbooks/", json={"name": "Execution settings", "source": "empty", "columns_config": [column]})
        assert response.status_code == 201, response.text
        wid = response.json()["id"]
    else:
        wid = _mk_workbook(Session, [column])
        response = tc.put(f"/api/workbooks/{wid}", json={"columns_config": [column]})
        assert response.status_code == 200, response.text
    with Session() as db:
        actual = db.get(Workbook, wid).columns_config[0]
        assert {key: actual[key] for key in column} == column
        assert db.query(Job).count() == 0
    assert tc.get(f"/api/workbooks/{wid}").json()["workbook"]["columns_config"][0] == actual


@pytest.mark.parametrize("config", [
    {"policy": {"max_steps": -1}}, {"policy": {"max_steps": 1.5}},
    {"policy": {"max_cost_usd": -0.01}}, {"policy": {"max_cost_usd": "Infinity"}},
    {"policy": {"max_cost_usd": "NaN"}}, {"max_tokens": 0},
])
def test_invalid_execution_settings_reject_without_mutation(client, config):
    tc, Session, _ = client
    column = {"id": "execution", "name": "Execution", "type": "agent", "width": 180}
    wid = _mk_workbook(Session, [column])
    assert tc.put(f"/api/workbooks/{wid}", json={"columns_config": [{**column, **config}]}).status_code == 422
    with Session() as db:
        assert db.get(Workbook, wid).columns_config == [column]
        assert db.query(Job).count() == 0


@pytest.mark.parametrize("configured_tools", [[], ["chosen_provider"], None])
def test_agent_provider_candidates_respect_explicit_selection(client, monkeypatch, configured_tools):
    import asyncio
    from apps.api.services.workbook import agent_column, enrichment
    _, Session, _ = client
    wid = _mk_workbook(Session, [])
    candidates = []
    monkeypatch.setitem(enrichment.DEFAULT_WATERFALLS, "email", ["default_provider"])
    def capture_plan(db, target, tools, **kwargs):
        candidates.append(list(tools))
        return []
    monkeypatch.setattr(agent_column._planner, "order_chain", capture_plan)
    monkeypatch.setattr(agent_column, "_save_trace", lambda *args: None)
    def unexpected_provider(*args):
        pytest.fail("An exhausted plan must not call a provider")
    monkeypatch.setattr(agent_column, "get_provider", unexpected_provider)
    with Session() as db:
        result = asyncio.run(agent_column.run_agent_cell(db, wid, 1,
            {"id": "agent", "type": "agent", "tools": configured_tools},
            {"id": 1, "company": "Acme"}))
    assert candidates == [configured_tools if configured_tools is not None else ["default_provider"]]
    assert result["trace"]["outcome"] == "exhausted"
    assert result["trace"]["spent"] == 0


@pytest.mark.parametrize("cell_limit,workbook_limit,already_spent,allowed", [
    (0, 0, 0, False), (0.01, 0, 0, False), (0.1, 0.03, 0.02, False),
    (0.1, 0.03, 0.03, False), (0.02, 0, 0, True),
])
def test_agent_paid_lookup_respects_cell_and_workbook_headroom(client, monkeypatch, cell_limit, workbook_limit, already_spent, allowed):
    import asyncio
    from types import SimpleNamespace
    from apps.api.services.workbook import agent_column
    _, Session, _ = client
    wid = _mk_workbook(Session, [])
    with Session() as db:
        wb = db.get(Workbook, wid)
        wb.budget_max_usd = workbook_limit
        wb.budget_spent_usd = already_spent
        db.commit()
    calls = []
    class Provider:
        default_confidence = 0.9
        async def enrich(self, lead):
            calls.append("paid")
            return SimpleNamespace(success=True, fields={"email": "fixture@example.com"}, confidence=0.9)
    monkeypatch.setattr(agent_column, "get_provider", lambda name: Provider())
    async def isolated_provider(name, lead, timeout):
        result = await Provider().enrich(lead)
        return {"provider": name, "success": result.success, "fields": result.fields, "confidence": result.confidence}
    monkeypatch.setattr(agent_column, "run_provider", isolated_provider)
    monkeypatch.setattr(agent_column._planner, "provider_cost", lambda name: 0.02)
    monkeypatch.setattr(agent_column._planner, "is_paid", lambda name: True)
    monkeypatch.setattr(agent_column, "_save_trace", lambda *args: None)
    with Session() as db:
        result = asyncio.run(agent_column.run_agent_cell(db, wid, 1,
            {"id": "agent", "tools": ["paid"], "policy": {"max_cost_usd": cell_limit}},
            {"id": 1, "company": "Acme"}))
        db.commit()
    assert calls == (["paid"] if allowed else [])
    assert result["trace"]["outcome"] == ("found" if allowed else "budget")
    assert result["trace"]["spent"] == (0.02 if allowed else 0)
    with Session() as db:
        assert db.get(Workbook, wid).budget_spent_usd == pytest.approx(already_spent + (0.02 if allowed else 0))


def test_agent_uses_bounded_runner_and_records_timeout(client, monkeypatch):
    import asyncio
    from apps.api.services.workbook import agent_column
    _, Session, _ = client
    wid = _mk_workbook(Session, [])
    calls = []
    class Provider:
        default_confidence = 0.9
        async def enrich(self, lead):
            pytest.fail("Agent must not invoke providers directly")
    async def runner(name, lead, timeout):
        calls.append((name, timeout))
        raise asyncio.TimeoutError()
    monkeypatch.setattr(agent_column, "get_provider", lambda name: Provider())
    monkeypatch.setattr(agent_column, "run_provider", runner)
    monkeypatch.setattr(agent_column._planner, "order_chain", lambda db, target, tools, **kwargs: list(tools))
    monkeypatch.setattr(agent_column, "_save_trace", lambda *args: None)
    with Session() as db:
        result = asyncio.run(agent_column.run_agent_cell(db, wid, 1,
            {"id": "agent", "tools": ["fixture"]}, {"id": 1, "company": "Acme"}, provider_timeout=0.25))
    assert calls == [("fixture", 0.25)]
    assert result["trace"]["steps"][0]["reason"] == "timeout"
    assert result["_provider_attempts"][0]["timed_out"] is True


def test_wide_paste_persists_exact_fields_and_readback(client):
    tc, Session, _ = client
    columns = [{"id": f"field_{i}", "name": f"Field {i}", "type": "input"} for i in range(50)]
    wid = _mk_workbook(Session, columns)
    rid = _mk_row(Session, wid, {"untouched": "preserved"})
    fields = {f"field_{i}": f"Value {i} — 日本語" for i in range(50)}
    fields.update(field_0=0, field_1=False, field_2="")
    response = tc.patch(f"/api/workbooks/{wid}/rows?recompute=false", json={"updates": [{"row_id": rid, "fields": fields}]})
    assert response.status_code == 200, response.text
    assert response.json()["updated_rows"] == 1
    reloaded = tc.get(f"/api/workbooks/{wid}")
    assert reloaded.status_code == 200, reloaded.text
    assert reloaded.json()["rows"][0]["data"] == {"untouched": "preserved", **fields}
    with Session() as db:
        assert db.get(WorkbookRow, rid).data == {"untouched": "preserved", **fields}
        assert db.query(Job).count() == 0


@pytest.mark.parametrize("other_workspace", [WS1, WS2])
def test_paste_rejects_foreign_row_without_partial_write(client, other_workspace):
    tc, Session, _ = client
    columns = [{"id": "company", "type": "input"}]
    wid = _mk_workbook(Session, columns)
    other = _mk_workbook(Session, columns, ws=other_workspace)
    own_row = _mk_row(Session, wid, {"company": "Original"})
    foreign_row = _mk_row(Session, other, {"company": "Foreign"}, ws=other_workspace)
    response = tc.patch(f"/api/workbooks/{wid}/rows", json={"updates": [
        {"row_id": own_row, "fields": {"company": "Changed"}},
        {"row_id": foreign_row, "fields": {"company": "Forbidden"}},
    ]})
    assert response.status_code == 404
    with Session() as db:
        assert db.get(WorkbookRow, own_row).data == {"company": "Original"}
        assert db.get(WorkbookRow, foreign_row).data == {"company": "Foreign"}
        assert db.query(Job).count() == 0


def test_saved_view_filters_and_sorts_before_pagination(client):
    tc, Session, _ = client
    wid = _mk_workbook(Session, [
        {"id": "company", "name": "Company", "type": "input", "lead_field": "company"},
    ])
    for company in ["Acme Alpha", "Other", "Acme Gamma", "Acme Beta", "Elsewhere"]:
        _mk_row(Session, wid, {"company": company})
    created = tc.post(f"/api/v2/workbooks/{wid}/views", json={
        "name": "Acme descending",
        "config": {
            "filters": [{"column": "company", "op": "contains", "value": "acme"}],
            "sort": [{"column": "company", "dir": "desc"}],
            "hidden_columns": [],
        },
    })
    assert created.status_code == 201, created.text

    response = tc.get(f"/api/workbooks/{wid}", params={
        "page": 1, "page_size": 2, "view_id": created.json()["id"],
    })

    assert response.status_code == 200, response.text
    payload = response.json()
    assert payload["total_rows"] == 5
    assert payload["query_total_rows"] == 3
    assert [row["data"]["company"] for row in payload["rows"]] == ["Acme Gamma", "Acme Beta"]

    searched = tc.get(f"/api/workbooks/{wid}", params={"search": "alpha"})
    assert searched.status_code == 200
    assert searched.json()["query_total_rows"] == 1
    assert searched.json()["rows"][0]["data"]["company"] == "Acme Alpha"

    wildcard = tc.get(f"/api/workbooks/{wid}", params={"search": "%"})
    assert wildcard.status_code == 200
    assert wildcard.json()["query_total_rows"] == 0

    exported = tc.get(f"/api/workbooks/{wid}/export.csv", params={"view_id": created.json()["id"]})
    assert exported.status_code == 200, exported.text
    assert "attachment;" in exported.headers["content-disposition"]
    records = list(csv.reader(io.StringIO(exported.content.decode("utf-8-sig"))))
    assert records == [["Company"], ["Acme Gamma"], ["Acme Beta"], ["Acme Alpha"]]


def test_csv_export_neutralizes_spreadsheet_formulas(client):
    tc, Session, _ = client
    wid = _mk_workbook(Session, [
        {"id": "company", "name": "Company", "type": "input", "lead_field": "company"},
    ])
    _mk_row(Session, wid, {"company": "=HYPERLINK(\"https://bad.example\")"})

    response = tc.get(f"/api/workbooks/{wid}/export.csv")

    assert response.status_code == 200
    records = list(csv.reader(io.StringIO(response.content.decode("utf-8-sig"))))
    assert records[1][0].startswith("'=HYPERLINK")


def test_csv_export_can_scope_to_selected_rows_across_pages(client):
    tc, Session, _ = client
    wid = _mk_workbook(Session, [
        {"id": "company", "name": "Company", "type": "input", "lead_field": "company"},
    ])
    first = _mk_row(Session, wid, {"company": "First"})
    _mk_row(Session, wid, {"company": "Middle"})
    last = _mk_row(Session, wid, {"company": "Last"})

    response = tc.get(f"/api/workbooks/{wid}/export.csv", params=[
        ("row_ids", first), ("row_ids", last),
    ])

    assert response.status_code == 200, response.text
    records = list(csv.reader(io.StringIO(response.content.decode("utf-8-sig"))))
    assert records == [["Company"], ["First"], ["Last"]]


@pytest.mark.parametrize("selection,expected", [
    ({"waterfall": []}, []),
    ({"waterfall": [], "provider": "hunter_io"}, []),
    ({"waterfall": ["prospeo", "hunter_io"]}, ["prospeo", "hunter_io"]),
    ({"provider": "hunter_io"}, ["hunter_io"]),
    ({"waterfall": None}, ["fixture_default"]),
    ({}, ["fixture_default"]),
])
def test_run_estimate_preserves_explicit_provider_selection(client, monkeypatch, selection, expected):
    from apps.api.services.workbook import vendor_catalog, enrichment
    tc, Session, _ = client
    wid = _mk_workbook(Session, [{"id": "email", "name": "Email", "type": "waterfall",
                                 "target_field": "email", **selection}])
    _mk_row(Session, wid, {"company": "Example"})
    monkeypatch.setitem(enrichment.DEFAULT_WATERFALLS, "email", ["fixture_default"])
    captured = []
    def estimate(rows, providers):
        captured.append((rows, providers))
        return {"rows": rows, "best_usd": 0, "worst_usd": 0, "breakdown": [], "note": "Fixture"}
    monkeypatch.setattr(vendor_catalog, "estimate_run_cost", estimate)
    response = tc.get(f"/api/workbooks/{wid}/run/estimate")
    assert response.status_code == 200, response.text
    assert captured == [(1, {"email": expected})]
    from apps.api.services.billing import service as billing
    billed = []
    monkeypatch.setattr(billing, "billing_enabled", lambda: True)
    def project(rows, providers):
        billed.append((rows, providers))
        return 0
    monkeypatch.setattr(billing, "projected_platform_cost", project)
    monkeypatch.setattr(billing, "check_and_debit", lambda *args, **kwargs: None)
    response = tc.post(f"/api/workbooks/{wid}/run", json={})
    assert response.status_code == 200, response.text
    assert billed == captured


def test_run_and_estimate_scope_to_complete_saved_view_query(client):
    tc, Session, _ = client
    wid = _mk_workbook(Session, [
        {"id": "company", "name": "Company", "type": "input", "lead_field": "company"},
        {"id": "normalized", "name": "Normalized", "type": "formula", "formula": "lower({company})"},
    ])
    matching = [
        _mk_row(Session, wid, {"company": "Acme Alpha"}),
        _mk_row(Session, wid, {"company": "Acme Beta"}),
    ]
    _mk_row(Session, wid, {"company": "Other"})
    created = tc.post(f"/api/v2/workbooks/{wid}/views", json={
        "name": "Acme",
        "config": {"filters": [{"column": "company", "op": "contains", "value": "acme"}]},
    })
    view_id = created.json()["id"]

    estimate = tc.get(f"/api/workbooks/{wid}/run/estimate", params={"view_id": view_id})
    assert estimate.status_code == 200
    assert estimate.json()["rows"] == 2

    response = tc.post(f"/api/workbooks/{wid}/run", json={
        "view_id": view_id, "search": "beta", "fill_missing": True, "expected_rows": 1,
    })
    assert response.status_code == 200, response.text
    assert response.json()["total_jobs"] == 1
    with Session() as session:
        job = session.query(Job).filter(Job.type == "run_workbook").one()
        assert job.payload["row_ids"] == [matching[1]]
        assert job.payload["fill_missing"] is True


def test_run_cycle_rejected_before_billing_but_exact_safe_scope_allowed(client, monkeypatch):
    tc, Session, _ = client
    columns = [{"id": "a", "type": "formula", "formula": "{b}"},
               {"id": "b", "type": "formula", "formula": "{a}"},
               {"id": "safe", "type": "formula", "formula": "1"}]
    wid = _mk_workbook(Session, columns)
    rid = _mk_row(Session, wid, {"company": "Fixture"})
    from apps.api.services.billing import service as billing
    calls = []
    monkeypatch.setattr(billing, "billing_enabled", lambda: calls.append("billing") or False)
    rejected = tc.post(f"/api/workbooks/{wid}/run", json={"column_ids": ["a"]})
    assert rejected.status_code == 422
    assert "circular reference: a" in rejected.json()["detail"]
    assert calls == []
    with Session() as db:
        assert db.query(Job).count() == 0
        assert db.get(Workbook, wid).status == "draft"
    accepted = tc.post(f"/api/workbooks/{wid}/run", json={"row_ids": [rid], "row_columns": {str(rid): ["safe"]}})
    assert accepted.status_code == 200, accepted.text
    with Session() as db:
        assert db.query(Job).one().payload["row_columns"] == {str(rid): ["safe"]}


def test_reviewed_run_count_drift_rejected_before_billing_or_enqueue(client, monkeypatch):
    tc, Session, _ = client
    wid = _mk_workbook(Session, [
        {"id": "company", "name": "Company", "type": "input", "lead_field": "company"},
        {"id": "normalized", "name": "Normalized", "type": "formula", "formula": "lower({company})"},
    ])
    row = _mk_row(Session, wid, {"company": "Acme"})
    from apps.api.services.billing import service as billing
    calls = []
    monkeypatch.setattr(billing, "billing_enabled", lambda: calls.append("billing") or False)
    stale = tc.post(f"/api/workbooks/{wid}/run", json={"expected_rows": 2})
    assert stale.status_code == 409, stale.text
    assert "row count changed" in stale.json()["detail"]
    assert calls == []
    with Session() as session:
        assert session.query(Job).count() == 0
        assert session.get(Workbook, wid).status == "draft"
    accepted = tc.post(f"/api/workbooks/{wid}/run", json={"expected_rows": 1, "fill_missing": True})
    assert accepted.status_code == 200, accepted.text
    with Session() as session:
        job = session.query(Job).one()
        assert job.payload["row_ids"] == [row]
        assert accepted.json()["job_id"] == job.id
        assert accepted.json()["run_id"] == job.payload["run_id"]


def test_run_history_is_workspace_scoped_paginated_and_payload_allowlisted(client):
    tc, Session, _ = client
    wid = _mk_workbook(Session, [])
    other = _mk_workbook(Session, [])
    foreign = _mk_workbook(Session, [], ws=WS2)
    with Session() as db:
        ids = []
        for i in range(3):
            job = Job(type="run_workbook", workspace_id=WS1, status="pending", payload={
                "workbook_id": wid, "row_ids": [1, 2], "column_ids": ["email"],
                "run_id": f"run-{i}", "secret": "must-not-leak", "force": False,
                "output_attempts": {
                    "private-cell": {"state": "dispatch_claimed", "contract_hash": "private-hash"},
                    "sent": {"state": "recorded", "result": {"success": True, "value": "private-receipt"}},
                    "failed": {"state": "recorded", "result": {"success": False, "error": "private-error"}},
                    "bad": {"state": "recorded", "result": {"success": "true"}},
                },
            })
            db.add(job)
            db.flush()
            ids.append(job.id)
        db.add_all([
            Job(type="run_workbook", workspace_id=WS1, payload={"workbook_id": other}),
            Job(type="run_workbook", workspace_id=WS2, payload={"workbook_id": wid}),
            Job(type="source_workbook", workspace_id=WS1, payload={"workbook_id": wid}),
        ])
        db.commit()
    first = tc.get(f"/api/workbooks/{wid}/runs", params={"limit": 2})
    assert first.status_code == 200, first.text
    assert [run["job_id"] for run in first.json()["runs"]] == ids[::-1][:2]
    assert first.json()["has_more"] is True
    assert "must-not-leak" not in first.text and "row_ids" not in first.text
    assert first.json()["runs"][0]["row_count"] == 2
    assert first.json()["runs"][0]["output_attempts"] == {
        "total": 4, "awaiting_receipt": 1, "succeeded": 1, "failed": 1, "unknown": 1}
    assert "private-" not in first.text
    assert first.json()["runs"][0]["created_at"].endswith("+00:00")
    tail = tc.get(f"/api/workbooks/{wid}/runs", params={"before_id": first.json()["next_before_id"]})
    assert [run["job_id"] for run in tail.json()["runs"]] == ids[:1]
    assert tail.json()["has_more"] is False
    assert tc.get(f"/api/workbooks/{foreign}/runs").status_code == 404


def test_router_to_worker_to_persisted_run_receipt_with_real_formulas(client, monkeypatch):
    """Actual formula execution and DB writes; subprocess/Redis/pool are isolated."""
    import asyncio
    from apps.api.services import queue_service as queue_module, job_process_runner
    from apps.api.services.workbook import enrichment, provider_runner, run_receipts
    tc, Session, _ = client
    for module in (queue_module, enrichment, run_receipts):
        monkeypatch.setattr(module, "SessionLocal", Session)
    monkeypatch.setattr(enrichment, "_make_redis", lambda: None)
    monkeypatch.setattr(provider_runner, "ensure_workers", lambda *_: None)
    monkeypatch.setattr(enrichment, "flush_row_change_emits", lambda *_: None)
    wid = _mk_workbook(Session, [
        {"id": "zero", "name": "Zero", "type": "formula", "formula": "1 - 1"},
        {"id": "false", "name": "False", "type": "formula", "formula": "1 > 2"},
        {"id": "invalid", "name": "Invalid", "type": "formula", "formula": "unknown_function(1)"},
    ])
    rid = _mk_row(Session, wid, {"company": "Acme"})
    response = tc.post(f"/api/workbooks/{wid}/run", json={"expected_rows": 1})
    assert response.status_code == 200, response.text
    queue = queue_module.QueueService()
    queue.register_handler("run_workbook", enrichment.handle_run_workbook)

    async def in_process_handler(job_id, job_type, payload, **kwargs):
        assert job_type == "run_workbook"
        await enrichment.handle_run_workbook(job_id, payload)

    monkeypatch.setattr(job_process_runner, "run_job_subprocess", in_process_handler)
    claimed = queue.claim_next_job()
    assert claimed["id"] == response.json()["job_id"]
    asyncio.run(queue._process_job(claimed["id"], "run_workbook", claimed["payload"], locked_at=claimed["locked_at"]))
    receipt = tc.get(f"/api/workbooks/{wid}/runs").json()["runs"][0]
    assert receipt["status"] == "completed", receipt
    assert receipt["result"]["completed"] == 2, receipt
    assert receipt["result"]["errors"] == 1
    assert receipt["result"]["total"] == 3
    assert receipt["run_id"] == response.json()["run_id"]
    with Session() as db:
        row = db.get(WorkbookRow, rid)
        assert row.enrichments["zero"]["value"] == 0
        assert row.enrichments["zero"]["status"] == "complete"
        assert row.enrichments["false"]["value"] == "false"
        assert row.enrichments["invalid"]["status"] == "error"
    reloaded = tc.get(f"/api/workbooks/{wid}")
    assert reloaded.status_code == 200, reloaded.text
    assert reloaded.json()["rows"][0]["enrichments"]["zero"]["value"] == 0
    assert reloaded.json()["rows"][0]["enrichments"]["invalid"]["status"] == "error"


def test_run_result_receipt_requires_current_lease_and_survives_reload(client, monkeypatch):
    from datetime import datetime, timedelta, timezone
    from apps.api.services.workbook import run_receipts
    tc, Session, _ = client
    monkeypatch.setattr(run_receipts, "SessionLocal", Session)
    wid = _mk_workbook(Session, [])
    locked = datetime.now(timezone.utc)
    payload = {"workbook_id": wid, "workspace_id": WS1, "row_ids": [1, 2], "column_ids": ["email", "name"]}
    with Session() as db:
        job = Job(type="run_workbook", workspace_id=WS1, status="processing", payload=payload, worker_id="owner", locked_at=locked)
        db.add(job)
        db.commit()
        jid = job.id
    result = {"completed": 3, "errors": 1, "total": 4, "rows": 2, "stopped": False, "secret": "excluded"}
    assert not run_receipts.persist_run_result(jid, payload, result)
    leased = {**payload, "__queue_lease": {"worker_id": "owner", "locked_at": locked.isoformat()}}
    assert not run_receipts.persist_run_result(jid, {**leased, "workspace_id": None}, result)
    assert not run_receipts.persist_run_result(jid, leased, {**result, "errors": -1})
    assert run_receipts.persist_run_result(jid, leased, result)
    receipt = tc.get(f"/api/workbooks/{wid}/runs").json()["runs"][0]
    assert receipt["result"]["errors"] == 1
    assert "secret" not in receipt["result"]
    with Session() as db:
        job = db.get(Job, jid)
        job.locked_at = locked + timedelta(seconds=1)
        job.retry_count = 1
        db.commit()
    assert not run_receipts.persist_run_result(jid, leased, {**result, "errors": 0})
    assert tc.get(f"/api/workbooks/{wid}/runs").json()["runs"][0]["result"] is None
    leased["__queue_lease"]["locked_at"] = (locked + timedelta(seconds=1)).isoformat()
    assert not run_receipts.persist_run_result(jid, {**leased, "workspace_id": WS2}, result)
    assert run_receipts.persist_run_result(jid, leased, result)
    with Session() as db:
        db.get(Job, jid).status = "cancelled"
        db.commit()
    assert run_receipts.persist_run_result(jid, leased, {**result, "stopped": True})
    assert tc.get(f"/api/workbooks/{wid}/runs").json()["runs"][0]["status"] == "cancelled"
    with Session() as db:
        db.get(Job, jid).status = "completed"
        db.commit()
    assert not run_receipts.persist_run_result(jid, leased, result)
    receipt = tc.get(f"/api/workbooks/{wid}/runs").json()["runs"][0]
    assert receipt["status"] == "completed" and receipt["result"]["errors"] == 1


def test_selected_row_delete_is_limited_to_owned_workbook(client):
    tc, Session, _ = client
    wid = _mk_workbook(Session, [])
    other = _mk_workbook(Session, [])
    foreign = _mk_workbook(Session, [], ws=WS2)
    selected = _mk_row(Session, wid, {"company": "Selected"})
    survivor = _mk_row(Session, wid, {"company": "Unselected"})
    other_row = _mk_row(Session, other, {"company": "Other workbook"})
    foreign_row = _mk_row(Session, foreign, {"company": "Other workspace"}, ws=WS2)
    response = tc.request("DELETE", f"/api/workbooks/{wid}/rows", json={
        "row_ids": [selected, other_row, foreign_row],
    })
    assert response.status_code == 200, response.text
    assert response.json() == {"deleted": 1}
    with Session() as session:
        assert {row.id for row in session.query(WorkbookRow).all()} == {survivor, other_row, foreign_row}
    denied = tc.request("DELETE", f"/api/workbooks/{foreign}/rows", json={"row_ids": [foreign_row]})
    assert denied.status_code == 404
    with Session() as session:
        assert session.get(WorkbookRow, foreign_row) is not None


def test_count_locked_query_delete_is_exact_and_detects_drift(client):
    tc, Session, _ = client
    wid = _mk_workbook(Session, [
        {"id": "company", "name": "Company", "type": "input", "lead_field": "company"},
    ])
    _mk_row(Session, wid, {"company": "Acme Alpha"})
    _mk_row(Session, wid, {"company": "Acme Beta"})
    survivor = _mk_row(Session, wid, {"company": "Other"})

    stale = tc.post(f"/api/workbooks/{wid}/rows/delete-query", json={
        "search": "acme", "expected_count": 1, "confirmation": "DELETE 1 ROWS",
    })
    assert stale.status_code == 409
    with Session() as session:
        assert session.query(WorkbookRow).filter_by(workbook_id=wid).count() == 3

    deleted = tc.post(f"/api/workbooks/{wid}/rows/delete-query", json={
        "search": "acme", "expected_count": 2, "confirmation": "DELETE 2 ROWS",
    })
    assert deleted.status_code == 200, deleted.text
    assert deleted.json() == {"deleted": 2, "matched": 2}
    with Session() as session:
        remaining = session.query(WorkbookRow).filter_by(workbook_id=wid).all()
        assert [row.id for row in remaining] == [survivor]


def test_stable_row_cursor_handles_duplicate_positions(client):
    tc, Session, _ = client
    wid = _mk_workbook(Session, [
        {"id": "company", "name": "Company", "type": "input", "lead_field": "company"},
    ])
    expected_ids = [_mk_row(Session, wid, {"company": name}) for name in ("A", "B", "C")]

    first = tc.get(f"/api/workbooks/{wid}", params={"cursor_mode": True, "page_size": 2})
    assert first.status_code == 200, first.text
    first_payload = first.json()
    assert [row["row_id"] for row in first_payload["rows"]] == expected_ids[:2]
    assert first_payload["has_more"] is True
    assert first_payload["next_cursor"]

    second = tc.get(f"/api/workbooks/{wid}", params={
        "cursor_mode": True, "cursor": first_payload["next_cursor"], "page_size": 2,
    })
    assert second.status_code == 200, second.text
    assert [row["row_id"] for row in second.json()["rows"]] == expected_ids[2:]
    assert second.json()["has_more"] is False
    assert second.json()["next_cursor"] is None
    assert tc.get(f"/api/workbooks/{wid}", params={"cursor_mode": True, "cursor": "bad"}).status_code == 400


def test_saved_view_custom_sort_supports_stable_cursor_paging(client):
    tc, Session, _ = client
    wid = _mk_workbook(Session, [
        {"id": "company", "name": "Company", "type": "input", "lead_field": "company"},
    ])
    for company in ("Beta", "Alpha", "Gamma", "Beta", "Delta"):
        _mk_row(Session, wid, {"company": company})
    created = tc.post(f"/api/v2/workbooks/{wid}/views", json={
        "name": "Company descending",
        "config": {"sort": [{"column": "company", "dir": "desc"}]},
    })
    view_id = created.json()["id"]

    seen = []
    cursor = None
    while True:
        params = {"cursor_mode": True, "page_size": 2, "view_id": view_id}
        if cursor:
            params["cursor"] = cursor
        response = tc.get(f"/api/workbooks/{wid}", params=params)
        assert response.status_code == 200, response.text
        payload = response.json()
        seen.extend(row["data"]["company"] for row in payload["rows"])
        cursor = payload["next_cursor"]
        if not cursor:
            break

    assert seen == ["Gamma", "Delta", "Beta", "Beta", "Alpha"]
    legacy_cursor = tc.get(f"/api/workbooks/{wid}", params={
        "cursor_mode": True, "page_size": 1,
    }).json()["next_cursor"]
    rejected = tc.get(f"/api/workbooks/{wid}", params={
        "cursor_mode": True, "cursor": legacy_cursor, "view_id": view_id,
    })
    assert rejected.status_code == 400


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
    wid = _mk_workbook(Session, [{"id": key, "name": key, "type": "input"}
                                 for key in ("email", "company", "score", "notes", "source")])

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


@pytest.mark.parametrize("config", [
    {"filters": [{"column": "missing", "op": "not_empty"}]},
    {"sort": [{"column": "missing", "dir": "asc"}]},
    {"hidden_columns": ["missing"]},
])
def test_view_missing_references_rejected_without_mutation(client, config):
    tc, Session, _ = client
    wid = _mk_workbook(Session, [{"id": "company", "name": "Company", "type": "input"}])
    base = f"/api/v2/workbooks/{wid}/views"
    response = tc.post(base, json={"name": "Invalid", "config": config})
    assert response.status_code == 422, response.text
    assert "missing" in response.json()["detail"]
    created = tc.post(base, json={"name": "Keep"}).json()
    response = tc.put(f"{base}/{created['id']}", json={"name": "Changed", "config": config})
    assert response.status_code == 422, response.text
    assert tc.get(base).json()["views"] == [created]
    valid = {"filters": [], "sort": [], "hidden_columns": ["company"]}
    response = tc.put(f"{base}/{created['id']}", json={"config": valid})
    assert response.status_code == 200, response.text
    assert response.json()["config"] == valid
    with Session() as db:
        assert db.query(Job).count() == 0


@pytest.mark.parametrize("reference", ["filter", "sort", "hidden", "prompt"])
def test_full_column_replacement_cannot_bypass_reference_guards(client, reference):
    tc, Session, _ = client
    columns = [{"id": "company", "name": "Company", "type": "input"},
               {"id": "summary", "name": "Summary", "type": "ai_formula", "prompt": "Find {company}"}]
    wid = _mk_workbook(Session, columns)
    config = {
        "filter": {"filters": [{"column": "company", "op": "not_empty"}]},
        "sort": {"sort": [{"column": "company", "dir": "asc"}]},
        "hidden": {"hidden_columns": ["company"]},
        "prompt": {},
    }[reference]
    created = tc.post(f"/api/v2/workbooks/{wid}/views", json={"name": "Keep", "config": config})
    assert created.status_code == 201, created.text
    remaining = dict(columns[1])
    if reference != "prompt":
        remaining["prompt"] = "No references"
    response = tc.put(f"/api/workbooks/{wid}", json={"name": "Must not save", "columns_config": [remaining]})
    assert response.status_code == 409, response.text
    with Session() as db:
        wb = db.get(Workbook, wid)
        assert wb.columns_config == columns
        assert wb.name != "Must not save"
        assert db.query(Job).count() == 0


def test_full_column_replacement_rejects_duplicate_identity(client):
    tc, Session, _ = client
    column = {"id": "company", "name": "Company", "type": "input"}
    wid = _mk_workbook(Session, [column])
    response = tc.put(f"/api/workbooks/{wid}", json={"columns_config": [column, column]})
    assert response.status_code == 422, response.text
    with Session() as db:
        assert db.get(Workbook, wid).columns_config == [column]


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
    app.dependency_overrides[require_editor] = lambda: _ctx(WS2)
    try:
        assert tc.get(f"/api/v2/workbooks/{wid}/views").status_code == 404
        assert tc.post(f"/api/v2/workbooks/{wid}/views", json={"name": "x"}).status_code == 404
        assert tc.put(f"/api/v2/workbooks/{wid}/views/{vid}", json={"name": "x"}).status_code == 404
        assert tc.delete(f"/api/v2/workbooks/{wid}/views/{vid}").status_code == 404
    finally:
        app.dependency_overrides[current_workspace] = lambda: _ctx(WS1)
        app.dependency_overrides[require_editor] = lambda: _ctx(WS1)

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


def test_v2_row_identity_is_not_fabricated_as_lead_id(client):
    tc, Session, _ = client
    wid = _mk_workbook(Session, [
        {"id": "company", "name": "Company", "type": "lead_field", "lead_field": "company"},
    ])
    rid = _mk_row(Session, wid, {"company": "Acme"}, lead_id=None)

    response = tc.get(f"/api/workbooks/{wid}")
    assert response.status_code == 200, response.text
    row = response.json()["rows"][0]
    assert row["row_id"] == rid
    assert row["lead_id"] is None


def test_editing_unlinked_v2_row_updates_snapshot(client):
    tc, Session, _ = client
    wid = _mk_workbook(Session, [
        {"id": "company", "name": "Company", "type": "lead_field", "lead_field": "company"},
    ])
    rid = _mk_row(Session, wid, {"company": "Before"}, lead_id=None)

    response = tc.patch(
        f"/api/workbooks/{wid}/rows/{rid}", json={"company": "After"}
    )
    assert response.status_code == 200, response.text
    with Session() as db:
        row = db.get(WorkbookRow, rid)
        assert row.data["company"] == "After"


def test_full_worker_runnable_types_include_output():
    from apps.api.services.workbook.enrichment import ENRICHMENT_COL_TYPES

    assert "output" in ENRICHMENT_COL_TYPES


# ── Single-cell force re-run ─────────────────────────────────────────────

def test_cell_run_force_bypasses_output_run_once(client, monkeypatch):
    """Output columns are run-once; force=true (and only force) re-pushes."""
    import apps.api.services.workbook.enrichment as enr

    tc, Session, _ = client
    wid = _mk_workbook(Session, [
        {"id": "push", "name": "Push", "type": "output",
         "destination": "webhook", "destination_config": {"url": "https://x.example/hook"}},
    ])
    rid = _mk_row(Session, wid, {"company": "Acme"}, lead_id=101,
                  enrichments={"push": {"value": "sent", "status": "complete", "provider": "webhook"}})

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


@pytest.mark.parametrize("own_status,expected_calls", [(None, 0), ("complete", 0), ("error", 1)])
def test_output_run_once_uses_exact_row_receipt(client, monkeypatch, own_status, expected_calls):
    from apps.api.services.workbook import enrichment as engine
    tc, Session, _ = client
    wid = _mk_workbook(Session, [{"id": "push", "type": "output", "destination": "webhook"}])
    cells = {} if own_status is None else {"push": {"status": own_status, "value": "own receipt", "provider": "webhook"}}
    rid = _mk_row(Session, wid, {"company": "Selected"}, lead_id=101, enrichments=cells)
    _mk_row(Session, wid, {"company": "Other"}, lead_id=101,
            enrichments={"push": {"status": "complete", "value": "other receipt"}})
    with Session() as db:
        db.add(WorkbookEnrichment(workbook_id=wid, workspace_id=WS1, lead_id=101,
                                  column_id="push", status="complete", value="ambiguous legacy receipt"))
        db.commit()
    calls = []
    async def output(**kwargs):
        calls.append(kwargs)
        return {"value": "new receipt", "error": None}
    monkeypatch.setattr(engine, "execute_output_column", output)
    response = tc.post(f"/api/workbooks/{wid}/rows/{rid}/cells/push/run", json={"force": False})
    assert response.status_code == 200, response.text
    assert len(calls) == expected_calls
    if own_status is None:
        assert response.json()["error"] == "output_identity_requires_review"
    elif own_status == "complete":
        assert response.json()["value"] == "own receipt"
        assert response.json()["skipped"] is True


@pytest.mark.parametrize("failure", [
    {"success": False, "value": "POST 500", "error": "HTTP 500"},
    {"success": False, "value": "diagnostic", "error": None},
    {"value": "diagnostic", "error": "Destination rejected request"},
    {"success": True, "value": "contradictory", "error": "Destination rejected request"},
    {"success": "false", "value": "malformed", "error": None},
])
def test_failed_output_diagnostic_is_not_a_completed_receipt(client, monkeypatch, failure):
    from apps.api.services.workbook import enrichment as engine
    tc, Session, _ = client
    wid = _mk_workbook(Session, [{"id": "push", "type": "output", "destination": "webhook"}])
    rid = _mk_row(Session, wid, {"company": "Acme"})
    async def output(**kwargs):
        return failure
    monkeypatch.setattr(engine, "execute_output_column", output)
    response = tc.post(f"/api/workbooks/{wid}/rows/{rid}/cells/push/run", json={})
    assert response.status_code == 200, response.text
    assert response.json()["status"] == "error"
    assert response.json()["value"] is None
    with Session() as db:
        receipt = db.query(WorkbookEnrichment).one()
        assert receipt.status == "error" and receipt.value is None
        cell = db.get(WorkbookRow, rid).enrichments["push"]
        assert cell["status"] == "error" and cell["value"] is None
        assert cell["error"] == (failure["error"] or "output_failed")


@pytest.mark.parametrize("lost_ack", [False, True])
def test_queued_output_cell_replay_uses_journal(client, monkeypatch, lost_ack):
    import asyncio
    from datetime import datetime, timezone
    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.workbook import enrichment as engine
    from apps.api.services.workbook.batch_attempts import batch_lease_scope
    _, Session, _ = client
    Session.configure(autoflush=False)
    monkeypatch.setattr(engine, "SessionLocal", Session)
    column = {"id": "push", "type": "output", "destination": "webhook", "run_once": False}
    wid = _mk_workbook(Session, [column])
    rid = _mk_row(Session, wid, {"company": "Fixture"})
    locked = datetime.now(timezone.utc)
    with Session() as db:
        job = Job(type="run_workbook", workspace_id=WS1, status="processing", worker_id="owner",
                  locked_at=locked, payload={"workbook_id": wid})
        db.add(job)
        db.commit()
        jid = job.id
    calls = []
    async def output(**kwargs):
        calls.append(kwargs)
        if lost_ack:
            raise TimeoutError("Uncertain delivery")
        return {"success": True, "value": "delivery-1"}
    monkeypatch.setattr(engine, "execute_output_column", output)
    payload = {"workbook_id": wid, "workspace_id": WS1, "__queue_lease": {
        "worker_id": "owner", "locked_at": locked.isoformat()}}
    def run():
        with workspace_scope(WS1), batch_lease_scope(jid, payload), Session() as db:
            return asyncio.run(engine.enrich_cell(db, wid, rid, "push", column,
                {"id": rid, "__row_id": rid, "__lead_id": None, "company": "Fixture"},
                [column], force=True))
    if lost_ack:
        with pytest.raises(TimeoutError):
            run()
    else:
        assert run()["success"] is True
    replay = run()
    assert replay["success"] is (not lost_ack)
    assert replay["error"] == ("output_delivery_requires_review" if lost_ack else None)
    assert len(calls) == 1
    with Session() as db:
        cell = db.get(WorkbookRow, rid).enrichments["push"]
        assert cell["status"] == ("error" if lost_ack else "complete")
        assert cell["value"] == (None if lost_ack else "delivery-1")


@pytest.mark.parametrize("value,status,allowed", [(None, "error", False), ("stale", "error", False), (0, "complete", True), (False, "complete", True)])
def test_output_requires_available_computed_dependency(client, monkeypatch, value, status, allowed):
    from apps.api.services.workbook import enrichment as engine
    tc, Session, _ = client
    columns = [{"id": "score", "name": "Score", "type": "formula"},
               {"id": "push", "type": "output", "destination": "webhook", "destination_config": {"body": "{score}"}}]
    wid = _mk_workbook(Session, columns)
    rid = _mk_row(Session, wid, {"score": "old copied result"}, enrichments={"score": {"status": status, "value": value}})
    calls = []
    async def output(**kwargs):
        calls.append(kwargs)
        return {"success": True, "value": "receipt"}
    monkeypatch.setattr(engine, "execute_output_column", output)
    response = tc.post(f"/api/workbooks/{wid}/rows/{rid}/cells/push/run", json={})
    assert response.status_code == 200
    assert len(calls) == int(allowed)
    with Session() as db:
        cell = db.get(WorkbookRow, rid).enrichments["push"]
        assert cell["status"] == ("complete" if allowed else "error")
        if not allowed:
            assert cell["error"] == "upstream_dependency_unavailable"


@pytest.mark.parametrize("kind,field", [("research", "prompt"), ("http", "http_url"), ("formula", "formula")])
def test_cell_rejects_ambiguous_display_reference_before_execution(client, monkeypatch, kind, field):
    from apps.api.services.workbook import research_column, http_column, formula_column
    tc, Session, _ = client
    async def forbidden(*args, **kwargs):
        pytest.fail("Ambiguous reference must not reach execution")
    monkeypatch.setattr(research_column, "execute_research_column", forbidden)
    monkeypatch.setattr(http_column, "execute_http_column", forbidden)
    monkeypatch.setattr(formula_column, "execute_formula_column", forbidden)
    columns = [{"id": "first", "name": "Shared", "type": "input"},
               {"id": "second", "name": "Shared", "type": "input"},
               {"id": "result", "name": "Result", "type": kind, field: "{Shared}"}]
    wid = _mk_workbook(Session, columns)
    rid = _mk_row(Session, wid, {"first": "one", "second": "two"})
    response = tc.post(f"/api/workbooks/{wid}/rows/{rid}/cells/result/run", json={})
    assert response.status_code == 200, response.text
    with Session() as db:
        assert db.get(WorkbookRow, rid).enrichments["result"]["error"] == "ambiguous_column_reference"


def test_output_rejects_dependency_cycle_even_with_saved_values(client, monkeypatch):
    from apps.api.services.workbook import enrichment as engine
    tc, Session, _ = client
    columns = [{"id": "a", "type": "formula", "formula": "{b}"},
               {"id": "b", "type": "formula", "formula": "{a}"},
               {"id": "push", "type": "output", "destination_config": {"body": "{a}"}}]
    wid = _mk_workbook(Session, columns)
    rid = _mk_row(Session, wid, {}, enrichments={key: {"status": "complete", "value": "stale"} for key in ("a", "b")})
    async def forbidden(**kwargs):
        pytest.fail("Cycle-dependent output must not dispatch")
    monkeypatch.setattr(engine, "execute_output_column", forbidden)
    response = tc.post(f"/api/workbooks/{wid}/rows/{rid}/cells/push/run", json={})
    assert response.status_code == 200
    with Session() as db:
        assert db.get(WorkbookRow, rid).enrichments["push"]["error"] == "dependency_cycle"


@pytest.mark.parametrize("answer", ["Partnership team confirmed", ""])
def test_research_evidence_survives_row_reload(client, answer):
    from apps.api.services.workbook.enrichment import _set_enrichment
    from apps.api.core.tenancy import workspace_scope

    tc, Session, _ = client
    wid = _mk_workbook(Session, [{"id": "research", "name": "Research", "type": "research"}])
    rid = _mk_row(Session, wid, {"company": "Acme"}, lead_id=202)
    evidence = {
        "answer": answer,
        "citations": [{"url": "https://example.com/team", "title": "Team", "quoted_text": "Partnerships", "fetched_at": "2026-09-22T00:00:00+00:00"}] if answer else [],
        "cost_usd": 0.012,
        "stopped_reason": "answered" if answer else "no_answer",
        "synthesis_fallback": False,
    }
    with workspace_scope(WS1), Session() as db:
        _set_enrichment(db, wid, 202, "research", answer or None,
                        "complete" if answer else "error",
                        metadata={"research": evidence})
        db.commit()
        assert db.query(WorkbookEnrichment).filter_by(workbook_id=wid).one().cell_metadata["research"] == evidence
        assert db.query(WorkbookRow).filter_by(id=rid).one().enrichments["research"]["research"] == evidence
    response = tc.get(f"/api/workbooks/{wid}")
    assert response.status_code == 200, response.text
    overlay = response.json()["rows"][0]["enrichments"]["research"]
    assert overlay["research"] == evidence
    assert overlay["status"] == ("complete" if answer else "error")


@pytest.mark.parametrize("receipt_value,receipt_workspace,expected", [
    ("Answer", WS1, True), ("Stale answer", WS1, False), ("Answer", WS2, False),
])
def test_historical_research_readback_is_scoped_and_matches_current_cell(client, receipt_value, receipt_workspace, expected):
    tc, Session, _ = client
    wid = _mk_workbook(Session, [{"id": "research", "type": "research", "name": "Research"}])
    original = {"research": {"value": "Answer", "status": "complete"}}
    rid = _mk_row(Session, wid, {"company": "Acme"}, enrichments=original, lead_id=202)
    evidence = {"answer": receipt_value, "citations": [], "cost_usd": 0.01,
                "stopped_reason": "answered", "synthesis_fallback": False}
    with Session() as db:
        db.add(WorkbookEnrichment(workbook_id=wid, workspace_id=receipt_workspace,
                                 lead_id=202, column_id="research", value=receipt_value,
                                 status="complete", cell_metadata={"research": evidence}))
        db.commit()
    response = tc.get(f"/api/workbooks/{wid}")
    assert response.status_code == 200, response.text
    research = response.json()["rows"][0]["enrichments"]["research"]["research"]
    assert research == (evidence if expected else None)
    with Session() as db:
        assert db.get(WorkbookRow, rid).enrichments == original, "GET must not backfill or mutate rows"


@pytest.mark.parametrize("status,value", [("running", None), ("error", None), ("complete", "Same answer")])
def test_new_attempt_without_metadata_clears_previous_evidence(client, status, value):
    from apps.api.services.workbook.enrichment import _set_enrichment
    from apps.api.core.tenancy import workspace_scope

    tc, Session, _ = client
    wid = _mk_workbook(Session, [{"id": "research", "type": "research", "name": "Research"}])
    rid = _mk_row(Session, wid, {"company": "Acme"}, lead_id=202)
    metadata = {"research": {"answer": "Same answer", "citations": [], "stopped_reason": "answered"},
                "verify": {"status": "valid"}}
    with workspace_scope(WS1), Session() as db:
        _set_enrichment(db, wid, 202, "research", "Same answer", "complete", metadata=metadata)
        db.commit()
        _set_enrichment(db, wid, 202, "research", value, status)
        db.commit()
        assert db.query(WorkbookEnrichment).filter_by(workbook_id=wid).one().cell_metadata is None
        assert "research" not in db.get(WorkbookRow, rid).enrichments["research"]
    response = tc.get(f"/api/workbooks/{wid}")
    assert response.status_code == 200, response.text
    overlay = response.json()["rows"][0]["enrichments"]["research"]
    assert overlay["research"] is None
    assert overlay["verify_status"] is None


@pytest.mark.parametrize("answer", ["Confirmed team", ""])
def test_research_execution_exposes_evidence_in_cell_response(client, monkeypatch, answer):
    import asyncio
    import apps.api.services.workbook.enrichment as enrichment
    import apps.api.services.workbook.research_column as research
    from apps.api.core.tenancy import workspace_scope

    tc, Session, _ = client
    column = {"id": "research", "name": "Research", "type": "research", "prompt": "Find team"}
    wid = _mk_workbook(Session, [column])
    rid = _mk_row(Session, wid, {"company": "Acme"}, lead_id=202)
    evidence = {"answer": answer, "citations": [], "cost_usd": 0.01,
                "stopped_reason": "answered" if answer else "no_answer", "synthesis_fallback": False}

    async def execute(**kwargs):
        assert kwargs["workspace_id"] == WS1
        return {"value": answer, "error": None if answer else "no_answer", "metadata": {"research": evidence}}

    monkeypatch.setattr(research, "execute_research_column", execute)
    response = tc.post(f"/api/workbooks/{wid}/rows/{rid}/cells/research/run", json={"force": True})
    assert response.status_code == 200, response.text
    assert response.json()["research"] == evidence
    assert response.json()["status"] == ("complete" if answer else "error")
    reloaded = tc.get(f"/api/workbooks/{wid}").json()
    assert reloaded["rows"][0]["enrichments"]["research"]["research"] == evidence
    messages = []

    async def broadcast(redis, workbook_id, message):
        assert workbook_id == wid
        messages.append(message)

    monkeypatch.setattr(enrichment, "_broadcast", broadcast)
    with workspace_scope(WS1), Session() as db:
        asyncio.run(enrichment.enrich_cell(
            db, wid, 202, "research", column,
            {"company": "Acme", "__row_id": rid, "__lead_id": 202},
            [column], redis_client=object(), force=True,
        ))
    final = [message for message in messages if message.get("research") is not None][-1]
    assert final["research"] == evidence
    assert final["rowId"] == rid
    assert final["status"] == ("complete" if answer else "error")


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
    assert provider_calls == ["mock_provider"], "only the configured provider may execute"

    # Persisted: overlay row AND the inline WorkbookRow.enrichments mirror.
    s = Session()
    cell = s.query(WorkbookEnrichment).filter_by(workbook_id=wid, column_id="find_email").one()
    assert cell.value == "new@acme.com"
    row = s.query(WorkbookRow).filter_by(id=rid).one()
    assert row.enrichments["find_email"]["value"] == "new@acme.com"
    s.close()


@pytest.mark.parametrize("selection", [
    {"waterfall": []},
    {"waterfall": [], "provider": "hunter_io"},
    {"waterfall": ["hunter_io", "prospeo"]},
    {"provider": "hunter_io"},
])
def test_explicit_provider_selection_is_exact_and_ordered(client, monkeypatch, selection):
    """Configured paid providers cannot be expanded or reordered by yield/cost."""
    import apps.api.services.workbook.enrichment as enr

    tc, Session, _ = client
    column = {"id": "find_email", "name": "Email", "type": "waterfall",
              "target_field": "email", "verify": False, **selection}
    wid = _mk_workbook(Session, [column])
    rid = _mk_row(Session, wid, {"company": "Acme", "website": "acme.com"})
    calls = []

    class Provider:
        default_confidence = 0.9

    async def run_provider(name, lead, timeout=10.0):
        calls.append(name)
        return {"provider": name, "success": False, "fields": {}}

    monkeypatch.setattr(enr, "get_provider", lambda name: Provider())
    monkeypatch.setattr(enr, "run_provider", run_provider)
    response = tc.post(f"/api/workbooks/{wid}/rows/{rid}/cells/find_email/run", json={})
    assert response.status_code == 200, response.text
    assert calls == selection.get("waterfall", [selection.get("provider")])


def test_explicit_provider_order_still_enforces_budget_and_cooldown(client, monkeypatch):
    import apps.api.services.workbook.enrichment as enr
    from datetime import datetime, timedelta, timezone

    tc, Session, _ = client
    column = {"id": "find_email", "name": "Email", "type": "waterfall",
              "target_field": "email", "verify": False,
              "waterfall": ["hunter_io", "prospeo", "website_scraper"]}
    wid = _mk_workbook(Session, [column])
    rid = _mk_row(Session, wid, {"company": "Acme", "website": "acme.com"})
    with Session() as db:
        workbook = db.get(Workbook, wid)
        workbook.budget_max_usd = 0.025  # Hunter is unaffordable; Prospeo is in cooldown.
        db.add(ProviderStat(provider="prospeo", field="email",
                            cooldown_until=datetime.now(timezone.utc) + timedelta(minutes=5)))
        db.commit()
    calls = []

    class Provider:
        default_confidence = 0.9

    async def run_provider(name, lead, timeout=10.0):
        calls.append(name)
        return {"provider": name, "success": False, "fields": {}}

    monkeypatch.setattr(enr, "get_provider", lambda name: Provider())
    monkeypatch.setattr(enr, "run_provider", run_provider)
    response = tc.post(f"/api/workbooks/{wid}/rows/{rid}/cells/find_email/run", json={})
    assert response.status_code == 200, response.text
    assert calls == ["website_scraper"]


@pytest.mark.parametrize("selection, known, budget, expected", [
    ({"waterfall": []}, set(), 0.0, "no_providers_selected"),
    ({"waterfall": ["ghost_provider"]}, set(), 0.0,
     "providers_unavailable: ghost_provider (unknown_provider)"),
    ({"waterfall": ["hunter_io", "ghost_provider"]}, {"hunter_io"}, 0.001,
     "providers_unavailable: hunter_io (over_budget), ghost_provider (unknown_provider)"),
])
def test_explicit_selection_that_calls_no_provider_reports_why(
        client, monkeypatch, selection, known, budget, expected):
    """Empty/unknown/unaffordable selections are explicit, never generic no_data."""
    import apps.api.services.workbook.enrichment as enr

    tc, Session, _ = client
    column = {"id": "find_email", "name": "Email", "type": "waterfall",
              "target_field": "email", "verify": False, **selection}
    wid = _mk_workbook(Session, [column])
    rid = _mk_row(Session, wid, {"company": "Acme", "website": "acme.com"})
    if budget:
        with Session() as db:
            db.get(Workbook, wid).budget_max_usd = budget
            db.commit()

    class Provider:
        default_confidence = 0.9

    async def run_provider(name, lead, timeout=10.0):
        pytest.fail(f"{name} must not be called")

    monkeypatch.setattr(enr, "get_provider", lambda name: Provider() if name in known else None)
    monkeypatch.setattr(enr, "run_provider", run_provider)
    response = tc.post(f"/api/workbooks/{wid}/rows/{rid}/cells/find_email/run", json={})
    assert response.status_code == 200, response.text
    body = response.json()
    assert body["status"] == "error" and body["error"] == expected


def test_skipped_providers_are_recorded_when_a_later_provider_succeeds(client, monkeypatch):
    import apps.api.services.workbook.enrichment as enr

    tc, Session, _ = client
    column = {"id": "find_email", "name": "Email", "type": "waterfall",
              "target_field": "email", "verify": False,
              "waterfall": ["ghost_provider", "fixture"]}
    wid = _mk_workbook(Session, [column])
    rid = _mk_row(Session, wid, {"company": "Acme", "website": "acme.com"})

    class Provider:
        default_confidence = 0.9

    async def run_provider(name, lead, timeout=10.0):
        return {"provider": name, "success": True, "fields": {"email": "ann@acme.com"}}

    monkeypatch.setattr(enr, "get_provider", lambda name: Provider() if name == "fixture" else None)
    monkeypatch.setattr(enr, "run_provider", run_provider)
    response = tc.post(f"/api/workbooks/{wid}/rows/{rid}/cells/find_email/run", json={})
    assert response.status_code == 200, response.text
    assert response.json()["value"] == "ann@acme.com"
    with Session() as s:
        cell = s.query(WorkbookEnrichment).filter_by(workbook_id=wid, column_id="find_email").one()
        assert cell.cell_metadata["skipped_providers"] == [
            {"provider": "ghost_provider", "reason": "unknown_provider"}]
        row = s.query(WorkbookRow).filter_by(id=rid).one()
        assert row.enrichments["find_email"]["skipped_providers"][0]["provider"] == "ghost_provider"


@pytest.mark.parametrize("success", [True, False])
def test_queued_waterfall_reserves_and_replays_paid_attempt(client, monkeypatch, success):
    import asyncio
    from types import SimpleNamespace
    from apps.api.services.workbook import enrichment as enr
    from apps.api.services.workbook.execution_identity import execution_scope
    from apps.api.services.workbook.spend_models import WorkbookSpendAttempt
    from apps.api.core.tenancy import workspace_scope
    _, Session, _ = client
    with Session() as db:
        WorkbookSpendAttempt.__table__.create(db.get_bind())
    column = {"id": "email", "name": "Email", "type": "waterfall", "target_field": "email", "waterfall": ["fixture"], "verify": False}
    wid = _mk_workbook(Session, [column])
    rid = _mk_row(Session, wid, {"company": "Acme"})
    with Session() as db:
        db.get(Workbook, wid).budget_max_usd = 0.02
        db.commit()
    calls = []
    async def provider(name, lead, timeout):
        calls.append(name)
        return {"provider": name, "success": success, "fields": {"email": "fixture@example.com"} if success else {}, "confidence": 0.9}
    from apps.api.services.workbook import planner
    monkeypatch.setattr(enr, "SessionLocal", Session)
    monkeypatch.setattr(enr, "get_provider", lambda name: SimpleNamespace(default_confidence=0.9))
    monkeypatch.setattr(enr, "run_provider", provider)
    monkeypatch.setattr(planner, "provider_cost", lambda name: 0.02)
    monkeypatch.setattr(planner, "is_paid", lambda name: True)
    def run():
        with workspace_scope(WS1), execution_scope(WS1, wid, 123), Session() as db:
            return asyncio.run(enr.enrich_cell(db, wid, rid, "email", column,
                {"id": rid, "company": "Acme", "__row_id": rid, "__lead_id": None}, [column], force=True))
    first, retry = run(), run()
    assert calls == ["fixture"]
    if success:
        assert first["value"] == retry["value"] == "fixture@example.com"
    else:
        assert not first["success"] and not retry["success"]
    with Session() as db:
        assert db.get(Workbook, wid).budget_spent_usd == (0.02 if success else 0)
        receipt = db.query(WorkbookSpendAttempt).filter_by(workbook_id=wid).one()
        assert receipt.status == ("settled" if success else "uncertain")


def test_queued_waterfall_blocks_unknown_provider_price(client, monkeypatch):
    import asyncio
    from types import SimpleNamespace
    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.workbook import enrichment as enr, providers
    from apps.api.services.workbook.execution_identity import execution_scope
    from apps.api.services.workbook.spend_models import WorkbookSpendAttempt
    _, Session, _ = client
    with Session() as db:
        WorkbookSpendAttempt.__table__.create(db.get_bind())
    column = {"id": "email", "name": "Email", "type": "waterfall", "target_field": "email",
              "waterfall": ["unknown_fixture"], "verify": False}
    wid = _mk_workbook(Session, [column])
    rid = _mk_row(Session, wid, {"company": "Example"})
    provider = SimpleNamespace(default_confidence=0.9, cost_per_lookup=None)
    monkeypatch.setattr(providers, "get_provider", lambda name: provider)
    monkeypatch.setattr(enr, "get_provider", lambda name: provider)
    async def forbidden(*args, **kwargs):
        pytest.fail("Unknown-price provider must not execute")
    monkeypatch.setattr(enr, "run_provider", forbidden)
    with workspace_scope(WS1), execution_scope(WS1, wid, 123), Session() as db:
        result = asyncio.run(enr.enrich_cell(db, wid, rid, "email", column,
            {"id": rid, "company": "Example", "__row_id": rid, "__lead_id": None}, [column], force=True))
    assert result["error"] == "provider_price_unknown"
    with Session() as db:
        assert db.query(WorkbookSpendAttempt).filter_by(workbook_id=wid).count() == 0


@pytest.mark.parametrize("phase", ["rows", "batch"])
def test_cancelled_workbook_task_is_not_marked_complete(client, monkeypatch, phase):
    import asyncio
    from apps.api.services.workbook import enrichment as enr
    _, Session, _ = client
    wid = _mk_workbook(Session, [{"id": "email", "name": "Email", "type": "waterfall", "waterfall": []}])
    _mk_row(Session, wid, {"company": "Example"})
    monkeypatch.setattr(enr, "SessionLocal", Session)
    broadcasts = []
    class RedisFixture:
        closed = False
        async def aclose(self):
            self.closed = True
    redis = RedisFixture()
    async def broadcast(client, workbook_id, message):
        broadcasts.append(message)
    monkeypatch.setattr(enr, "_make_redis", lambda: redis)
    monkeypatch.setattr(enr, "_broadcast", broadcast)
    async def prepass(*args, **kwargs):
        return set(), 0
    monkeypatch.setattr(enr, "_run_ai_batch_prepass", prepass)
    async def scenario():
        entered = asyncio.Event()
        async def row(*args, **kwargs):
            entered.set()
            await asyncio.Future()
        monkeypatch.setattr(enr, "_run_one_row", row)
        if phase == "batch":
            monkeypatch.setattr(enr, "_run_ai_batch_prepass", row)
        task = asyncio.create_task(enr._run_workbook_enrichment_impl(wid))
        await asyncio.wait_for(entered.wait(), timeout=2)
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task
    asyncio.run(scenario())
    assert redis.closed
    assert broadcasts[-1]["status"] == "paused"
    assert broadcasts[-1]["completed"] == 0
    with Session() as db:
        workbook = db.get(Workbook, wid)
        assert workbook.status == "paused"
        assert workbook.completed_rows == 0


@pytest.mark.parametrize("kind", ["ai_formula", "waterfall", "agent"])
def test_stale_nonbatch_cell_cannot_overwrite_replacement_result(client, monkeypatch, kind):
    import asyncio
    from datetime import datetime, timezone
    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.workbook import enrichment as enr
    from apps.api.services.workbook.batch_attempts import batch_lease_scope
    _, Session, _ = client
    Session.configure(autoflush=False)  # Match production; no writer lock during AI.
    monkeypatch.setattr(enr, "SessionLocal", Session)
    column = {"id": "ai", "type": kind, "prompt": "Describe {company}",
              "waterfall": ["first_fixture", "second_fixture"], "tools": ["first_fixture", "second_fixture"],
              "target_field": "email", "verify": False}
    wid = _mk_workbook(Session, [column])
    rid = _mk_row(Session, wid, {"company": "Example"})
    locked = datetime.now(timezone.utc)
    with Session() as db:
        job = Job(type="run_workbook", workspace_id=WS1, status="processing", worker_id="owner",
                  locked_at=locked, payload={"workbook_id": wid})
        db.add(job)
        db.commit()
        jid = job.id
    replacement = {"ai": {"value": "New owner result", "status": "complete"}}
    async def ai(**kwargs):
        with Session() as db:
            db.get(Job, jid).worker_id = "replacement"
            db.get(WorkbookRow, rid).enrichments = replacement
            db.commit()
        return {"value": "Stale result"}
    monkeypatch.setattr(enr, "execute_ai_column", ai)
    calls = []
    if kind in ("waterfall", "agent"):
        from types import SimpleNamespace
        from apps.api.services.workbook import providers
        provider = SimpleNamespace(default_confidence=0.9, cost_per_lookup=0)
        monkeypatch.setattr(providers, "get_provider", lambda name: provider)
        monkeypatch.setattr(enr, "get_provider", lambda name: provider)
        async def lookup(name, lead, timeout):
            calls.append(name)
            await ai()
            return {"provider": name, "success": False, "fields": {}}
        monkeypatch.setattr(enr, "run_provider", lookup)
        if kind == "agent":
            from apps.api.services.workbook import agent_column
            monkeypatch.setattr(agent_column, "SessionLocal", Session)
            monkeypatch.setattr(agent_column, "get_provider", lambda name: provider)
            monkeypatch.setattr(agent_column, "run_provider", lookup)
            monkeypatch.setattr(agent_column, "_save_trace", lambda *args: None)
    payload = {"workbook_id": wid, "workspace_id": WS1, "__queue_lease": {
        "worker_id": "owner", "locked_at": locked.isoformat()}}
    with workspace_scope(WS1), batch_lease_scope(jid, payload), Session() as db:
        with pytest.raises(ValueError, match="lease"):
            asyncio.run(enr.enrich_cell(db, wid, rid, "ai", column,
                {"id": rid, "__row_id": rid, "__lead_id": None, "company": "Example"}, [column], force=True))
    with Session() as db:
        assert db.get(WorkbookRow, rid).enrichments == replacement
        assert db.query(WorkbookEnrichment).filter_by(workbook_id=wid).count() == 0
    if kind in ("waterfall", "agent"):
        assert calls == ["first_fixture"]


def test_cost_headroom_includes_reserved_and_uncertain_exposure(client):
    from apps.api.services.workbook.spend_models import WorkbookSpendAttempt
    tc, Session, _ = client
    wid = _mk_workbook(Session, [])
    with Session() as db:
        WorkbookSpendAttempt.__table__.create(db.get_bind())
        wb = db.get(Workbook, wid)
        wb.budget_max_usd, wb.budget_spent_usd = 0.1, 0.01
        for i, status in enumerate(["reserved", "dispatched", "uncertain", "settled", "released"]):
            db.add(WorkbookSpendAttempt(id=str(i), workspace_id=WS1, workbook_id=wid,
                run_id="run", row_identity="row:1", column_id="email", provider="fixture", attempt_key=str(i),
                contract_hash="a" * 64, reserved_microusd=20000, cost_basis={}, status=status, created_at=1, updated_at=1))
        db.commit()
    response = tc.get(f"/api/workbooks/{wid}/cost")
    assert response.status_code == 200
    result = response.json()
    assert result["reserved_usd"] == 0.04
    assert result["uncertain_usd"] == 0.02
    assert result["remaining_usd"] == 0.03
    assert result["accounting_basis"] == "catalog_estimates_and_recorded_charges"
    for invalid in [-1, "NaN", "Infinity"]:
        assert tc.put(f"/api/workbooks/{wid}/budget", json={"max_usd": invalid}).status_code == 422
    other = _mk_workbook(Session, [], ws=WS2)
    assert tc.get(f"/api/workbooks/{other}/cost").status_code == 404


def test_default_waterfall_retains_cost_planning(client, monkeypatch):
    import apps.api.services.workbook.enrichment as enr

    tc, Session, _ = client
    column = {"id": "find_email", "name": "Email", "type": "waterfall",
              "target_field": "email", "verify": False}
    wid = _mk_workbook(Session, [column])
    rid = _mk_row(Session, wid, {"company": "Acme", "website": "acme.com"})
    calls = []

    class Provider:
        default_confidence = 0.9

    async def run_provider(name, lead, timeout=10.0):
        calls.append(name)
        return {"provider": name, "success": False, "fields": {}}

    monkeypatch.setitem(enr.DEFAULT_WATERFALLS, "email", ["hunter_io", "prospeo"])
    monkeypatch.setattr(enr, "get_provider", lambda name: Provider())
    monkeypatch.setattr(enr, "run_provider", run_provider)
    response = tc.post(f"/api/workbooks/{wid}/rows/{rid}/cells/find_email/run", json={})
    assert response.status_code == 200, response.text
    assert calls == ["prospeo", "hunter_io"]


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


@pytest.mark.parametrize("has_rows", [False, True])
def test_missing_v2_cell_row_never_falls_back_to_lead_identity(client, monkeypatch, has_rows):
    from apps.api.services.workbook import enrichment as engine
    tc, Session, _ = client
    wid = _mk_workbook(Session, [{"id": "formula", "name": "Formula", "type": "formula", "formula": "1"}])
    if has_rows:
        _mk_row(Session, wid, {"company": "Wrong target"}, lead_id=900)
    def forbidden_store(*args, **kwargs):
        raise AssertionError("A v2 row request must not consult the legacy lead store")
    async def forbidden_execution(**kwargs):
        raise AssertionError("A missing row must not execute a cell")
    monkeypatch.setattr(WorkspaceCtx, "lead_db", forbidden_store)
    monkeypatch.setattr(engine, "enrich_cell", forbidden_execution)
    response = tc.post(f"/api/workbooks/{wid}/rows/900/cells/formula/run", json={"force": True})
    assert response.status_code == 404, response.text
    with Session() as db:
        assert db.query(WorkbookEnrichment).count() == 0
        assert db.query(Job).count() == 0


@pytest.mark.parametrize("matches", [False, True])
def test_legacy_cell_run_intersects_workbook_filter(client, monkeypatch, matches):
    from types import SimpleNamespace
    from apps.api.routers import workbooks as router
    from apps.api.services.workbook import enrichment as engine
    tc, Session, _ = client
    wid = _mk_workbook(Session, [{"id": "formula", "type": "formula", "formula": "1"}])
    with Session() as db:
        wb = db.get(Workbook, wid)
        wb.source_type = "leads_filter"
        wb.filter_criteria = {"city": "Pune"}
        db.commit()
    monkeypatch.setattr(WorkspaceCtx, "lead_db", lambda self: SimpleNamespace(close=lambda: None))
    def query(store, filters, **kwargs):
        assert filters == {"city": "Pune", "lead_ids": [71]}
        return ([{"id": 71, "company": "Matched"}], 1) if matches else ([], 0)
    monkeypatch.setattr(router, "_query_leads", query)
    calls = []
    async def execute(**kwargs):
        calls.append(kwargs["lead_id"])
        return {"success": True, "value": "1"}
    monkeypatch.setattr(engine, "enrich_cell", execute)
    response = tc.post(f"/api/workbooks/{wid}/rows/71/cells/formula/run", json={})
    assert response.status_code == (200 if matches else 404), response.text
    assert calls == ([71] if matches else [])


def test_cell_run_other_workspace_workbook_404(client):
    """Cell run on a workbook owned by another tenant → 404 (no leak)."""
    tc, Session, app = client
    wid = _mk_workbook(Session, [
        {"id": "col_a", "name": "A", "type": "waterfall", "target_field": "email"},
    ], ws=WS1)
    rid = _mk_row(Session, wid, {"company": "Acme"})

    app.dependency_overrides[current_workspace] = lambda: _ctx(WS2)
    app.dependency_overrides[require_editor] = lambda: _ctx(WS2)
    try:
        r = tc.post(f"/api/workbooks/{wid}/rows/{rid}/cells/col_a/run", json={"force": True})
        assert r.status_code == 404
    finally:
        app.dependency_overrides[current_workspace] = lambda: _ctx(WS1)
        app.dependency_overrides[require_editor] = lambda: _ctx(WS1)


def test_viewer_cannot_mutate_workbook(client, monkeypatch):
    from apps.api.services.workspace import manager as ws_manager

    tc, Session, app = client
    wid = _mk_workbook(Session, [])
    app.dependency_overrides.pop(require_editor)
    monkeypatch.setattr(ws_manager, "member_role", lambda workspace_id, user_id: "viewer")

    response = tc.post(
        f"/api/workbooks/{wid}/rows",
        json={"rows": [{"company": "Must Not Write"}]},
    )
    assert response.status_code == 403


def test_budget_update_rejects_foreign_workbook(client):
    tc, Session, app = client
    wid = _mk_workbook(Session, [], ws=WS1)
    app.dependency_overrides[current_workspace] = lambda: _ctx(WS2)
    app.dependency_overrides[require_editor] = lambda: _ctx(WS2)
    try:
        response = tc.put(
            f"/api/workbooks/{wid}/budget", json={"max_usd": 100}
        )
        assert response.status_code == 404
    finally:
        app.dependency_overrides[current_workspace] = lambda: _ctx(WS1)
        app.dependency_overrides[require_editor] = lambda: _ctx(WS1)
