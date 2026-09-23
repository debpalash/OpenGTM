import asyncio
import threading
from collections import deque
from datetime import datetime, timedelta, timezone

from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker

from apps.api.database import Base
from apps.api.models import Job
from apps.api.services.queue_service import QueueService


def test_worker_runs_bounded_parallel_slots_and_drains(monkeypatch):
    async def scenario():
        queue = QueueService(concurrency=3, shutdown_grace=2)
        monkeypatch.setattr(queue, "recover_jobs", lambda: None)
        lease_time = datetime.now(timezone.utc)
        claims = deque({"id": index, "type": "test", "payload": {}, "locked_at": lease_time} for index in range(6))
        lock = threading.Lock(); active = 0; maximum = 0; finished = 0; done = asyncio.Event()

        def claim():
            with lock: return claims.popleft() if claims else None

        async def process(job_id, job_type, payload, *, locked_at):
            nonlocal active, maximum, finished
            assert locked_at == lease_time
            active += 1; maximum = max(maximum, active)
            await asyncio.sleep(0.03)
            active -= 1; finished += 1
            if finished == 6: done.set()

        monkeypatch.setattr(queue, "claim_next_job", claim)
        monkeypatch.setattr(queue, "_process_job", process)
        monkeypatch.setattr(queue, "_monitor_heartbeats", lambda: asyncio.sleep(3600))
        await queue.start_worker()
        await asyncio.wait_for(done.wait(), timeout=2)
        await queue.stop_worker()
        assert maximum == 3
        assert queue._worker_tasks == [] and queue._monitor_task is None and not queue._active_slots

    asyncio.run(scenario())


def test_worker_cancels_after_shutdown_grace(monkeypatch):
    async def scenario():
        queue = QueueService(concurrency=1, shutdown_grace=0)
        monkeypatch.setattr(queue, "recover_jobs", lambda: None)
        claimed = False; started = asyncio.Event(); cancelled = asyncio.Event()
        lease_time = datetime.now(timezone.utc)

        def claim():
            nonlocal claimed
            if claimed: return None
            claimed = True; return {"id": 1, "type": "test", "payload": {}, "locked_at": lease_time}

        async def process(*_, locked_at):
            assert locked_at == lease_time
            started.set()
            try: await asyncio.sleep(3600)
            except asyncio.CancelledError: cancelled.set(); raise

        monkeypatch.setattr(queue, "claim_next_job", claim)
        monkeypatch.setattr(queue, "_process_job", process)
        monkeypatch.setattr(queue, "_monitor_heartbeats", lambda: asyncio.sleep(3600))
        await queue.start_worker(); await asyncio.wait_for(started.wait(), timeout=1); await queue.stop_worker()
        assert cancelled.is_set() and not queue._active_slots

    asyncio.run(scenario())


def test_queue_metrics_aggregate_without_payloads():
    engine = create_engine("sqlite:///:memory:")
    Base.metadata.create_all(engine, tables=[Job.__table__])
    db = sessionmaker(bind=engine)(); now = datetime.now(timezone.utc)
    db.add_all([
        Job(type="run_workbook", payload={"secret": "hidden"}, status="pending", created_at=now - timedelta(seconds=12), next_run_at=now),
        Job(type="run_workbook", payload={}, status="processing", worker_id="worker-a", created_at=now, next_run_at=now),
        Job(type="watch_poll", payload={}, status="failed", created_at=now, next_run_at=now),
    ]); db.commit()
    metrics = QueueService(concurrency=4).metrics(db)
    assert metrics["configured_concurrency"] == 4
    assert metrics["counts"] == {"failed": 1, "pending": 1, "processing": 1}
    assert metrics["active_by_type"] == {"run_workbook": 2}
    assert metrics["active_workers"] == 1 and metrics["oldest_pending_age_seconds"] >= 10
    assert "secret" not in str(metrics)
    db.close()
