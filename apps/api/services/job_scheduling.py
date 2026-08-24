"""Database-enforced single-flight helpers for scheduled durable jobs."""

from __future__ import annotations

from datetime import datetime
from typing import Any, Optional

from sqlalchemy import text
from sqlalchemy.exc import IntegrityError
from sqlalchemy.orm import Session

from apps.api.models import Job


ACTIVE_JOB_STATUSES = ("pending", "processing")


def enqueue_job_once(
    db: Session,
    *,
    job_type: str,
    payload: dict[str, Any],
    fire_key: str,
    next_run_at: datetime,
    priority: int = 1,
    max_retries: int = 3,
) -> Optional[Job]:
    """Insert one active occurrence for ``fire_key`` or return ``None``.

    Postgres takes a transaction-scoped advisory lock and is additionally
    protected by ``uq_jobs_fire_key_active``. SQLite uses the same read guard,
    which is sufficient for the single-process dev/test scheduler.
    """
    if db.get_bind().dialect.name == "postgresql":
        db.execute(
            text("SELECT pg_advisory_xact_lock(hashtext(:key))"),
            {"key": fire_key},
        )

    existing = (
        db.query(Job)
        .filter(
            Job.fire_key == fire_key,
            Job.status.in_(ACTIVE_JOB_STATUSES),
        )
        .first()
    )
    if existing is not None:
        return None

    job = Job(
        type=job_type,
        payload={**payload, "fire_key": fire_key},
        fire_key=fire_key,
        status="pending",
        priority=priority,
        next_run_at=next_run_at,
        max_retries=max_retries,
    )
    db.add(job)
    try:
        db.flush()
    except IntegrityError:
        # Another Postgres transaction won the partial unique-index race.
        db.rollback()
        return None
    return job
