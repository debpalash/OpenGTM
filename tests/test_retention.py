import asyncio
from datetime import datetime, timedelta, timezone

import pytest
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.database import Base
from apps.api.models import Job
from apps.api.services.audiences.models import Audience, AudienceMembershipEvent
from apps.api.services.destinations.models import AudienceDestination, DestinationDelivery, DestinationRun
from apps.api.services.governance.models import GovernanceAuditEvent, RetentionPolicy, RetentionRun, RetentionSchedule
from apps.api.services.leadgen.orm_models import SignalRow
from apps.api.services.outreach.orm_models import OutreachSend
from apps.api.services.playbooks.models import PlaybookResult, PlaybookRun, ResearchPlaybook
from apps.api.services.governance.retention import normalized_days, preview_retention


def _session():
    engine = create_engine("sqlite:///:memory:", connect_args={"check_same_thread": False}, poolclass=StaticPool)
    Base.metadata.create_all(engine, tables=[Job.__table__, Audience.__table__, AudienceMembershipEvent.__table__, AudienceDestination.__table__, DestinationRun.__table__, DestinationDelivery.__table__, ResearchPlaybook.__table__, PlaybookRun.__table__, PlaybookResult.__table__, SignalRow.__table__, OutreachSend.__table__, GovernanceAuditEvent.__table__, RetentionPolicy.__table__, RetentionSchedule.__table__, RetentionRun.__table__])
    return sessionmaker(bind=engine)


def _audit(ws, request_id, when):
    return GovernanceAuditEvent(workspace_id=ws, actor_role="admin", method="POST", route="/x", resource_path="/x", response_status=200, outcome="success", request_id=request_id, metadata_json={}, created_at=when)


def test_retention_preview_and_enforcement_are_tenant_scoped(monkeypatch):
    Session = _session(); db = Session(); now = datetime.now(timezone.utc)
    db.add_all([_audit("ws", "old", now - timedelta(days=400)), _audit("ws", "new", now), _audit("other", "other", now - timedelta(days=400))])
    policy = RetentionPolicy(workspace_id="ws", enabled=False, legal_hold=False, retention_days=normalized_days({"audit": 365}))
    run = RetentionRun(id="run", workspace_id="ws", requested_by="1", policy_snapshot=policy.retention_days)
    db.add_all([policy, run]); db.commit()
    assert preview_retention(db, "ws", policy.retention_days, now)["audit"] == 1
    db.close()
    from apps.api.services.governance import retention
    monkeypatch.setattr(retention, "SessionLocal", Session)
    asyncio.run(retention.handle_retention_enforce(1, {"workspace_id": "ws", "run_id": "run"}))
    db = Session()
    assert {x.request_id for x in db.query(GovernanceAuditEvent).all()} == {"new", "other"}
    saved = db.query(RetentionRun).one()
    assert saved.status == "completed" and saved.deleted_counts["audit"] == 1
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


def test_retention_minimums_are_enforced():
    with pytest.raises(ValueError, match="audit retention must be 90"):
        normalized_days({"audit": 30})
    with pytest.raises(ValueError, match="unsupported retention"):
        normalized_days({"leads": 30})
