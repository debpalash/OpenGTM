"""Concurrency-safety tests for QueueService.claim_next_job().

These run OFFLINE on SQLite (the conftest default DATABASE_URL). The Postgres
FOR UPDATE SKIP LOCKED path cannot be exercised here (no live Postgres), so we
verify the dialect detection picks the SQLite path and that the SQLite path
itself is double-grab-safe under real thread contention.

Core property under test: enqueue N jobs, run several claimers concurrently, and
assert each job is claimed by EXACTLY ONE claimer — never twice. That is the
horizontal-scaling guarantee (no double-grab → no double-charge).
"""
import threading

import pytest

from apps.api.database import SessionLocal, engine
from apps.api.db_init import init_db
from apps.api.models import Job
from apps.api.services.queue_service import QueueService, queue_service


@pytest.fixture(autouse=True)
def _schema_and_clean_jobs():
    """Ensure the schema exists and start each test with an empty jobs table."""
    init_db()
    with SessionLocal() as db:
        db.query(Job).delete()
        db.commit()
    yield
    with SessionLocal() as db:
        db.query(Job).delete()
        db.commit()


def _enqueue(n: int) -> None:
    with SessionLocal() as db:
        for i in range(n):
            queue_service.add_job(db, "download_link", {"i": i})


def test_dialect_detection_uses_sqlite_path():
    """Tests/dev run on SQLite — confirm the engine reports sqlite so the
    guarded-UPDATE claim path (not the Postgres SKIP LOCKED path) is selected."""
    assert engine.dialect.name == "sqlite"


def test_claim_returns_none_when_empty():
    assert queue_service.claim_next_job() is None


def test_claimed_job_transitions_to_running():
    _enqueue(1)
    claimed = queue_service.claim_next_job()
    assert claimed is not None
    with SessionLocal() as db:
        job = db.query(Job).filter(Job.id == claimed["id"]).first()
        # Existing vocabulary: 'pending' -> 'processing' (the queue's "running").
        assert job.status == "processing"
        assert job.started_at is not None
        assert job.worker_id is not None  # ownership stamped
        assert job.locked_at is not None  # lock timestamp stamped


def test_concurrent_claimers_never_double_grab():
    """The headline test: many threads, each with its OWN QueueService identity
    (simulating separate worker replicas), race to drain the queue. Every job
    must be claimed exactly once; no id may appear twice across all claimers."""
    n_jobs = 60
    n_workers = 8
    _enqueue(n_jobs)

    # Each thread gets a distinct QueueService → distinct worker_id, mimicking
    # independent replicas all pointed at the same DB.
    services = [QueueService() for _ in range(n_workers)]
    claimed_ids: list[int] = []
    lock = threading.Lock()
    start = threading.Event()

    def drain(svc: QueueService):
        start.wait()
        while True:
            job = svc.claim_next_job()
            if job is None:
                # Could be transiently empty mid-race; only stop once truly drained.
                with SessionLocal() as db:
                    remaining = (
                        db.query(Job).filter(Job.status == "pending").count()
                    )
                if remaining == 0:
                    return
                continue
            with lock:
                claimed_ids.append(job["id"])

    threads = [threading.Thread(target=drain, args=(s,)) for s in services]
    for t in threads:
        t.start()
    start.set()
    for t in threads:
        t.join(timeout=60)

    # 1. No job claimed more than once (the double-grab guarantee).
    assert len(claimed_ids) == len(set(claimed_ids)), (
        f"double-grab detected: {len(claimed_ids)} claims but only "
        f"{len(set(claimed_ids))} unique ids"
    )
    # 2. Every enqueued job was claimed exactly once.
    assert len(claimed_ids) == n_jobs
    # 3. DB agrees: every job is now 'processing', none left 'pending'.
    with SessionLocal() as db:
        assert db.query(Job).filter(Job.status == "pending").count() == 0
        assert db.query(Job).filter(Job.status == "processing").count() == n_jobs
        # Every processing job carries an owner.
        unowned = (
            db.query(Job)
            .filter(Job.status == "processing", Job.worker_id == None)  # noqa: E711
            .count()
        )
        assert unowned == 0


def test_each_job_claimed_by_single_worker_id():
    """Stronger ownership check: across concurrent claimers, no two workers may
    report owning the same job id."""
    n_jobs = 40
    n_workers = 6
    _enqueue(n_jobs)

    services = [QueueService() for _ in range(n_workers)]
    # Map job_id -> set of worker_ids that claimed it (should be size 1 each).
    owners: dict[int, set] = {}
    lock = threading.Lock()
    start = threading.Event()

    def drain(svc: QueueService):
        start.wait()
        while True:
            job = svc.claim_next_job()
            if job is None:
                with SessionLocal() as db:
                    if db.query(Job).filter(Job.status == "pending").count() == 0:
                        return
                continue
            with lock:
                owners.setdefault(job["id"], set()).add(svc.worker_id)

    threads = [threading.Thread(target=drain, args=(s,)) for s in services]
    for t in threads:
        t.start()
    start.set()
    for t in threads:
        t.join(timeout=60)

    assert len(owners) == n_jobs
    multi = {jid: ws for jid, ws in owners.items() if len(ws) != 1}
    assert not multi, f"jobs claimed by multiple workers: {multi}"
