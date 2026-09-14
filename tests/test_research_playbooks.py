import asyncio

import pytest
from fastapi import HTTPException
from starlette.requests import Request

from apps.api.routers.playbooks import (
    cancel_run,
    list_results,
    list_runs,
    playbook_capabilities,
    retry_run,
)

from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.database import Base
from apps.api.models import Job
from apps.api.services.audiences.models import Audience, AudienceMember
from apps.api.services.playbooks.models import PlaybookResult, PlaybookRun, PlaybookSchedule, ResearchPlaybook


def test_playbook_history_pages_are_stable_bounded_and_tenant_safe():
    from datetime import datetime, timedelta

    engine = create_engine(
        "sqlite:///:memory:",
        connect_args={"check_same_thread": False},
        poolclass=StaticPool,
    )
    Base.metadata.create_all(
        engine,
        tables=[Audience.__table__, ResearchPlaybook.__table__, PlaybookRun.__table__, PlaybookResult.__table__],
    )
    Session = sessionmaker(bind=engine)
    created = datetime.utcnow() - timedelta(days=1)
    with Session() as db:
        db.add(Audience(id="aud", workspace_id="ws", name="Target", filters={}))
        db.add(Audience(id="other-aud", workspace_id="other", name="Other", filters={}))
        db.add(ResearchPlaybook(id="pb", workspace_id="ws", name="Paged", prompt_template="Research fully"))
        db.add(ResearchPlaybook(id="other-pb", workspace_id="other", name="Other", prompt_template="Research fully"))
        for index in range(5):
            db.add(PlaybookRun(
                id=f"run-{index}", workspace_id="ws", playbook_id="pb", audience_id="aud",
                prompt_version=1, prompt_snapshot="Research fully", created_at=created,
            ))
        db.add(PlaybookRun(
            id="foreign-run", workspace_id="other", playbook_id="other-pb", audience_id="other-aud",
            prompt_version=1, prompt_snapshot="Research fully", created_at=created + timedelta(days=2),
        ))
        for lead_id in range(1, 7):
            db.add(PlaybookResult(
                workspace_id="ws", run_id="run-0", lead_id=lead_id, status="success", value="done",
            ))
        db.commit()
        ctx = type("Ctx", (), {"workspace_id": "ws"})()

        first = list_runs("pb", db=db, ctx=ctx, limit=2, offset=0, cursor=None)
        assert first["has_more"] is True and len(first["runs"]) == 2
        assert "foreign-run" not in {run["id"] for run in first["runs"]}
        db.add(PlaybookRun(
            id="new-run", workspace_id="ws", playbook_id="pb", audience_id="aud",
            prompt_version=1, prompt_snapshot="Research fully", created_at=created + timedelta(days=3),
        ))
        db.commit()
        second = list_runs("pb", db=db, ctx=ctx, limit=2, offset=0, cursor=first["next_cursor"])
        third = list_runs("pb", db=db, ctx=ctx, limit=2, offset=0, cursor=second["next_cursor"])
        traversed = first["runs"] + second["runs"] + third["runs"]
        assert len(traversed) == 5 and len({run["id"] for run in traversed}) == 5
        assert "new-run" not in {run["id"] for run in traversed}

        result_first = list_results("run-0", db=db, ctx=ctx, limit=2, offset=0, cursor=None)
        result_second = list_results(
            "run-0", db=db, ctx=ctx, limit=2, offset=0, cursor=result_first["next_cursor"],
        )
        assert [row["lead_id"] for row in result_first["results"] + result_second["results"]] == [1, 2, 3, 4]
        assert result_first["has_more"] is True and result_second["has_more"] is True

        with pytest.raises(HTTPException, match="invalid playbook run cursor"):
            list_runs("pb", db=db, ctx=ctx, limit=2, offset=0, cursor="bad")
        with pytest.raises(HTTPException, match="invalid playbook result cursor"):
            list_results("run-0", db=db, ctx=ctx, limit=2, offset=0, cursor="bad")


def test_agent_capabilities_fail_closed_without_live_evidence(monkeypatch):
    monkeypatch.delenv("OPENGTM_INTEGRATION_CERTIFICATIONS", raising=False)
    monkeypatch.delenv("OPENGTM_INTEGRATION_CERTIFICATION_KEY", raising=False)
    result = playbook_capabilities(ctx=type("Ctx", (), {"workspace_id": "ws"})())
    assert {item["id"] for item in result["capabilities"]} == {
        "grounded_research", "chained_playbooks", "audience_runs",
        "recurring_schedules",
    }
    assert all(item["maturity"] == "beta" for item in result["capabilities"])


def test_playbook_worker_is_resumable_and_versions_prompt(monkeypatch):
    engine = create_engine("sqlite:///:memory:", connect_args={"check_same_thread": False}, poolclass=StaticPool)
    Base.metadata.create_all(engine, tables=[Audience.__table__, AudienceMember.__table__, ResearchPlaybook.__table__, PlaybookRun.__table__, PlaybookResult.__table__])
    Session = sessionmaker(bind=engine)
    db = Session()
    db.add(Audience(id="aud", workspace_id="ws", name="Target", filters={}))
    db.add(AudienceMember(workspace_id="ws", audience_id="aud", lead_id=1, snapshot={"company": "Acme", "website": "acme.test"}))
    steps = [
        {"key": "signals", "name": "Signals", "prompt_template": "Research {company} at {website}", "output_format": "text"},
        {"key": "angle", "name": "Angle", "prompt_template": "Create an angle from {signals}", "output_format": "text"},
    ]
    playbook = ResearchPlaybook(id="pb", workspace_id="ws", name="Brief", prompt_template="Research {company} at {website}", steps=steps, version=3)
    run = PlaybookRun(id="run", workspace_id="ws", playbook_id="pb", audience_id="aud", prompt_version=3, prompt_snapshot=playbook.prompt_template, steps_snapshot=steps, max_members=10)
    db.add_all([playbook, run]); db.commit(); db.close()

    calls = []
    async def fake_execute(prompt, lead, columns, **kwargs):
        calls.append((prompt, dict(lead), kwargs["workspace_id"]))
        return {"success": True, "value": "Buying signals" if len(calls) == 1 else "Evidence-backed angle", "metadata": {"research": {"citations": ["https://acme.test"]}}}

    from apps.api.services.playbooks import engine as worker
    monkeypatch.setattr(worker, "SessionLocal", Session)
    monkeypatch.setattr("apps.api.services.workbook.research_column.execute_research_column", fake_execute)
    asyncio.run(worker.handle_playbook_run(1, {"workspace_id": "ws", "run_id": "run"}))
    asyncio.run(worker.handle_playbook_run(2, {"workspace_id": "ws", "run_id": "run"}))
    db = Session()
    saved = db.query(PlaybookResult).one()
    assert saved.status == "success" and saved.attempts == 1
    assert saved.value == "Evidence-backed angle" and saved.result_metadata["completed_steps"] == 2
    assert db.query(PlaybookRun).one().status == "completed"
    assert calls[0] == ("Research {company} at {website}", {"company": "Acme", "website": "acme.test"}, "ws")
    assert calls[1][0] == "Create an angle from {signals}" and calls[1][1]["signals"] == "Buying signals"
    db.close()


def test_playbook_queue_failure_preserves_success_and_closes_interrupted_work(monkeypatch):
    engine = create_engine("sqlite:///:memory:", connect_args={"check_same_thread": False}, poolclass=StaticPool)
    Base.metadata.create_all(engine, tables=[Audience.__table__, ResearchPlaybook.__table__, PlaybookRun.__table__, PlaybookResult.__table__])
    Session = sessionmaker(bind=engine)
    with Session() as db:
        db.add(Audience(id="aud", workspace_id="ws", name="Target", filters={}))
        db.add(ResearchPlaybook(id="pb", workspace_id="ws", name="Crash safe", prompt_template="Research {company}"))
        db.add(PlaybookRun(id="run", workspace_id="ws", playbook_id="pb", audience_id="aud", status="running", prompt_version=1, prompt_snapshot="Research {company}"))
        db.add_all([
            PlaybookResult(workspace_id="ws", run_id="run", lead_id=1, status="success", value="keep", attempts=1),
            PlaybookResult(workspace_id="ws", run_id="run", lead_id=2, status="running", attempts=1),
        ])
        db.commit()

    from apps.api.services.playbooks import engine as worker
    monkeypatch.setattr(worker, "SessionLocal", Session)
    payload = {"workspace_id": "ws", "run_id": "run"}
    worker.reconcile_playbook_job_failure(8, payload, "worker timeout", True)
    with Session() as db:
        run = db.get(PlaybookRun, "run")
        results = {result.lead_id: result for result in db.query(PlaybookResult).all()}
        assert run.status == "pending" and run.finished_at is None
        assert results[1].status == "success" and results[1].value == "keep"
        assert results[2].status == "pending"
        assert "worker timeout" in results[2].error

    worker.reconcile_playbook_job_failure(8, payload, "worker timeout", False)
    with Session() as db:
        run = db.get(PlaybookRun, "run")
        results = {result.lead_id: result for result in db.query(PlaybookResult).all()}
        assert run.status == "failed" and run.finished_at is not None
        assert run.attempted == 2 and run.succeeded == 1 and run.failed == 1
        assert results[1].status == "success" and results[1].value == "keep"
        assert results[2].status == "failed"
        assert "Final failure" in run.error


def test_playbook_schedule_is_durable_and_single_flight(monkeypatch):
    engine = create_engine("sqlite:///:memory:", connect_args={"check_same_thread": False}, poolclass=StaticPool)
    Base.metadata.create_all(engine, tables=[Job.__table__, Audience.__table__, ResearchPlaybook.__table__, PlaybookRun.__table__, PlaybookSchedule.__table__])
    Session = sessionmaker(bind=engine)
    db = Session()
    db.add(Audience(id="aud", workspace_id="ws", name="Target", filters={}))
    playbook = ResearchPlaybook(id="pb", workspace_id="ws", name="Scheduled", prompt_template="Research this account fully", schedule_audience_id="aud", schedule_interval_minutes=60)
    db.add(playbook); db.commit()
    from apps.api.services.playbooks import scheduler
    scheduler.schedule_next(db, playbook)
    scheduler.schedule_next(db, playbook)
    assert db.query(PlaybookSchedule).one().enabled is True
    assert db.query(Job).filter(Job.type == "research_playbook_schedule", Job.status == "pending").count() == 1
    scheduled = db.query(Job).filter(Job.type == "research_playbook_schedule", Job.status == "pending").one()
    db.close()

    monkeypatch.setattr(scheduler, "SessionLocal", Session)
    asyncio.run(scheduler.handle_playbook_schedule(scheduled.id, scheduled.payload))
    db = Session()
    assert db.query(PlaybookRun).count() == 1
    assert db.query(Job).filter(Job.type == "research_playbook_run", Job.status == "pending").count() == 1
    assert db.query(Job).filter(Job.type == "research_playbook_schedule", Job.status == "pending").count() == 1
    db.close()


def test_playbook_schedule_bootstrap_pages_only_due_work(monkeypatch):
    from datetime import datetime, timedelta, timezone

    engine = create_engine(
        "sqlite:///:memory:", connect_args={"check_same_thread": False},
        poolclass=StaticPool,
    )
    Base.metadata.create_all(engine, tables=[
        Job.__table__, Audience.__table__, ResearchPlaybook.__table__,
        PlaybookSchedule.__table__,
    ])
    Session = sessionmaker(bind=engine)
    now = datetime.now(timezone.utc)
    with Session() as db:
        db.add(Audience(id="aud", workspace_id="ws", name="Target", filters={}))
        for index in range(5):
            db.add(ResearchPlaybook(
                id=f"due-{index}", workspace_id="ws", name=f"Due {index}",
                prompt_template="Research fully", schedule_audience_id="aud",
                schedule_interval_minutes=60,
            ))
            db.add(PlaybookSchedule(
                playbook_id=f"due-{index}", workspace_id="ws", enabled=True,
                next_run_at=now - timedelta(minutes=1),
            ))
        db.add(ResearchPlaybook(
            id="future", workspace_id="ws", name="Future",
            prompt_template="Research fully", schedule_audience_id="aud",
            schedule_interval_minutes=60,
        ))
        db.add(PlaybookSchedule(
            playbook_id="future", workspace_id="ws", enabled=True,
            next_run_at=now + timedelta(days=1),
        ))
        db.commit()

    from apps.api.services.playbooks import scheduler
    monkeypatch.setattr(scheduler, "SessionLocal", Session)
    monkeypatch.setattr(scheduler, "PLAYBOOK_BOOTSTRAP_PAGE_SIZE", 2)

    assert scheduler.bootstrap_playbook_schedules() == 5
    assert scheduler.bootstrap_playbook_schedules() == 0
    with Session() as db:
        jobs = db.query(Job).filter(
            Job.type == "research_playbook_schedule", Job.status == "pending",
        ).all()
        assert {job.payload["playbook_id"] for job in jobs} == {
            "due-0", "due-1", "due-2", "due-3", "due-4",
        }


def test_failed_playbook_profiles_retry_in_place_without_repeating_success(monkeypatch):
    engine = create_engine("sqlite:///:memory:", connect_args={"check_same_thread": False}, poolclass=StaticPool)
    Base.metadata.create_all(engine, tables=[Job.__table__, Audience.__table__, AudienceMember.__table__, ResearchPlaybook.__table__, PlaybookRun.__table__, PlaybookResult.__table__])
    Session = sessionmaker(bind=engine)
    with Session() as db:
        db.add(Audience(id="aud", workspace_id="ws", name="Target", filters={}))
        db.add_all([
            AudienceMember(workspace_id="ws", audience_id="aud", lead_id=1, snapshot={"company": "Done"}),
            AudienceMember(workspace_id="ws", audience_id="aud", lead_id=2, snapshot={"company": "Retry"}),
        ])
        playbook = ResearchPlaybook(id="pb", workspace_id="ws", name="Retry", prompt_template="Research {company} completely")
        run = PlaybookRun(id="run", workspace_id="ws", playbook_id="pb", audience_id="aud", status="completed_with_errors", prompt_version=1, prompt_snapshot=playbook.prompt_template, failed=1, succeeded=1)
        db.add_all([playbook, run,
            PlaybookResult(workspace_id="ws", run_id="run", lead_id=1, status="success", value="keep", attempts=1),
            PlaybookResult(workspace_id="ws", run_id="run", lead_id=2, status="failed", error="temporary", attempts=1),
        ])
        db.commit()
        request = Request({"type": "http", "method": "POST", "path": "/retry", "headers": []})
        ctx = type("Ctx", (), {"workspace_id": "ws", "user": type("User", (), {"id": 7})()})()
        response = retry_run("run", request, db=db, ctx=ctx)
        assert response["status"] == "pending"
        assert db.query(Job).one().payload["run_id"] == "run"
        assert request.state.audit_metadata["previous_failed"] == 1

    calls = []
    async def fake_execute(prompt, lead, columns, **kwargs):
        calls.append(lead["company"])
        return {"success": True, "value": "recovered", "metadata": {}}

    from apps.api.services.playbooks import engine as worker
    monkeypatch.setattr(worker, "SessionLocal", Session)
    monkeypatch.setattr("apps.api.services.workbook.research_column.execute_research_column", fake_execute)
    asyncio.run(worker.handle_playbook_run(2, {"workspace_id": "ws", "run_id": "run"}))

    with Session() as db:
        results = {row.lead_id: row for row in db.query(PlaybookResult).all()}
        assert calls == ["Retry"]
        assert results[1].value == "keep" and results[1].attempts == 1
        assert results[2].value == "recovered" and results[2].attempts == 2
        saved = db.query(PlaybookRun).one()
        assert saved.status == "completed" and saved.succeeded == 2 and saved.failed == 0


def test_playbook_run_cancel_is_durable_audited_and_resumable():
    engine = create_engine("sqlite:///:memory:", connect_args={"check_same_thread": False}, poolclass=StaticPool)
    Base.metadata.create_all(engine, tables=[Job.__table__, Audience.__table__, ResearchPlaybook.__table__, PlaybookRun.__table__])
    Session = sessionmaker(bind=engine)
    with Session() as db:
        db.add(Audience(id="aud", workspace_id="ws", name="Target", filters={}))
        db.add(ResearchPlaybook(id="pb", workspace_id="ws", name="Cancel", prompt_template="Research this account fully"))
        db.add(PlaybookRun(id="run", workspace_id="ws", playbook_id="pb", audience_id="aud", status="pending", prompt_version=1, prompt_snapshot="Research this account fully"))
        db.add(Job(type="research_playbook_run", workspace_id="ws", status="pending", fire_key="playbook:run", payload={"workspace_id": "ws", "run_id": "run"}))
        db.commit()
        request = Request({"type": "http", "method": "POST", "path": "/cancel", "headers": []})
        ctx = type("Ctx", (), {"workspace_id": "ws", "user": type("User", (), {"id": 7})()})()
        response = cancel_run("run", request, db=db, ctx=ctx)
        assert response["status"] == "cancelled"
        assert db.query(Job).one().status == "cancelled"
        assert request.state.audit_metadata["action"] == "research_playbook.run.cancel"

        retried = retry_run("run", request, db=db, ctx=ctx)
        assert retried["status"] == "pending"
        assert db.query(Job).filter(Job.status == "pending").count() == 1


def test_playbook_worker_honors_cancellation_between_profiles(monkeypatch):
    engine = create_engine("sqlite:///:memory:", connect_args={"check_same_thread": False}, poolclass=StaticPool)
    Base.metadata.create_all(engine, tables=[Audience.__table__, AudienceMember.__table__, ResearchPlaybook.__table__, PlaybookRun.__table__, PlaybookResult.__table__])
    Session = sessionmaker(bind=engine)
    with Session() as db:
        db.add(Audience(id="aud", workspace_id="ws", name="Target", filters={}))
        db.add_all([
            AudienceMember(workspace_id="ws", audience_id="aud", lead_id=1, snapshot={"company": "First"}),
            AudienceMember(workspace_id="ws", audience_id="aud", lead_id=2, snapshot={"company": "Second"}),
        ])
        db.add(ResearchPlaybook(id="pb", workspace_id="ws", name="Cancel", prompt_template="Research {company} fully"))
        db.add(PlaybookRun(id="run", workspace_id="ws", playbook_id="pb", audience_id="aud", prompt_version=1, prompt_snapshot="Research {company} fully"))
        db.commit()

    calls = []
    async def execute_then_cancel(prompt, lead, columns, **kwargs):
        calls.append(lead["company"])
        with Session() as other:
            run = other.get(PlaybookRun, "run")
            run.status, run.error = "cancelled", "Cancelled by user"
            other.commit()
        return {"success": True, "value": "preserved", "metadata": {}}

    from apps.api.services.playbooks import engine as worker
    monkeypatch.setattr(worker, "SessionLocal", Session)
    monkeypatch.setattr("apps.api.services.workbook.research_column.execute_research_column", execute_then_cancel)
    asyncio.run(worker.handle_playbook_run(1, {"workspace_id": "ws", "run_id": "run"}))
    with Session() as db:
        assert calls == ["First"]
        assert db.get(PlaybookRun, "run").status == "cancelled"
        assert db.query(PlaybookResult).one().status == "success"
