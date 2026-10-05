"""Which workbook runs use run_workbook_connector, and that Python still owns the rest.

Offline (SQLite): the gate in connector_run.py decides from the column configs
and hydrated rows alone, and the route table decides whether the new job type is
used at all. With no route a run is enqueued as run_workbook exactly as before.
"""
import pytest

from apps.api.database import SessionLocal
from apps.api.db_init import init_db
from apps.api.models import Job, JobExecutorRoute
from apps.api.services.leadgen.enrichment.declarative.compiler import compile_manifest
from apps.api.services.leadgen.enrichment.declarative.manifest import ProviderManifest
from apps.api.services.queue_service import JOB_TIMEOUTS
from apps.api.services.workbook import connector_run
from apps.api.services.workbook.providers import _registry, register_provider


def _connector(name, *, url="https://api.vendor.example/v1/find", cost=0.0, auth=None, extra_header=None):
    raw = {
        "name": name, "capability": "email", "cost_per_lookup": cost,
        "request": {"method": "GET", "url": url, "headers": extra_header or {}},
        "response": {"mappings": {"email": "$.email"}},
    }
    if auth:
        raw["auth"] = auth
    return compile_manifest(ProviderManifest(**raw))


@pytest.fixture(autouse=True)
def registry():
    init_db()
    before = dict(_registry)
    with SessionLocal() as db:
        db.query(Job).delete()
        db.query(JobExecutorRoute).filter(JobExecutorRoute.job_type == connector_run.JOB_TYPE).delete()
        db.commit()
    yield
    _registry.clear()
    _registry.update(before)
    with SessionLocal() as db:
        db.query(JobExecutorRoute).filter(JobExecutorRoute.job_type == connector_run.JOB_TYPE).delete()
        db.commit()


def _column(**kw):
    return {"id": "email", "name": "Email", "type": "enrichment", "provider": "route_free",
            "target_field": "email", "verify": False, **kw}


ROW = {"id": 7, "__row_id": 7, "__lead_id": None, "company": "Acme"}


def test_type_constants_and_timeout_ceiling():
    assert connector_run.JOB_TYPE == "run_workbook_connector"
    assert connector_run.WORKBOOK_RUN_JOB_TYPES == ("run_workbook", "run_workbook_connector")
    assert JOB_TIMEOUTS["run_workbook_connector"] == JOB_TIMEOUTS["run_workbook"] == 1800


def test_an_explicit_connector_chain_over_unlinked_rows_is_eligible():
    register_provider(_connector("route_free"))
    assert connector_run.ineligible_reason([_column()], [ROW]) is None
    waterfall = _column(type="waterfall", provider=None, waterfall=["route_free"])
    assert connector_run.ineligible_reason([waterfall], [ROW]) is None


@pytest.mark.parametrize("column, reason", [
    ({"type": "ai_formula"}, "column_type"),
    ({"type": "http"}, "column_type"),
    ({"condition": "{x} == 1"}, "condition"),
    ({"input_columns": ["other"]}, "column_references"),
    ({"verify": True}, "email_verify"),
    ({"provider": None}, "default_waterfall"),
    ({"provider": "does_not_exist"}, "not_a_connector"),
])
def test_unsupported_columns_stay_on_python(column, reason):
    register_provider(_connector("route_free"))
    got = connector_run.ineligible_reason([_column(**column)], [ROW])
    assert got and got.startswith(reason), got


def test_linked_and_legacy_rows_stay_on_python():
    register_provider(_connector("route_free"))
    assert connector_run.ineligible_reason([_column()], [{**ROW, "__lead_id": 3}]) == "linked_lead_rows"
    assert connector_run.ineligible_reason([_column()], [{"id": 1}]) == "legacy_lead_rows"


def test_connector_differences_between_the_two_hosts_stay_on_python(monkeypatch):
    register_provider(_connector("route_http", url="http://api.vendor.example/v1"))
    got = connector_run.ineligible_reason([_column(provider="route_http")], [ROW])
    assert got == "non_https_connector:route_http"

    register_provider(_connector("route_env", extra_header={"X-Other": "${env:SOME_OTHER_KEY}"},
                                 auth={"type": "header", "param": "X-Key", "value": "${env:ROUTE_KEY}",
                                       "env_var": "ROUTE_KEY"}))
    got = connector_run.ineligible_reason([_column(provider="route_env")], [ROW])
    assert got == "undeclared_env_reference:route_env"

    register_provider(_connector("route_auth", auth={"type": "header", "param": "X-Key",
                                                     "value": "${env:ROUTE_KEY}", "env_var": "ROUTE_KEY"}))
    monkeypatch.setenv("ROUTE_KEY", "k")
    assert connector_run.ineligible_reason([_column(provider="route_auth")], [ROW]) is None


def test_runtime_flags_that_the_go_executor_does_not_implement_keep_runs_on_python(monkeypatch):
    from apps.api.core.config import settings

    register_provider(_connector("route_free"))
    monkeypatch.setattr(settings, "AUTOMATIONS_ENABLED", True, raising=False)
    assert connector_run.ineligible_reason([_column()], [ROW]) == "automations_enabled"
    monkeypatch.setattr(settings, "AUTOMATIONS_ENABLED", False, raising=False)
    monkeypatch.setattr(settings, "PROVENANCE_TRACKING_ENABLED", True, raising=False)
    assert connector_run.ineligible_reason([_column()], [ROW]) == "provenance_tracking_enabled"


def test_with_no_route_every_run_is_run_workbook():
    register_provider(_connector("route_free"))
    with SessionLocal() as db:
        assert connector_run.select_job_type(db, [_column()], [ROW]) == "run_workbook"


def test_routing_to_go_switches_only_eligible_runs_and_rolls_back_with_one_command():
    register_provider(_connector("route_free"))
    with SessionLocal() as db:
        db.merge(JobExecutorRoute(job_type=connector_run.JOB_TYPE, executor="go"))
        db.commit()
        assert connector_run.select_job_type(db, [_column()], [ROW]) == "run_workbook_connector"
        # an ineligible run in the same workspace still goes to the legacy type
        assert connector_run.select_job_type(db, [_column(type="ai_formula")], [ROW]) == "run_workbook"
        # rollback: `opengtm routes set run_workbook_connector python`
        db.merge(JobExecutorRoute(job_type=connector_run.JOB_TYPE, executor="python"))
        db.commit()
        assert connector_run.select_job_type(db, [_column()], [ROW]) == "run_workbook"


def test_python_workers_keep_a_handler_for_the_new_type_so_rollback_needs_no_deploy():
    from apps.api.services.job_registry import register_job_handlers
    from apps.api.services.queue_service import QueueService
    from apps.api.services.workbook.enrichment import handle_run_workbook

    service = QueueService()
    names = register_job_handlers(service)
    assert "run_workbook_connector" in names
    assert service.handlers["run_workbook_connector"] is handle_run_workbook
