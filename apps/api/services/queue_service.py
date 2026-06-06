import asyncio
import logging
import traceback
from datetime import datetime, timedelta, timezone
from typing import Dict, Any, Optional, Callable, Awaitable

from sqlalchemy import select, update, or_
from sqlalchemy.orm import Session

from apps.api.database import SessionLocal
from apps.api.models import Job

logger = logging.getLogger(__name__)


class QueueService:
    def __init__(self):
        self.is_running = False
        self._shutdown_event = asyncio.Event()
        self.handlers: Dict[str, Callable[[int, Dict], Awaitable[None]]] = {}
        self.heartbeat_interval = 30  # seconds

    def register_handler(
        self, job_type: str, handler: Callable[[int, Dict], Awaitable[None]]
    ):
        self.handlers[job_type] = handler

    def add_job(
        self, db: Session, job_type: str, payload: Dict, priority: int = 1
    ) -> Job:
        job = Job(
            type=job_type,
            payload=payload,
            priority=priority,
            status="pending",
            created_at=datetime.now(timezone.utc),
            next_run_at=datetime.now(timezone.utc),
            max_retries=3,
        )
        db.add(job)
        db.commit()
        db.refresh(job)
        logger.info(f"Job {job.id} ({job_type}) added to queue.")
        return job

    def recover_jobs(self):
        """Reset stuck processing jobs to pending on startup."""
        try:
            with SessionLocal() as db:
                # Find all jobs that are 'processing' older than X minutes?
                # For now, on startup, any processing job is considered crashed.
                stuck_jobs = db.query(Job).filter(Job.status == "processing").all()
                if stuck_jobs:
                    logger.info(f"Recovering {len(stuck_jobs)} stuck jobs...")
                    for job in stuck_jobs:
                        job.status = "pending"
                        job.started_at = None
                        job.error = "Recovered from crash"
                        # Increment retry count to avoid infinite crash loops
                        job.retry_count = (job.retry_count or 0) + 1
                        job.next_run_at = datetime.now(timezone.utc) + timedelta(minutes=1)
                    db.commit()
        except Exception as e:
            logger.error(f"Failed to recover jobs: {e}")

    async def start_worker(self):
        if self.is_running:
            return
        self.is_running = True
        self._shutdown_event.clear()

        # Recover jobs before starting loop
        self.recover_jobs()

        logger.info("Starting Queue Worker...")
        asyncio.create_task(self._worker_loop())
        asyncio.create_task(self._monitor_heartbeats())

    async def stop_worker(self):
        self.is_running = False
        self._shutdown_event.set()
        logger.info("Stopping Queue Worker...")

    async def _worker_loop(self):
        logger.info("Queue Worker Loop Started")
        while self.is_running:
            try:
                # Poll for job
                job_id = None
                job_type = None
                job_payload = None

                with SessionLocal() as db:
                    # Find highest priority pending job that is ready to run
                    job = (
                        db.query(Job)
                        .filter(
                            Job.status == "pending",
                            or_(
                                Job.next_run_at <= datetime.now(timezone.utc),
                                Job.next_run_at == None,
                            ),
                        )
                        .order_by(Job.priority.desc(), Job.next_run_at.asc())
                        .first()
                    )

                    if job:
                        # Mark as processing
                        job.status = "processing"
                        job.started_at = datetime.now(timezone.utc)
                        job.last_heartbeat = datetime.now(timezone.utc)
                        db.commit()

                        job_id = job.id
                        job_type = job.type
                        job_payload = job.payload

                if job_id:
                    await self._process_job(job_id, job_type, job_payload)
                else:
                    await asyncio.sleep(1)  # Wait if empty

            except Exception as e:
                logger.error(f"Worker loop error: {e}")
                await asyncio.sleep(5)

    async def _process_job(self, job_id: int, job_type: str, payload: Dict):
        logger.info(f"Processing Job {job_id} ({job_type})")
        handler = self.handlers.get(job_type)

        error = None
        status = "completed"

        # Start Heartbeat Task for this job
        heartbeat_task = asyncio.create_task(self._job_heartbeat(job_id))

        try:
            if handler:
                # Isolate handler execution in its own thread + event loop.
                # Handlers do heavy provider/LLM I/O and CPU-bound parsing; running
                # them on the API's main event loop froze EVERY request (even non-DB
                # routes like /docs). Each handler is self-contained — it creates its
                # own DB sessions (check_same_thread=False) and clients from the
                # payload — so running it off-loop in a worker thread is safe.
                await asyncio.to_thread(lambda: asyncio.run(handler(job_id, payload)))
            else:
                raise Exception(f"No handler for job type {job_type}")

        except Exception as e:
            logger.error(f"Job {job_id} failed: {e}")
            error = str(e)
            traceback.print_exc()
            status = "failed"

            # Retry Logic happens in DB update below
        finally:
            heartbeat_task.cancel()

        # Update DB
        try:
            with SessionLocal() as db:
                job = db.query(Job).filter(Job.id == job_id).first()
                if job:
                    if status == "failed":
                        # Check Retry
                        if (job.retry_count or 0) < (job.max_retries or 3):
                            job.status = "pending"
                            job.retry_count = (job.retry_count or 0) + 1
                            # Exponential Backoff: 1min, 2min, 4min...
                            backoff_minutes = 2 ** (job.retry_count - 1)
                            job.next_run_at = datetime.now(timezone.utc) + timedelta(
                                minutes=backoff_minutes
                            )
                            job.error = f"Retry {job.retry_count}: {error}"
                            logger.info(
                                f"Scheduled retry for Job {job_id} in {backoff_minutes} mins"
                            )
                        else:
                            job.status = "failed"
                            job.completed_at = datetime.now(timezone.utc)
                            job.error = f"Final Failure: {error}"
                    else:
                        job.status = "completed"
                        job.completed_at = datetime.now(timezone.utc)
                        job.error = None

                    db.commit()
        except Exception as e:
            logger.error(f"Failed to update job status: {e}")

    async def _job_heartbeat(self, job_id: int):
        while True:
            try:
                await asyncio.sleep(self.heartbeat_interval)
                with SessionLocal() as db:
                    job = db.query(Job).filter(Job.id == job_id).first()
                    if job and job.status == "processing":
                        job.last_heartbeat = datetime.now(timezone.utc)
                        db.commit()
            except asyncio.CancelledError:
                break
            except Exception as e:
                logger.warn(f"Heartbeat failed for job {job_id}: {e}")

    async def _monitor_heartbeats(self):
        """Monitor for dead jobs (processing but no heartbeat)."""
        while self.is_running:
            try:
                await asyncio.sleep(60)  # Check every minute
                with SessionLocal() as db:
                    # Find jobs processing > 5 mins ago with no heartbeat update
                    # Assuming heartbeat is every 30s. Allow 5 mins grace.
                    threshold = datetime.now(timezone.utc) - timedelta(minutes=5)
                    dead_jobs = (
                        db.query(Job)
                        .filter(
                            Job.status == "processing",
                            or_(
                                Job.last_heartbeat < threshold,
                                Job.last_heartbeat == None,
                            ),
                        )
                        .all()
                    )

                    for job in dead_jobs:
                        # Mark as crashed/retry
                        logger.warn(
                            f"Job {job.id} detected dead (heartbeat timeout). Recovering..."
                        )
                        job.status = (
                            "pending"  # Will trigger retry count check on next pickup?
                        )
                        # Actually we should increment retry here to avoid loops
                        job.retry_count = (job.retry_count or 0) + 1
                        job.error = "Heartbeat Timeout"
                        job.next_run_at = datetime.now(timezone.utc) + timedelta(minutes=1)

                    if dead_jobs:
                        db.commit()

            except Exception as e:
                logger.error(f"Monitor loop error: {e}")


# Global Instance
queue_service = QueueService()
