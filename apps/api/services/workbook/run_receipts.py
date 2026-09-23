"""Persist workbook results only for the queue lease that produced them."""
from datetime import datetime, timezone

from apps.api.database import SessionLocal
from apps.api.models import Job


def persist_run_result(job_id: int, payload: dict, result: dict) -> bool:
    lease = payload.get("__queue_lease") or {}
    if not payload.get("workspace_id") or not payload.get("workbook_id") or not lease.get("worker_id") or not lease.get("locked_at"):
        return False  # Direct/non-queue callers do not own a durable job receipt.
    if not isinstance(result, dict) or any(
        type(result.get(key)) is not int or result[key] < 0
        for key in ("completed", "errors", "total", "rows")
    ) or type(result.get("stopped")) is not bool:
        return False
    from apps.api.services.queue_service import _timestamp_matches

    locked_at = datetime.fromisoformat(lease["locked_at"])
    with SessionLocal() as db:
        predicates = (
            Job.id == job_id,
            Job.type == "run_workbook",
            Job.workspace_id == payload.get("workspace_id"),
            Job.worker_id == lease["worker_id"],
            _timestamp_matches(Job.locked_at, locked_at),
            Job.status.in_(["processing", "cancelled"]),
        )
        # Serialize all JSON read/merge/write operations before reading payload.
        # SQLite ignores FOR UPDATE; otherwise an output claim committed between
        # our SELECT and UPDATE could be erased by the stale payload snapshot.
        if db.query(Job).filter(*predicates).update(
            {Job.retry_count: Job.retry_count}, synchronize_session=False
        ) != 1:
            return False
        job = db.query(Job).filter(*predicates).populate_existing().first()
        if not job or (job.payload or {}).get("workbook_id") != payload.get("workbook_id"):
            return False
        receipt = {key: result[key] for key in ("completed", "errors", "total", "rows", "stopped")}
        receipt["recorded_at"] = datetime.now(timezone.utc).isoformat()
        receipt["retry_count"] = job.retry_count or 0
        updated_payload = {**(job.payload or {}), "execution_result": receipt}
        # Repeat the lease predicate: SQLite ignores FOR UPDATE.
        changed = db.query(Job).filter(*predicates).update({Job.payload: updated_payload}, synchronize_session=False)
        db.commit()
        return bool(changed)
