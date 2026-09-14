import asyncio
from datetime import datetime, timedelta, timezone

import pytest
from fastapi import HTTPException
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.database import Base
from apps.api.models import Job
from apps.api.services.audiences.models import Audience, AudienceMembershipEvent
from apps.api.services.destinations.models import AudienceDestination, DestinationDelivery, DestinationInboundReceipt, DestinationRun
from apps.api.services.governance.models import GovernanceAuditEvent, RetentionPolicy, RetentionRun, RetentionSchedule
from apps.api.services.leadgen.orm_models import LLMUsageRow, SignalRow
from apps.api.services.outreach.orm_models import OutreachSend
from apps.api.services.playbooks.models import PlaybookResult, PlaybookRun, ResearchPlaybook
from apps.api.services.governance.retention import normalized_days, preview_retention
from apps.api.routers.governance import retention_runs


def _session():
    engine = create_engine("sqlite:///:memory:", connect_args={"check_same_thread": False}, poolclass=StaticPool)
    Base.metadata.create_all(engine, tables=[Job.__table__, Audience.__table__, AudienceMembershipEvent.__table__, AudienceDestination.__table__, DestinationRun.__table__, DestinationDelivery.__table__, DestinationInboundReceipt.__table__, ResearchPlaybook.__table__, PlaybookRun.__table__, PlaybookResult.__table__, SignalRow.__table__, LLMUsageRow.__table__, OutreachSend.__table__, GovernanceAuditEvent.__table__, RetentionPolicy.__table__, RetentionSchedule.__table__, RetentionRun.__table__])
    return sessionmaker(bind=engine)


def _audit(ws, request_id, when):
    return GovernanceAuditEvent(workspace_id=ws, actor_role="admin", method="POST", route="/x", resource_path="/x", response_status=200, outcome="success", request_id=request_id, metadata_json={}, created_at=when)


def test_retention_run_history_is_stable_bounded_and_tenant_safe():
    Session = _session()
    created = datetime.now(timezone.utc) - timedelta(days=1)
    with Session() as db:
        db.add_all([
            RetentionRun(
                id=f"retention-{index}", workspace_id="ws",
                requested_by="admin", policy_snapshot={}, created_at=created,
            )
            for index in range(5)
        ])
        db.add(RetentionRun(
            id="foreign-retention", workspace_id="other",
            requested_by="admin", policy_snapshot={}, created_at=created + timedelta(days=2),
        ))
        db.commit()
        ctx = type("Ctx", (), {"workspace_id": "ws"})()

        first = retention_runs(db=db, ctx=ctx, limit=2, offset=0, cursor=None)
        assert first["has_more"] is True and len(first["runs"]) == 2
        assert "foreign-retention" not in {run["id"] for run in first["runs"]}
        db.add(RetentionRun(
            id="new-retention", workspace_id="ws",
            requested_by="admin", policy_snapshot={}, created_at=created + timedelta(days=3),
        ))
        db.commit()
        second = retention_runs(
            db=db, ctx=ctx, limit=2, offset=0, cursor=first["next_cursor"],
        )
        third = retention_runs(
            db=db, ctx=ctx, limit=2, offset=0, cursor=second["next_cursor"],
        )
        traversed = first["runs"] + second["runs"] + third["runs"]
        assert len(traversed) == 5 and len({run["id"] for run in traversed}) == 5
        assert "new-retention" not in {run["id"] for run in traversed}
        with pytest.raises(HTTPException, match="invalid retention run cursor"):
            retention_runs(db=db, ctx=ctx, limit=2, offset=0, cursor="bad")


def test_retention_preview_and_enforcement_are_tenant_scoped(monkeypatch):
    Session = _session(); db = Session(); now = datetime.now(timezone.utc)
    db.add_all([_audit("ws", "old", now - timedelta(days=400)), _audit("ws", "new", now), _audit("other", "other", now - timedelta(days=400))])
    db.add_all([
        LLMUsageRow(workspace_id="ws", provider="old", date=(now - timedelta(days=400)).date().isoformat(), updated_at="old"),
        LLMUsageRow(workspace_id="ws", provider="new", date=now.date().isoformat(), updated_at="new"),
        LLMUsageRow(workspace_id="other", provider="other", date=(now - timedelta(days=400)).date().isoformat(), updated_at="old"),
    ])
    policy = RetentionPolicy(workspace_id="ws", enabled=False, legal_hold=False, retention_days=normalized_days({"audit": 365}))
    run = RetentionRun(id="run", workspace_id="ws", requested_by="1", policy_snapshot=policy.retention_days)
    db.add_all([policy, run]); db.commit()
    assert preview_retention(db, "ws", policy.retention_days, now)["audit"] == 1
    assert preview_retention(db, "ws", policy.retention_days, now)["llm_usage"] == 1
    db.close()
    from apps.api.services.governance import retention
    monkeypatch.setattr(retention, "SessionLocal", Session)
    asyncio.run(retention.handle_retention_enforce(1, {"workspace_id": "ws", "run_id": "run"}))
    db = Session()
    assert {x.request_id for x in db.query(GovernanceAuditEvent).all()} == {"new", "other"}
    assert {(x.workspace_id, x.provider) for x in db.query(LLMUsageRow).all()} == {("ws", "new"), ("other", "other")}
    saved = db.query(RetentionRun).one()
    assert saved.status == "completed" and saved.deleted_counts["audit"] == 1
    assert saved.deleted_counts["llm_usage"] == 1
    db.close()


def test_legal_hold_blocks_even_manual_enforcement(monkeypatch):
    Session = _session(); db = Session()
    policy = RetentionPolicy(workspace_id="ws", enabled=True, legal_hold=True, retention_days=normalized_days({}))
    run = RetentionRun(id="run", workspace_id="ws", requested_by="1", policy_snapshot=policy.retention_days)
    db.add_all([policy, run]); db.commit(); db.close()
    from apps.api.services.governance import retention
    monkeypatch.setattr(retention, "SessionLocal", Session)
    asyncio.run(retention.handle_retention_enforce(1, {"workspace_id": "ws", "run_id": "run"}))
    db = Session(); assert db.query(RetentionRun).one().status == "cancelled"; db.close()


def test_killed_scheduled_retention_creates_retry_evidence_and_recurs(monkeypatch):
    Session = _session()
    with Session() as db:
        policy = RetentionPolicy(
            workspace_id="ws", enabled=True, legal_hold=False,
            retention_days=normalized_days({}),
        )
        job = Job(
            type="retention_enforce", workspace_id="ws", status="pending",
            payload={"workspace_id": "ws"}, fire_key="retention:ws:today",
        )
        db.add_all([policy, job]); db.commit()
        job_id = job.id

    from apps.api.services.governance import retention
    monkeypatch.setattr(retention, "SessionLocal", Session)
    payload = {"workspace_id": "ws"}
    retention.reconcile_retention_job_failure(job_id, payload, "worker timeout", True)
    with Session() as db:
        run = db.query(RetentionRun).one()
        queue_job = db.get(Job, job_id)
        assert run.status == "pending" and "Queue retry scheduled" in run.error
        assert queue_job.payload["run_id"] == run.id
        run_id = run.id
        queue_job.status = "failed"
        db.commit()

    retention.reconcile_retention_job_failure(
        job_id, {"workspace_id": "ws", "run_id": run_id}, "worker timeout", False,
    )
    with Session() as db:
        run = db.get(RetentionRun, run_id)
        policy = db.get(RetentionPolicy, "ws")
        assert run.status == "failed" and run.finished_at is not None
        assert "Final failure: worker timeout" in run.error
        assert policy.next_run_at is not None
        scheduled = db.query(Job).filter(
            Job.type == "retention_enforce", Job.status == "pending",
            Job.fire_key.like("retention:ws:%"),
        ).all()
        assert len(scheduled) == 1


def test_retention_minimums_are_enforced():
    with pytest.raises(ValueError, match="audit retention must be 90"):
        normalized_days({"audit": 30})
    with pytest.raises(ValueError, match="unsupported retention"):
        normalized_days({"leads": 30})
