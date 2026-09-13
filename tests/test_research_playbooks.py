import asyncio

from apps.api.routers.playbooks import playbook_capabilities

from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.database import Base
from apps.api.models import Job
from apps.api.services.audiences.models import Audience, AudienceMember
from apps.api.services.playbooks.models import PlaybookResult, PlaybookRun, PlaybookSchedule, ResearchPlaybook


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
