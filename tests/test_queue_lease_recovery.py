"""Expired attempts are bounded and cannot mutate replacement claims."""

import asyncio
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timedelta, timezone
import threading

import pytest

from apps.api.database import SessionLocal
from apps.api.models import Job
from apps.api.services.queue_service import QueueService
from sqlalchemy import text


@pytest.fixture(autouse=True)
def clean_jobs():
    with SessionLocal() as db:
        db.query(Job).delete()
        db.commit()
    yield
    with SessionLocal() as db:
        db.query(Job).delete()
        db.commit()


@pytest.mark.parametrize("startup", [False, True])
@pytest.mark.parametrize("retries,limit,expected", [(0, 0, "failed"), (2, 2, "failed"), (0, 2, "pending")])
def test_recovery_honors_retry_limit_and_reconciles(startup, retries, limit, expected):
    queue = QueueService()
    observed = []
    queue.register_failure_handler("test", lambda *args: observed.append(args))
    with SessionLocal() as db:
        job = Job(type="test", payload={}, status="processing", retry_count=retries,
                  max_retries=limit, worker_id=queue.worker_id,
                  last_heartbeat=datetime.now(timezone.utc) - timedelta(minutes=6))
        db.add(job)
        db.commit()
        job_id = job.id
    queue._recover_attempts(startup=startup)
    queue._recover_attempts(startup=startup)
    with SessionLocal() as db:
        job = db.get(Job, job_id)
        assert job.status == expected
        assert job.retry_count == retries + (expected == "pending")
        assert (job.completed_at is not None) == (expected == "failed")
        assert job.worker_id is None
    assert len(observed) == 1
    assert observed[0][-1] == (expected == "pending")


@pytest.mark.parametrize("same_worker", [False, True])
def test_stale_attempt_cannot_renew_or_finalize_replacement(monkeypatch, same_worker):
    queue = QueueService()
    with SessionLocal() as db:
        job_id = queue.add_job(db, "test", {}).id
    claimed = queue.claim_next_job()
    queue.register_handler("test", lambda *_: None)
    replacement_time = claimed["locked_at"] + timedelta(seconds=1)
    replacement_owner = queue.worker_id if same_worker else "replacement-worker"

    async def reclaimed_during_process(*args, **kwargs):
        with SessionLocal() as db:
            job = db.get(Job, job_id)
            job.worker_id = replacement_owner
            job.locked_at = replacement_time
            job.last_heartbeat = replacement_time
            db.commit()
        queue._renew_claim(job_id, claimed["locked_at"])
        assert not kwargs["should_continue"]()

    monkeypatch.setattr("apps.api.services.job_process_runner.run_job_subprocess", reclaimed_during_process)
    asyncio.run(queue._process_job(job_id, "test", {}, locked_at=claimed["locked_at"]))
    with SessionLocal() as db:
        job = db.get(Job, job_id)
        assert job.status == "processing"
        assert job.worker_id == replacement_owner
        assert job.locked_at == replacement_time
        assert job.last_heartbeat == replacement_time


def test_active_other_worker_is_not_recovered_on_startup():
    queue = QueueService()
    with SessionLocal() as db:
        job = Job(type="test", status="processing", worker_id="other",
                  last_heartbeat=datetime.now(timezone.utc))
        db.add(job)
        db.commit()
        job_id = job.id
    queue.recover_jobs()
    queue._recover_attempts()
    with SessionLocal() as db:
        assert db.get(Job, job_id).status == "processing"


def test_concurrent_reapers_count_one_retry():
    with SessionLocal() as db:
        job = Job(type="test", payload={}, status="processing", worker_id="dead",
                  last_heartbeat=datetime.now(timezone.utc) - timedelta(minutes=6))
        db.add(job)
        db.commit()
        job_id = job.id
    barrier = threading.Barrier(4)
    def reap():
        barrier.wait()
        QueueService()._recover_attempts()
    with ThreadPoolExecutor(max_workers=4) as pool:
        list(pool.map(lambda _: reap(), range(4)))
    with SessionLocal() as db:
        job = db.get(Job, job_id)
        assert job.status == "pending"
        assert job.retry_count == 1


def test_recovery_accepts_legacy_sqlite_offset_timestamp():
    old = (datetime.now(timezone.utc) - timedelta(minutes=6)).isoformat(" ")
    with SessionLocal() as db:
        job = Job(type="test", status="processing", worker_id="dead")
        db.add(job)
        db.commit()
        job_id = job.id
        db.execute(text("UPDATE jobs SET locked_at=:old, last_heartbeat=:old WHERE id=:id"),
                   {"old": old, "id": job_id})
        db.commit()
    QueueService()._recover_attempts()
    with SessionLocal() as db:
        assert db.get(Job, job_id).status == "pending"
