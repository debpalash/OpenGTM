"""AC10 — legacy outreach flag gate survives the type-acceptance change.

With AUTOMATIONS_ALLOW_LEGACY_OUTREACH OFF, creating a rule with send_email /
sequencer returns 409 legacy_outreach_disabled (the gate lives in the action-type
validation branch, NOT a move into ACTION_TYPES_V1). SQLite, offline.
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
from apps.api.services.outreach import orm_models as om
from apps.api.routers.automations import router as automations_router, require_admin

WS = "ws_legacy"


class _User:
    id = "u1"


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
        om.OutreachSequence.__table__, om.OutreachEnrollment.__table__,
        om.OutreachSend.__table__, om.OutreachSuppression.__table__, om.OutreachSchedule.__table__,
    ])
    SL = sessionmaker(bind=engine)
    import apps.api.database as database
    import apps.api.services.outreach.store as store_mod
    monkeypatch.setattr(database, "SessionLocal", SL, raising=False)
    monkeypatch.setattr(store_mod, "SessionLocal", SL, raising=False)

    app = FastAPI()
    app.include_router(automations_router)

    def _db():
        s = SL()
        try:
            yield s
        finally:
            s.close()

    def _ws():
        return WorkspaceCtx(user=_User(), workspace_id=WS, slug="x")

    app.dependency_overrides[get_db] = _db
    app.dependency_overrides[current_workspace] = _ws
    app.dependency_overrides[require_admin] = _ws
    return TestClient(app), SL


@pytest.mark.parametrize("atype", ["send_email", "sequencer"])
def test_legacy_outreach_disabled_when_flag_off(client, atype):
    tc, _ = client
    r = tc.post("/api/automations/triggers", json={
        "name": "r", "trigger_type": "on_row_added", "trigger_config": {},
        "actions": [{"type": atype, "config": {"sequence_id": "x"}}],
    })
    assert r.status_code == 409
    assert r.json()["detail"] == "legacy_outreach_disabled"


def test_sequencer_accepted_when_flag_on(client, monkeypatch):
    tc, SL = client
    monkeypatch.setattr(settings, "AUTOMATIONS_ALLOW_LEGACY_OUTREACH", True, raising=False)
    # Seed a sequence in the workspace so validation passes.
    from apps.api.services.outreach.store import PgOutreachStore
    seq = PgOutreachStore(WS).create_sequence("S", consent_basis="legit")
    r = tc.post("/api/automations/triggers", json={
        "name": "r", "trigger_type": "on_row_added", "trigger_config": {},
        "actions": [{"type": "sequencer", "config": {"sequence_id": seq["id"]}}],
    })
    assert r.status_code in (200, 201), r.text


def test_sequencer_unknown_sequence_404_when_flag_on(client, monkeypatch):
    tc, _ = client
    monkeypatch.setattr(settings, "AUTOMATIONS_ALLOW_LEGACY_OUTREACH", True, raising=False)
    r = tc.post("/api/automations/triggers", json={
        "name": "r", "trigger_type": "on_row_added", "trigger_config": {},
        "actions": [{"type": "sequencer", "config": {"sequence_id": "nope"}}],
    })
    assert r.status_code == 404
