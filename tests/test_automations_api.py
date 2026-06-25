"""Trigger Engine — API router tests (SQLite, offline). Maps to AC-1.

Auth/workspace deps are overridden so we exercise validation + CRUD logic without
a real auth stack. Covers: CRUD, invalid trigger/action types, on_signal 409 when
PG off, legacy outreach 409, re_enrich column validation, pause/resume, preview
charges nothing, router 404 when disabled.
"""

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.core.config import settings
from apps.api.core.tenancy import WorkspaceCtx, current_workspace
from apps.api.database import Base, get_db
from apps.api.services.automations.models import (
    Trigger, TriggerRun, TriggerActionResult, TriggerCapReservation, ScheduledTrigger,
)
from apps.api.services.workbook.models import Workbook, WorkbookRow
from apps.api.services.billing.models import WorkspaceCredit, CreditLedgerEntry
from apps.api.services.workbook import activity_models as act_models
from apps.api.routers.automations import router as automations_router, require_admin

WS = "ws_api"


class _User:
    id = "user-1"


@pytest.fixture()
def client(monkeypatch):
    monkeypatch.setattr(settings, "AUTOMATIONS_ENABLED", True, raising=False)
    monkeypatch.setattr(settings, "PG_LEAD_STORE", False, raising=False)
    monkeypatch.setattr(settings, "AUTOMATIONS_ALLOW_LEGACY_OUTREACH", False, raising=False)

    engine = create_engine(
        "sqlite:///:memory:", connect_args={"check_same_thread": False}, poolclass=StaticPool,
    )
    Base.metadata.create_all(engine, tables=[
        Trigger.__table__, TriggerRun.__table__, TriggerActionResult.__table__,
        TriggerCapReservation.__table__, ScheduledTrigger.__table__,
        Workbook.__table__, WorkbookRow.__table__,
        WorkspaceCredit.__table__, CreditLedgerEntry.__table__,
        act_models.WorkbookActivity.__table__,
    ])
    Session = sessionmaker(bind=engine)

    app = FastAPI()
    app.include_router(automations_router)

    def _override_db():
        s = Session()
        try:
            yield s
        finally:
            s.close()

    def _override_ws():
        return WorkspaceCtx(user=_User(), workspace_id=WS, slug="api")

    app.dependency_overrides[get_db] = _override_db
    app.dependency_overrides[current_workspace] = _override_ws
    app.dependency_overrides[require_admin] = _override_ws

    return TestClient(app), Session


def _mk_workbook(Session, columns):
    s = Session()
    wb = Workbook(name="WB", workspace_id=WS, columns_config=columns)
    s.add(wb)
    s.commit()
    wid = wb.id
    s.close()
    return wid


def test_create_list_get_update_delete(client):
    tc, _ = client
    body = {
        "name": "rule1",
        "trigger_type": "on_row_added",
        "trigger_config": {},
        "condition": '{score} > 50',
        "actions": [{"type": "webhook", "config": {"url": "https://hooks.example.com/x"}}],
    }
    r = tc.post("/api/automations/triggers", json=body)
    assert r.status_code == 200, r.text
    tid = r.json()["id"]

    r = tc.get("/api/automations/triggers")
    assert r.status_code == 200 and len(r.json()) == 1

    r = tc.get(f"/api/automations/triggers/{tid}")
    assert r.status_code == 200 and "recent_runs" in r.json()

    r = tc.patch(f"/api/automations/triggers/{tid}", json={"name": "rule1b"})
    assert r.status_code == 200 and r.json()["name"] == "rule1b"

    r = tc.delete(f"/api/automations/triggers/{tid}")
    assert r.status_code == 200 and r.json()["deleted"] is True
    assert tc.get("/api/automations/triggers").json() == []


def test_invalid_trigger_type_rejected(client):
    tc, _ = client
    r = tc.post("/api/automations/triggers", json={
        "name": "bad", "trigger_type": "on_full_moon", "actions": [],
    })
    assert r.status_code == 422


def test_invalid_action_type_rejected(client):
    tc, _ = client
    r = tc.post("/api/automations/triggers", json={
        "name": "bad", "trigger_type": "on_row_added",
        "actions": [{"type": "teleport", "config": {}}],
    })
    assert r.status_code == 422


def test_on_signal_allowed_without_pg(client):
    """Self-host on_signal: on_signal rules are now allowed on SQLite (no 409).

    The scanner + poller write through the shared ORM signal store so
    emit_signal_matches fires on SQLite too; the signals table is present on both
    backends. (Previously gated 409 when PG_LEAD_STORE was off — gate removed.)"""
    tc, _ = client
    r = tc.post("/api/automations/triggers", json={
        "name": "sig", "trigger_type": "on_signal",
        "trigger_config": {"signal_types": ["hiring"]}, "actions": [],
    })
    assert r.status_code == 200, r.text
    assert r.json()["trigger_type"] == "on_signal"


def test_legacy_outreach_rejected(client):
    """AC-1: sequencer/send_email rejected (409) in v1."""
    tc, _ = client
    for atype in ("sequencer", "send_email"):
        r = tc.post("/api/automations/triggers", json={
            "name": "leg", "trigger_type": "on_row_added",
            "actions": [{"type": atype, "config": {}}],
        })
        assert r.status_code == 409
        assert r.json()["detail"] == "legacy_outreach_disabled"


def test_reenrich_column_validation(client):
    """AC-1: re_enrich column_ids validated against scoped workbook."""
    tc, Session = client
    wid = _mk_workbook(Session, [{"id": "colA", "name": "A", "type": "waterfall", "target_field": "email"}])

    # unknown column → 422
    r = tc.post("/api/automations/triggers", json={
        "name": "re", "trigger_type": "on_row_added", "scope_workbook_ids": [wid],
        "actions": [{"type": "re_enrich", "config": {"column_ids": ["does_not_exist"]}}],
    })
    assert r.status_code == 422

    # valid column → ok
    r = tc.post("/api/automations/triggers", json={
        "name": "re2", "trigger_type": "on_row_added", "scope_workbook_ids": [wid],
        "actions": [{"type": "re_enrich", "config": {"column_ids": ["colA"]}}],
    })
    assert r.status_code == 200, r.text


def test_unparseable_condition_rejected(client):
    tc, _ = client
    r = tc.post("/api/automations/triggers", json={
        "name": "c", "trigger_type": "on_row_added",
        "condition": "{a} >< 1", "actions": [],
    })
    assert r.status_code == 422


def test_pause_resume(client):
    """AC-9: pause/resume toggle enabled."""
    tc, _ = client
    r = tc.post("/api/automations/triggers", json={
        "name": "p", "trigger_type": "on_row_added", "actions": [],
    })
    tid = r.json()["id"]
    assert tc.post(f"/api/automations/triggers/{tid}/pause").json()["enabled"] is False
    assert tc.post(f"/api/automations/triggers/{tid}/resume").json()["enabled"] is True


def test_preview_no_writes(client):
    """AC-11: preview returns matched + projected cost, writes nothing."""
    tc, Session = client
    wid = _mk_workbook(Session, [
        {"id": "email", "name": "email", "type": "waterfall", "target_field": "email"},
        {"id": "score", "name": "score", "type": "lead_field", "lead_field": "score"},
    ])
    s = Session()
    s.add(WorkbookRow(workbook_id=wid, workspace_id=WS, position=0, data={"email": "a@b", "score": 90}))
    s.commit()
    s.close()

    r = tc.post("/api/automations/triggers", json={
        "name": "prev", "trigger_type": "on_row_added", "scope_workbook_ids": [wid],
        "condition": '{score} > 50',
        "actions": [{"type": "webhook", "config": {"url": "https://hooks.example.com/x"}}],
    })
    tid = r.json()["id"]
    r = tc.post(f"/api/automations/triggers/{tid}/preview", json={"limit": 10})
    assert r.status_code == 200, r.text
    body = r.json()
    assert body["matched"] == 1
    assert body["projected_total_usd"] == 0.0  # webhook is free
    assert body["sample"][0]["actions"][0]["type"] == "webhook"
    # no action-result rows written
    s = Session()
    assert s.query(TriggerActionResult).count() == 0
    s.close()


def test_router_404_when_disabled(client, monkeypatch):
    tc, _ = client
    monkeypatch.setattr(settings, "AUTOMATIONS_ENABLED", False, raising=False)
    r = tc.get("/api/automations/triggers")
    assert r.status_code == 404
