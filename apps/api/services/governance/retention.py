"""Previewable, legal-hold-aware retention enforcement."""
from datetime import datetime, timedelta, timezone

from apps.api.database import SessionLocal

DEFAULT_DAYS = {"audit": 365, "signals": 365, "activation": 180, "audience_history": 365, "agent_results": 180, "outreach_history": 365}
MIN_DAYS = {"audit": 90, "signals": 30, "activation": 30, "audience_history": 30, "agent_results": 30, "outreach_history": 30}


def normalized_days(value: dict) -> dict:
    unknown = set(value or {}) - set(DEFAULT_DAYS)
    if unknown:
        raise ValueError(f"unsupported retention categories: {', '.join(sorted(unknown))}")
    result = dict(DEFAULT_DAYS)
    for key, raw in (value or {}).items():
        days = int(raw)
        if not MIN_DAYS[key] <= days <= 3650:
            raise ValueError(f"{key} retention must be {MIN_DAYS[key]}..3650 days")
        result[key] = days
    return result


def _targets(db, workspace_id: str, days: dict, now: datetime):
    from apps.api.services.audiences.models import AudienceMembershipEvent
    from apps.api.services.destinations.models import DestinationDelivery, DestinationInboundReceipt
    from apps.api.services.governance.models import GovernanceAuditEvent
    from apps.api.services.leadgen.orm_models import SignalRow
    from apps.api.services.outreach.orm_models import OutreachSend
    from apps.api.services.playbooks.models import PlaybookResult
    models = [
        ("audit", GovernanceAuditEvent, GovernanceAuditEvent.created_at, False),
        ("signals", SignalRow, SignalRow.created_at, True),
        ("activation", DestinationDelivery, DestinationDelivery.created_at, False),
        ("activation", DestinationInboundReceipt, DestinationInboundReceipt.created_at, False),
        ("audience_history", AudienceMembershipEvent, AudienceMembershipEvent.created_at, False),
        ("agent_results", PlaybookResult, PlaybookResult.created_at, False),
        ("outreach_history", OutreachSend, OutreachSend.created_at, False),
    ]
    for category, model, column, epoch in models:
        cutoff = now - timedelta(days=days[category])
        value = cutoff.timestamp() if epoch else cutoff
        yield category, db.query(model).filter(model.workspace_id == workspace_id, column < value)


def preview_retention(db, workspace_id: str, days: dict, now: datetime | None = None) -> dict:
    now = now or datetime.now(timezone.utc)
    counts = {category: 0 for category in DEFAULT_DAYS}
    for category, query in _targets(db, workspace_id, normalized_days(days), now):
        counts[category] += query.count()
    return counts


def schedule_policy(db, policy, now: datetime | None = None):
    from apps.api.services.governance.models import RetentionSchedule
    from apps.api.services.job_scheduling import enqueue_job_once
    from apps.api.models import Job
    now = now or datetime.now(timezone.utc)
    db.query(Job).filter(Job.type == "retention_enforce", Job.status == "pending", Job.fire_key.like(f"retention:{policy.workspace_id}:%")).update({Job.status: "cancelled"}, synchronize_session=False)
    next_at = now + timedelta(days=1) if policy.enabled and not policy.legal_hold else None
    mirror = db.query(RetentionSchedule).filter(RetentionSchedule.workspace_id == policy.workspace_id).first()
    if mirror is None:
        mirror = RetentionSchedule(workspace_id=policy.workspace_id)
        db.add(mirror)
    mirror.enabled, mirror.next_run_at = bool(next_at), next_at
    policy.next_run_at = next_at
    if next_at:
        enqueue_job_once(db, job_type="retention_enforce", payload={"workspace_id": policy.workspace_id}, fire_key=f"retention:{policy.workspace_id}:{next_at.date().isoformat()}", next_run_at=next_at)
    db.commit()
    return next_at


async def handle_retention_enforce(job_id: int, payload: dict) -> None:
    workspace_id, run_id = payload.get("workspace_id"), payload.get("run_id")
    if not workspace_id:
        raise ValueError("retention enforcement requires workspace_id")
    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.governance.models import RetentionPolicy, RetentionRun
    with workspace_scope(workspace_id):
        with SessionLocal() as db:
            policy = db.query(RetentionPolicy).filter(RetentionPolicy.workspace_id == workspace_id).first()
            if policy is None:
                return
            run = db.query(RetentionRun).filter(RetentionRun.id == run_id, RetentionRun.workspace_id == workspace_id).first() if run_id else None
            if run is None:
                run = RetentionRun(workspace_id=workspace_id, requested_by="scheduler", policy_snapshot=normalized_days(policy.retention_days or {}))
                db.add(run); db.commit(); db.refresh(run)
            if run.status in {"completed", "cancelled"}:
                return
            if policy.legal_hold or not policy.enabled and run.requested_by == "scheduler":
                run.status, run.error, run.finished_at = "cancelled", "legal hold or scheduled retention disabled", datetime.now(timezone.utc)
                db.commit(); schedule_policy(db, policy); return
            run.status, run.started_at = "running", datetime.now(timezone.utc); db.commit()
            try:
                counts = {}
                for category, query in _targets(db, workspace_id, run.policy_snapshot, run.started_at):
                    counts[category] = counts.get(category, 0) + query.delete(synchronize_session=False)
                run.deleted_counts, run.status = counts, "completed"
                run.finished_at = datetime.now(timezone.utc)
                db.commit()
            except Exception as exc:
                db.rollback(); run.status, run.error, run.finished_at = "failed", str(exc)[:1000], datetime.now(timezone.utc); db.commit(); raise
            schedule_policy(db, policy)


def bootstrap_retention_schedules() -> int:
    from apps.api.services.governance.models import RetentionPolicy, RetentionSchedule
    from apps.api.core.tenancy import workspace_scope
    now = datetime.now(timezone.utc)
    with SessionLocal() as db:
        due = [(row.workspace_id, row.next_run_at) for row in db.query(RetentionSchedule).filter(RetentionSchedule.enabled.is_(True)).all()]
    count = 0
    for workspace_id, next_at in due:
        comparable = next_at.replace(tzinfo=timezone.utc) if next_at and next_at.tzinfo is None else next_at
        if comparable and comparable > now: continue
        with workspace_scope(workspace_id):
            with SessionLocal() as db:
                policy = db.query(RetentionPolicy).filter(RetentionPolicy.workspace_id == workspace_id).first()
                if policy and policy.enabled and not policy.legal_hold:
                    from apps.api.services.job_scheduling import enqueue_job_once
                    enqueue_job_once(db, job_type="retention_enforce", payload={"workspace_id": workspace_id}, fire_key=f"retention:{workspace_id}:{now.date().isoformat()}")
                    schedule_policy(db, policy, now=now); count += 1
    return count
