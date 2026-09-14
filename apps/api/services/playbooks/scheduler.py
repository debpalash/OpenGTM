"""Restart-safe recurring audience research playbooks."""

from datetime import datetime, timedelta, timezone

from sqlalchemy import or_

from apps.api.database import SessionLocal


PLAYBOOK_BOOTSTRAP_PAGE_SIZE = 500


def _utc(value: datetime) -> datetime:
    return value if value.tzinfo else value.replace(tzinfo=timezone.utc)


def remove_schedule(db, playbook_id: str) -> None:
    from apps.api.models import Job
    from apps.api.services.playbooks.models import PlaybookSchedule
    db.query(PlaybookSchedule).filter(PlaybookSchedule.playbook_id == playbook_id).delete()
    db.query(Job).filter(Job.type == "research_playbook_schedule", Job.status == "pending", Job.fire_key.like(f"playbook_schedule:{playbook_id}:%")).update({Job.status: "cancelled"}, synchronize_session=False)


def schedule_next(db, playbook, now: datetime | None = None):
    from apps.api.models import Job
    from apps.api.services.job_scheduling import enqueue_job_once
    from apps.api.services.playbooks.models import PlaybookSchedule
    db.query(Job).filter(Job.type == "research_playbook_schedule", Job.status == "pending", Job.fire_key.like(f"playbook_schedule:{playbook.id}:%")).update({Job.status: "cancelled"}, synchronize_session=False)
    active = bool(playbook.enabled and playbook.schedule_audience_id and playbook.schedule_interval_minutes)
    next_at = (now or datetime.now(timezone.utc)) + timedelta(minutes=max(15, min(int(playbook.schedule_interval_minutes or 60), 10080))) if active else None
    mirror = db.query(PlaybookSchedule).filter(PlaybookSchedule.playbook_id == playbook.id).first()
    if mirror is None:
        mirror = PlaybookSchedule(playbook_id=playbook.id, workspace_id=playbook.workspace_id)
        db.add(mirror)
    mirror.workspace_id, mirror.enabled, mirror.next_run_at = playbook.workspace_id, active, next_at
    playbook.next_run_at = next_at
    if next_at:
        enqueue_job_once(db, job_type="research_playbook_schedule", payload={"workspace_id": playbook.workspace_id, "playbook_id": playbook.id}, fire_key=f"playbook_schedule:{playbook.id}:{next_at.isoformat()}", next_run_at=next_at)
    db.commit()
    return next_at


async def handle_playbook_schedule(job_id: int, payload: dict) -> None:
    workspace_id, playbook_id = payload.get("workspace_id"), payload.get("playbook_id")
    if not workspace_id or not playbook_id:
        raise ValueError("playbook schedule requires workspace_id and playbook_id")
    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.playbooks.models import PlaybookRun, ResearchPlaybook
    from apps.api.services.queue_service import queue_service
    with workspace_scope(workspace_id):
        with SessionLocal() as db:
            playbook = db.query(ResearchPlaybook).filter(ResearchPlaybook.id == playbook_id, ResearchPlaybook.workspace_id == workspace_id).first()
            if playbook is None or not playbook.enabled or not playbook.schedule_audience_id:
                remove_schedule(db, playbook_id); db.commit(); return
            active = db.query(PlaybookRun).filter(PlaybookRun.workspace_id == workspace_id, PlaybookRun.playbook_id == playbook_id, PlaybookRun.status.in_(("pending", "running"))).first()
            if active is None:
                run = PlaybookRun(workspace_id=workspace_id, playbook_id=playbook.id, audience_id=playbook.schedule_audience_id, prompt_version=playbook.version, prompt_snapshot=playbook.prompt_template, steps_snapshot=playbook.steps or [], max_members=100, requested_by="scheduler")
                db.add(run); db.commit(); db.refresh(run)
                queue_service.add_job(db, "research_playbook_run", {"workspace_id": workspace_id, "run_id": run.id}, fire_key=f"playbook:{run.id}")
            schedule_next(db, playbook)


def bootstrap_playbook_schedules() -> int:
    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.playbooks.models import PlaybookSchedule, ResearchPlaybook
    now = datetime.now(timezone.utc)
    count = 0
    last_playbook_id = None
    while True:
        with SessionLocal() as db:
            due_query = db.query(
                PlaybookSchedule.playbook_id,
                PlaybookSchedule.workspace_id,
                PlaybookSchedule.next_run_at,
            ).filter(
                PlaybookSchedule.enabled.is_(True),
                or_(
                    PlaybookSchedule.next_run_at.is_(None),
                    PlaybookSchedule.next_run_at <= now,
                ),
            )
            if last_playbook_id is not None:
                due_query = due_query.filter(PlaybookSchedule.playbook_id > last_playbook_id)
            due = due_query.order_by(PlaybookSchedule.playbook_id.asc()).limit(
                PLAYBOOK_BOOTSTRAP_PAGE_SIZE,
            ).all()
        if not due:
            break
        for playbook_id, workspace_id, due_at in due:
            with workspace_scope(workspace_id):
                with SessionLocal() as db:
                    playbook = db.query(ResearchPlaybook).filter(ResearchPlaybook.id == playbook_id, ResearchPlaybook.workspace_id == workspace_id).first()
                    if playbook is None or not playbook.enabled or not playbook.schedule_audience_id:
                        remove_schedule(db, playbook_id); db.commit(); continue
                    from apps.api.models import Job
                    pending = db.query(Job).filter(Job.type == "research_playbook_schedule", Job.status.in_(("pending", "processing")), Job.fire_key.like(f"playbook_schedule:{playbook_id}:%")).first()
                    if pending is None:
                        from apps.api.services.job_scheduling import enqueue_job_once
                        occurrence = _utc(due_at).isoformat() if due_at else "bootstrap"
                        enqueue_job_once(db, job_type="research_playbook_schedule", payload={"workspace_id": workspace_id, "playbook_id": playbook_id}, fire_key=f"playbook_schedule:{playbook_id}:{occurrence}", next_run_at=now)
                        db.commit(); count += 1
        last_playbook_id = due[-1][0]
    return count
