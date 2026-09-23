"""Job claiming on real PostgreSQL (PG-gated).

Regression: the claim predicate used a NULL-only ``:fire_key_prefix`` parameter,
which PostgreSQL rejects ("could not determine data type of parameter"), so a
standalone worker without a prefix filter could never claim any job. SQLite
accepted the query, so only a PostgreSQL run exposes it.
"""

import os
import threading
import uuid
from datetime import datetime, timezone

import pytest
from sqlalchemy import create_engine, text
from sqlalchemy.orm import sessionmaker

TEST_DATABASE_URL = os.getenv("TEST_DATABASE_URL")

pytestmark = pytest.mark.skipif(
    not TEST_DATABASE_URL,
    reason="TEST_DATABASE_URL not set (Postgres queue claim tests skipped)",
)


@pytest.fixture(scope="module")
def Session():
    # Migrates to head; the schema owner models the production runtime role,
    # which owns the non-RLS jobs queue.
    from tests.pg_rls_support import rls_app_session
    _, dispose = rls_app_session(TEST_DATABASE_URL, pool_size=2)
    dispose()
    engine = create_engine(TEST_DATABASE_URL, pool_size=10, max_overflow=0)
    yield sessionmaker(bind=engine, autoflush=False)
    engine.dispose()


def _add_jobs(Session, tag, count, fire_key=None):
    from apps.api.models import Job
    with Session() as db:
        db.execute(text("UPDATE jobs SET status = 'completed' WHERE status = 'pending'"))
        now = datetime.now(timezone.utc)
        jobs = [Job(type=f"test_{tag}", payload={}, status="pending", priority=1,
                    created_at=now, next_run_at=now, fire_key=fire_key, max_retries=3)
                for _ in range(count)]
        db.add_all(jobs)
        db.commit()
        return {job.id for job in jobs}


def test_claim_without_prefix_filter_succeeds(Session):
    from apps.api.services.queue_service import QueueService
    ids = _add_jobs(Session, uuid.uuid4().hex[:6], 1)
    with Session() as db:
        claimed = QueueService()._claim_next_job_postgres(db, None)
    assert claimed is not None and claimed["id"] in ids


def test_claim_with_prefix_filter_only_matches_prefix(Session):
    from apps.api.services.queue_service import QueueService
    tag = uuid.uuid4().hex[:6]
    _add_jobs(Session, tag, 1, fire_key=f"other:{tag}")
    wanted = _add_jobs(Session, tag, 1, fire_key=f"wanted:{tag}")  # also completes the "other" job
    with Session() as db:
        db.execute(text("UPDATE jobs SET status = 'pending' WHERE fire_key = :k"), {"k": f"other:{tag}"})
        db.commit()
        assert QueueService()._claim_next_job_postgres(db, "nomatch:") is None
        claimed = QueueService()._claim_next_job_postgres(db, "wanted:")
    assert claimed is not None and claimed["id"] in wanted


def test_racing_workers_claim_each_job_exactly_once(Session):
    from apps.api.services.queue_service import QueueService
    ids = _add_jobs(Session, uuid.uuid4().hex[:6], 5)
    claims, errors = [], []
    barrier = threading.Barrier(8)

    def worker():
        try:
            barrier.wait(timeout=10)
            queue = QueueService()
            while True:
                with Session() as db:
                    job = queue._claim_next_job_postgres(db, None)
                if job is None:
                    return
                claims.append(job["id"])
        except Exception as exc:
            errors.append(repr(exc))

    threads = [threading.Thread(target=worker) for _ in range(8)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    assert not errors, errors
    assert sorted(claims) == sorted(ids)
