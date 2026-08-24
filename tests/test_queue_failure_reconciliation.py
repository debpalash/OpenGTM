"""Parent-worker failure callbacks reconcile domain state after killed children."""

import asyncio

from apps.api.database import SessionLocal
from apps.api.db_init import init_db
from apps.api.models import Job
from apps.api.services.queue_service import QueueService


def test_failure_hook_observes_retry_then_terminal_failure(monkeypatch):
    import apps.api.services.job_process_runner as process_runner

    init_db()
    with SessionLocal() as db:
        db.query(Job).delete()
        job = Job(
            type="test_failure_hook", payload={"run_id": "run-1"},
            status="processing", retry_count=0, max_retries=1,
        )
        db.add(job)
        db.commit()
        db.refresh(job)
        job_id = job.id

    async def fail_process(*args, **kwargs):
        raise process_runner.JobProcessTimeout("forced timeout")

    monkeypatch.setattr(process_runner, "run_job_subprocess", fail_process)
    observed = []
    queue = QueueService()

    async def registered_handler(job_id, payload):  # pragma: no cover
        raise AssertionError("handlers run in the patched subprocess boundary")

    queue.register_handler("test_failure_hook", registered_handler)
    queue.register_failure_handler(
        "test_failure_hook",
        lambda jid, payload, error, will_retry: observed.append(
            (jid, payload["run_id"], will_retry, error)
        ),
    )

    asyncio.run(queue._process_job(job_id, "test_failure_hook", {"run_id": "run-1"}))
    with SessionLocal() as db:
        job = db.get(Job, job_id)
        assert job.status == "pending"
        assert job.retry_count == 1
    assert observed[-1][:3] == (job_id, "run-1", True)

    asyncio.run(queue._process_job(job_id, "test_failure_hook", {"run_id": "run-1"}))
    with SessionLocal() as db:
        job = db.get(Job, job_id)
        assert job.status == "failed"
        assert job.completed_at is not None
    assert observed[-1][:3] == (job_id, "run-1", False)
    assert "forced timeout" in observed[-1][3]


def test_parent_finalizer_preserves_external_cancellation(monkeypatch):
    """A cancellation committed while the child runs cannot become completed."""
    import apps.api.services.job_process_runner as process_runner

    init_db()
    with SessionLocal() as db:
        job = Job(
            type="test_cancel_race",
            payload={},
            status="processing",
            retry_count=0,
            max_retries=1,
        )
        db.add(job)
        db.commit()
        db.refresh(job)
        job_id = job.id

    async def cancel_during_process(*args, **kwargs):
        with SessionLocal() as db:
            current = db.get(Job, job_id)
            current.status = "cancelled"
            db.commit()

    monkeypatch.setattr(process_runner, "run_job_subprocess", cancel_during_process)
    queue = QueueService()
    queue.register_handler("test_cancel_race", lambda job_id, payload: None)

    asyncio.run(queue._process_job(job_id, "test_cancel_race", {}))
    with SessionLocal() as db:
        job = db.get(Job, job_id)
        assert job.status == "cancelled"
        assert job.completed_at is not None
