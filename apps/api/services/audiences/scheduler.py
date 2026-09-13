"""Restart-safe, single-flight scheduling for dynamic audience refreshes."""

from datetime import datetime, timedelta, timezone
import logging

from apps.api.database import SessionLocal
from apps.api.services.audiences.models import Audience, AudienceSchedule
from apps.api.models import Job

logger = logging.getLogger("audiences.scheduler")


def _utcnow() -> datetime:
    return datetime.now(timezone.utc)


def _mirror(db, audience: Audience, next_at: datetime | None) -> None:
    row = db.query(AudienceSchedule).filter(AudienceSchedule.audience_id == audience.id).first()
    if row is None:
        row = AudienceSchedule(audience_id=audience.id, workspace_id=audience.workspace_id)
        db.add(row)
    row.workspace_id = audience.workspace_id
    row.next_refresh_at = next_at
    row.enabled = bool(audience.refresh_enabled)


def remove_schedule(db, audience_id: str) -> None:
    db.query(AudienceSchedule).filter(AudienceSchedule.audience_id == audience_id).delete()
    _cancel_pending(db, audience_id)


def _cancel_pending(db, audience_id: str) -> int:
    return db.query(Job).filter(
        Job.type == "audience_refresh",
        Job.fire_key.like(f"audience_refresh:{audience_id}:%"),
        Job.status == "pending",
    ).update({Job.status: "cancelled"}, synchronize_session=False)


def schedule_next(db, audience: Audience, *, now: datetime | None = None) -> datetime | None:
    """Persist the next occurrence and enqueue it once across scheduler replicas."""
    _cancel_pending(db, audience.id)
    if not audience.refresh_enabled:
        audience.next_refresh_at = None
        _mirror(db, audience, None)
        db.commit()
        return None
    from apps.api.services.job_scheduling import enqueue_job_once

    now = now or _utcnow()
    minutes = max(15, min(int(audience.refresh_interval_minutes or 60), 10080))
    next_at = now + timedelta(minutes=minutes)
    fire_key = f"audience_refresh:{audience.id}:{next_at.isoformat()}"
    enqueue_job_once(
        db, job_type="audience_refresh",
        payload={"workspace_id": audience.workspace_id, "audience_id": audience.id},
        fire_key=fire_key, next_run_at=next_at,
    )
    audience.next_refresh_at = next_at
    _mirror(db, audience, next_at)
    db.commit()
    return next_at


async def handle_audience_refresh(job_id: int, payload: dict) -> None:
    workspace_id = payload.get("workspace_id")
    audience_id = payload.get("audience_id")
    if not workspace_id or not audience_id:
        logger.warning("audience_refresh missing workspace_id/audience_id: %s", payload)
        return
    from apps.api.core.tenancy import WorkspaceCtx, workspace_scope
    from apps.api.services.audiences.refresh import refresh_audience
    from apps.api.services.workspace import manager as workspace_manager

    with workspace_scope(workspace_id):
        with SessionLocal() as db:
            audience = db.query(Audience).filter(
                Audience.id == audience_id, Audience.workspace_id == workspace_id,
            ).first()
            if audience is None or not audience.refresh_enabled:
                remove_schedule(db, audience_id)
                db.commit()
                return
            slug = workspace_manager.workspace_slug(workspace_id)
            if not slug:
                logger.warning("audience_refresh workspace missing: %s", workspace_id)
                return
            ctx = WorkspaceCtx(user=None, workspace_id=workspace_id, slug=slug)  # type: ignore[arg-type]
            refresh_audience(db, ctx, audience)
            schedule_next(db, audience)


def bootstrap_audience_schedules() -> int:
    """Reconcile due schedules from the non-RLS mirror after restarts."""
    now = _utcnow()
    with SessionLocal() as db:
        due = db.query(AudienceSchedule).filter(AudienceSchedule.enabled.is_(True)).all()
        identities = [
            (row.audience_id, row.workspace_id) for row in due
            if row.next_refresh_at is None or _as_utc(row.next_refresh_at) <= now
        ]
    enqueued = 0
    from apps.api.core.tenancy import workspace_scope
    for audience_id, workspace_id in identities:
        with workspace_scope(workspace_id):
            with SessionLocal() as db:
                audience = db.query(Audience).filter(
                    Audience.id == audience_id, Audience.workspace_id == workspace_id,
                ).first()
                if audience is None or not audience.refresh_enabled:
                    remove_schedule(db, audience_id)
                    db.commit()
                    continue
                schedule_next(db, audience, now=now)
                enqueued += 1
    logger.info("bootstrap_audience_schedules enqueued %d refresh(es)", enqueued)
    return enqueued


def _as_utc(value: datetime) -> datetime:
    return value if value.tzinfo else value.replace(tzinfo=timezone.utc)
