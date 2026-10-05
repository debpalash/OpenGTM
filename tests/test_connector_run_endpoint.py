"""POST /api/workbooks/{id}/run chooses the job type from the executor route.

Offline (SQLite, auth overridden, pattern of test_workbook_views.py). With no
route the endpoint enqueues ``run_workbook`` exactly as before; with
``run_workbook_connector`` routed to Go it enqueues the new type for eligible
runs only; stop, supersede and run history treat both types as workbook runs.
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
from apps.api.models import Job, JobExecutorRoute
from apps.api.routers.workbooks import router as workbooks_router, require_editor, views_router
from apps.api.services.leadgen.enrichment.declarative.compiler import compile_manifest
from apps.api.services.leadgen.enrichment.declarative.manifest import ProviderManifest
from apps.api.services.workbook.models import Workbook, WorkbookEnrichment, WorkbookRow, WorkbookView
from apps.api.services.workbook.planner_models import ProviderStat
from apps.api.services.workbook.providers import _registry, register_provider

WS = "ws_connector_run"


class _User:
    id = "user-1"


@pytest.fixture()
def client():
    engine = create_engine("sqlite:///:memory:", connect_args={"check_same_thread": False}, poolclass=StaticPool)
    Base.metadata.create_all(engine, tables=[
        Workbook.__table__, WorkbookRow.__table__, WorkbookEnrichment.__table__, WorkbookView.__table__,
        ProviderStat.__table__, Job.__table__, JobExecutorRoute.__table__,
    ])
    Session = sessionmaker(bind=engine)
    app = FastAPI()
    app.state.limiter = limiter
    app.include_router(workbooks_router)
    app.include_router(views_router)

    def _db():
        s = Session()
        try:
            yield s
        finally:
            s.close()

    ctx = WorkspaceCtx(user=_User(), workspace_id=WS, slug=WS)
    app.dependency_overrides[get_db] = _db
    app.dependency_overrides[current_workspace] = lambda: ctx
    app.dependency_overrides[require_editor] = lambda: ctx
    before = dict(_registry)
    register_provider(compile_manifest(ProviderManifest(
        name="ep_free", capability="email", cost_per_lookup=0.0,
        request={"method": "GET", "url": "https://api.vendor.example/find"},
        response={"mappings": {"email": "$.email"}})))
    yield TestClient(app), Session
    _registry.clear()
    _registry.update(before)


COLUMNS = [
    {"id": "company", "name": "Company", "type": "lead_field"},
    {"id": "email", "name": "Email", "type": "enrichment", "provider": "ep_free", "target_field": "email", "verify": False},
]


def _workbook(Session, columns=COLUMNS, lead_id=None):
    with Session() as s:
        wb = Workbook(name="WB", workspace_id=WS, columns_config=columns)
        s.add(wb)
        s.commit()
        row = WorkbookRow(workbook_id=wb.id, workspace_id=WS, position=0, data={"company": "Acme", "website": "acme.example"},
                          enrichments={}, lead_id=lead_id)
        s.add(row)
        s.commit()
        return wb.id, row.id


def _route(Session, executor):
    with Session() as s:
        s.merge(JobExecutorRoute(job_type="run_workbook_connector", executor=executor))
        s.commit()


def _job_types(Session):
    with Session() as s:
        return [(j.type, j.status) for j in s.query(Job).order_by(Job.id)]


def test_without_a_route_the_endpoint_enqueues_run_workbook(client):
    tc, Session = client
    wid, _ = _workbook(Session)
    assert tc.post(f"/api/workbooks/{wid}/run", json={}).json()["status"] == "started"
    assert _job_types(Session) == [("run_workbook", "pending")]


def test_routed_to_go_an_eligible_run_enqueues_the_connector_type(client):
    tc, Session = client
    wid, rid = _workbook(Session)
    _route(Session, "go")
    body = tc.post(f"/api/workbooks/{wid}/run", json={}).json()
    assert body["status"] == "started"
    with Session() as s:
        job = s.query(Job).one()
        assert job.type == "run_workbook_connector"
        assert job.payload["workbook_id"] == wid and job.payload["workspace_id"] == WS
        assert job.payload["column_ids"] == ["email"] and job.payload["row_ids"] == [rid]
        assert job.workspace_id == WS
    history = tc.get(f"/api/workbooks/{wid}/runs").json()["runs"]
    assert [r["job_id"] for r in history] == [body["job_id"]]


@pytest.mark.parametrize("columns, lead_id", [
    ([*COLUMNS, {"id": "f", "name": "F", "type": "formula", "formula": "1 + 1"}], None),   # a non-connector column
    (COLUMNS, 41),                                                                          # a linked row
    ([COLUMNS[0], {**COLUMNS[1], "verify": True}], None),                                   # the verify cascade
    ([COLUMNS[0], {k: v for k, v in COLUMNS[1].items() if k != "provider"}], None),         # a default waterfall
])
def test_routed_to_go_an_ineligible_run_stays_on_run_workbook(client, columns, lead_id):
    tc, Session = client
    wid, _ = _workbook(Session, columns, lead_id)
    _route(Session, "go")
    assert tc.post(f"/api/workbooks/{wid}/run", json={}).json()["status"] == "started"
    assert [t for t, _ in _job_types(Session)] == ["run_workbook"]


def test_rolling_back_the_route_returns_new_runs_to_run_workbook(client):
    tc, Session = client
    wid, _ = _workbook(Session)
    _route(Session, "go")
    tc.post(f"/api/workbooks/{wid}/run", json={})
    _route(Session, "python")  # opengtm routes set run_workbook_connector python
    tc.post(f"/api/workbooks/{wid}/run", json={})
    types = _job_types(Session)
    # the first run was superseded by the second, whichever type it used
    assert types == [("run_workbook_connector", "cancelled"), ("run_workbook", "pending")]


def test_stop_cancels_a_connector_run_and_history_lists_both_types(client):
    tc, Session = client
    wid, _ = _workbook(Session)
    with Session() as s:
        s.add(Job(type="run_workbook", payload={"workbook_id": wid, "workspace_id": WS}, status="completed",
                  workspace_id=WS, priority=1))
        s.commit()
    _route(Session, "go")
    tc.post(f"/api/workbooks/{wid}/run", json={})
    assert tc.post(f"/api/workbooks/{wid}/stop").json() == {"status": "paused"}
    assert _job_types(Session) == [("run_workbook", "completed"), ("run_workbook_connector", "cancelled")]
    assert len(tc.get(f"/api/workbooks/{wid}/runs").json()["runs"]) == 2
