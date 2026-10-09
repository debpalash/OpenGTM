"""Repeated policy saves must not start parallel recurring refresh chains."""

from datetime import datetime, timedelta, timezone

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.core.tenancy import WorkspaceCtx
from apps.api.database import Base, get_db
from apps.api.models import Job
from apps.api.routers.workbooks import require_editor, router
from apps.api.services.workbook.models import Workbook


WORKSPACE = "ws-refresh-policy"
WORKBOOK = "wb-refresh-policy"
OTHER_WORKBOOK = "wb-refresh-other"
ACTIVE = {"pending", "processing"}


class _User:
    id = "refresh-policy-user"


def _ctx():
    return WorkspaceCtx(user=_User(), workspace_id=WORKSPACE, slug="refresh-test")


@pytest.fixture
def harness():
    engine = create_engine(
        "sqlite:///:memory:",
        connect_args={"check_same_thread": False},
        poolclass=StaticPool,
    )
    Base.metadata.create_all(engine)
    sessions = sessionmaker(bind=engine)
    with sessions() as db:
        db.add_all([
            Workbook(id=wid, name=wid, workspace_id=WORKSPACE, columns_config=[])
            for wid in (WORKBOOK, OTHER_WORKBOOK)
        ])
        db.commit()

    app = FastAPI()
    app.include_router(router)

    def override_db():
        with sessions() as db:
            yield db

    app.dependency_overrides[get_db] = override_db
    app.dependency_overrides[require_editor] = _ctx
    try:
        with TestClient(app) as client:
            yield client, sessions
    finally:
        engine.dispose()


def _policy(enabled=True, interval="hourly"):
    return {"enabled": enabled, "interval": interval,
            "on_signal": [], "staleness_ttl_days": {}}


def _put(client, policy):
    response = client.put(f"/api/workbooks/{WORKBOOK}/refresh-policy", json=policy)
    assert response.status_code == 200, response.text
    assert response.json()["refresh_policy"] == policy
    return response.json()


def _seed(sessions, status, due, workbook=WORKBOOK):
    with sessions() as db:
        job = Job(type="refresh_workbook",
                  payload={"workbook_id": workbook, "workspace_id": WORKSPACE},
                  workspace_id=WORKSPACE, status=status, next_run_at=due,
                  worker_id="fixture-worker" if status == "processing" else None,
                  started_at=due if status == "processing" else None,
                  locked_at=due if status == "processing" else None,
                  completed_at=due if status == "completed" else None)
        db.add(job)
        db.commit()
        return job.id


def _jobs(sessions, workbook=WORKBOOK):
    with sessions() as db:
        return [{"id": job.id, "status": job.status, "payload": job.payload,
                 "next_run_at": job.next_run_at, "worker_id": job.worker_id}
                for job in db.query(Job).filter(Job.type == "refresh_workbook")
                if job.payload.get("workbook_id") == workbook]


def _active(sessions):
    return [job for job in _jobs(sessions) if job["status"] in ACTIVE]


def test_identical_refresh_policy_put_keeps_one_recurring_chain(harness):
    client, sessions = harness
    other = _seed(sessions, "pending", datetime.now(timezone.utc) - timedelta(minutes=5),
                  OTHER_WORKBOOK)
    original_other = _jobs(sessions, OTHER_WORKBOOK)
    policy = _policy()
    assert _put(client, policy)["next_in_minutes"] == 60
    assert len(_active(sessions)) == 1
    _put(client, policy)
    assert _jobs(sessions, OTHER_WORKBOOK) == original_other
    assert original_other[0]["id"] == other
    assert len(_active(sessions)) == 1, "one policy must own one active recurring chain"


@pytest.mark.parametrize("status, offset", [
    ("pending", 30), ("pending", -30), ("processing", -5),
])
def test_policy_save_keeps_existing_active_refresh_chain(harness, status, offset):
    client, sessions = harness
    due = datetime.now(timezone.utc) + timedelta(minutes=offset)
    owner = _seed(sessions, status, due)
    with sessions() as db:
        db.get(Workbook, WORKBOOK).refresh_policy = _policy()
        db.commit()
    _put(client, _policy())
    active = _active(sessions)
    assert len(active) == 1, "saving must not fork a pending or claimed recurring chain"
    # A due or claimed owner must remain identifiable, not vanish behind a new job.
    assert active[0]["id"] == owner
    assert active[0]["status"] == status
    assert active[0]["payload"] == {"workbook_id": WORKBOOK, "workspace_id": WORKSPACE}


def test_changed_refresh_policy_is_persisted_without_assuming_reschedule_timing(harness):
    client, sessions = harness
    _put(client, _policy())
    changed = _policy(interval="weekly")
    changed["staleness_ttl_days"] = {"summary": 7}
    assert _put(client, changed)["next_in_minutes"] == 10080
    with sessions() as db:
        assert db.get(Workbook, WORKBOOK).refresh_policy == changed
    assert _active(sessions), "enabled policy must retain a scheduled or claimed chain"


def test_disabled_policy_does_not_enqueue_a_new_chain(harness):
    client, sessions = harness
    _put(client, _policy())
    previous_ids = {job["id"] for job in _jobs(sessions)}
    disabled = _policy(enabled=False)
    assert _put(client, disabled)["next_in_minutes"] is None
    assert {job["id"] for job in _jobs(sessions)} <= previous_ids
    with sessions() as db:
        assert db.get(Workbook, WORKBOOK).refresh_policy == disabled


def test_completed_refresh_history_does_not_block_new_enabled_schedule(harness):
    client, sessions = harness
    history = _seed(sessions, "completed", datetime.now(timezone.utc) - timedelta(days=1))
    assert _active(sessions) == []
    _put(client, _policy())
    assert len(_active(sessions)) == 1
    assert _active(sessions)[0]["id"] != history
    assert next(job for job in _jobs(sessions) if job["id"] == history)["status"] == "completed"
