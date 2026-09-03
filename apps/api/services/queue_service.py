import asyncio
from contextlib import suppress
import logging
import os
import socket
import uuid
from datetime import datetime, timedelta, timezone
from typing import Dict, Any, Optional, Callable, Awaitable

from sqlalchemy import select, update, or_, text
from sqlalchemy.orm import Session

from apps.api.database import SessionLocal, engine
from apps.api.models import Job

logger = logging.getLogger(__name__)


def _make_worker_id() -> str:
    """Stable-per-process identity for a claiming worker.

    Combines hostname + PID + a short random suffix so two replicas (even on the
    same host, even after a PID is reused) never collide. Stamped onto a job when
    it is claimed so we can attribute work and reap a dead worker's jobs.
    """
    return f"{socket.gethostname()}:{os.getpid()}:{uuid.uuid4().hex[:8]}"

# Per-job-type wall-clock ceiling. The worker runs jobs sequentially, so a
# handler that hangs would block the queue; on timeout we fail the job and move
# on. run_workbook can be a large LLM run, so it gets the most headroom.
DEFAULT_JOB_TIMEOUT = 600  # seconds
JOB_TIMEOUTS = {
    "run_workbook": 1800,
    # source_workbook runs a full multi-strategy leadgen collection INLINE
    # (source_engine.materialize_source → JobRunner.submit → _process_job), which
    # routinely exceeds 900s for real queries; give it the same headroom as
    # run_workbook so it isn't killed mid-collection and retried forever.
    "source_workbook": 1800,
    # Checkpointed page-by-page; enough for a 500-row import plus API retries.
    "ambitionbox_import": 900,
    # Checkpointed streaming import; a full CNPJ dataset can take several hours.
    "data_collector_import": 21600,
    "refresh_workbook": 900,
    "signal_scan": 300,
    # Automations: one rule evaluation over a bounded row set; re_enrich runs
    # single-row with a bounded provider timeout, so 600s is ample headroom.
    "trigger_eval": 600,
    # Outreach: one email send (SMTP handoff). Bounded I/O.
    "send": 300,
    # Intent poller: one watch poll (SEC/JobSpy/RSS fan-in, bounded fetches).
    "watch_poll": 600,
    # Source health: ~91 sources x N canary DDG probes, batched with sleeps.
    "source_health_check": 1800,
}


class QueueService:
    def __init__(self):
        self.is_running = False
        self._shutdown_event = asyncio.Event()
        self.handlers: Dict[str, Callable[[int, Dict], Awaitable[None]]] = {}
        self.failure_handlers: Dict[
            str, Callable[[int, Dict, str, bool], None]
        ] = {}
        self.heartbeat_interval = 30  # seconds
        # Identity used to stamp claimed jobs. Each process (in-API or a
        # standalone worker replica) gets its own.
        self.worker_id = _make_worker_id()

    def register_handler(
        self, job_type: str, handler: Callable[[int, Dict], Awaitable[None]]
    ):
        self.handlers[job_type] = handler

    def register_failure_handler(
        self,
        job_type: str,
        handler: Callable[[int, Dict, str, bool], None],
    ) -> None:
        """Register durable-domain reconciliation after a failed attempt.

        This runs in the parent worker after it has decided whether the SQL job
        will retry. It therefore also covers hard timeouts and killed children,
        where code inside the job process cannot update its domain status.
        """
        self.failure_handlers[job_type] = handler

    def add_job(
        self,
        db: Session,
        job_type: str,
        payload: Dict,
        priority: int = 1,
        fire_key: Optional[str] = None,
    ) -> Job:
        job = Job(
            type=job_type,
            payload=payload,
            priority=priority,
            fire_key=fire_key,
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

    def claim_next_job(self, db: Optional[Session] = None) -> Optional[Dict[str, Any]]:
        """Atomically claim the next eligible job for THIS worker.

        Returns a small dict ``{"id", "type", "payload"}`` for the claimed job,
        or ``None`` when nothing is eligible. The claim is concurrency-safe: run
        across N replicas, every queued job is handed to exactly one worker — no
        double-grab, no double-charge.

        Eligibility matches the historical worker loop: ``status == 'pending'``
        and ``next_run_at`` is due (or NULL). Ordering preserves the previous
        behaviour (highest priority first, then oldest ``next_run_at``).

        Two dialect-aware implementations select the same row; only the locking
        primitive differs:

          * **Postgres** — ``SELECT ... FOR UPDATE SKIP LOCKED LIMIT 1`` inside a
            transaction. The row lock makes concurrent claimers skip a row another
            transaction has already locked, so no two workers see the same row.
            We flip it to ``processing`` + stamp ownership in the SAME tx and
            commit, releasing the lock.
          * **SQLite** — SQLite has no ``SKIP LOCKED`` and only one writer at a
            time, so we rely on a conditional, guarded ``UPDATE``: flip a single
            ``pending`` row to ``processing`` and trust ``rowcount`` to tell us
            whether WE won the claim. If another writer already flipped that row,
            our ``WHERE status='pending'`` matches zero rows (rowcount 0) and we
            retry the next candidate. SQLAlchemy's default ``BEGIN`` + SQLite's
            write lock serialise the read-modify-write, so the check-then-set is
            atomic from any single claimer's perspective.
        """
        owns_session = db is None
        db = db or SessionLocal()
        try:
            if engine.dialect.name == "postgresql":
                return self._claim_next_job_postgres(db)
            return self._claim_next_job_sqlite(db)
        finally:
            if owns_session:
                db.close()

    def _eligible_clause(self) -> str:
        # Shared SQL predicate: a pending job whose next_run_at is due or unset.
        return (
            "status = 'pending' "
            "AND (next_run_at IS NULL OR next_run_at <= :now)"
        )

    def _claim_next_job_postgres(self, db: Session) -> Optional[Dict[str, Any]]:
        now = datetime.now(timezone.utc)
        # Lock + select one eligible row, skipping rows other workers hold.
        row = db.execute(
            text(
                f"SELECT id FROM jobs WHERE {self._eligible_clause()} "
                "ORDER BY priority DESC, next_run_at ASC, created_at ASC "
                "FOR UPDATE SKIP LOCKED LIMIT 1"
            ),
            {"now": now},
        ).fetchone()
        if row is None:
            db.rollback()
            return None
        job_id = row[0]
        # Mark running + stamp ownership in the SAME transaction, then commit to
        # release the row lock.
        db.execute(
            text(
                "UPDATE jobs SET status = 'processing', started_at = :now, "
                "last_heartbeat = :now, locked_at = :now, worker_id = :wid "
                "WHERE id = :id"
            ),
            {"now": now, "wid": self.worker_id, "id": job_id},
        )
        db.commit()
        return self._load_claimed(db, job_id)

    def _claim_next_job_sqlite(self, db: Session) -> Optional[Dict[str, Any]]:
        now = datetime.now(timezone.utc)
        # Loop over candidates: a conditional UPDATE guarded by status='pending'.
        # rowcount==1 means we won; rowcount==0 means another writer already
        # claimed that id, so move to the next candidate. Bounded by a re-query
        # each iteration so we never spin forever.
        for _ in range(100):
            row = db.execute(
                text(
                    f"SELECT id FROM jobs WHERE {self._eligible_clause()} "
                    "ORDER BY priority DESC, next_run_at ASC, created_at ASC "
                    "LIMIT 1"
                ),
                {"now": now},
            ).fetchone()
            if row is None:
                db.rollback()
                return None
            job_id = row[0]
            result = db.execute(
                text(
                    "UPDATE jobs SET status = 'processing', started_at = :now, "
                    "last_heartbeat = :now, locked_at = :now, worker_id = :wid "
                    "WHERE id = :id AND status = 'pending'"
                ),
                {"now": now, "wid": self.worker_id, "id": job_id},
            )
            db.commit()
            if result.rowcount == 1:
                return self._load_claimed(db, job_id)
            # Lost the race for this id — re-query for the next candidate.
        logger.warning("claim_next_job: gave up after 100 contended attempts")
        return None

    def _load_claimed(self, db: Session, job_id: int) -> Optional[Dict[str, Any]]:
        job = db.query(Job).filter(Job.id == job_id).first()
        if job is None:
            return None
        return {"id": job.id, "type": job.type, "payload": job.payload}

    def recover_jobs(self):
        """Reset jobs THIS worker previously owned back to pending on startup.

        Horizontal-scaling note: we must NOT blindly reset every ``processing``
        job, because with multiple replicas another live worker may be actively
        running them. We only requeue jobs stamped with our own ``worker_id``
        (or legacy rows with no owner) — those are ours from a previous run of
        this process that crashed. Jobs owned by other live workers are left
        alone; the heartbeat reaper recovers genuinely dead ones.
        """
        try:
            with SessionLocal() as db:
                stuck_jobs = (
                    db.query(Job)
                    .filter(
                        Job.status == "processing",
                        or_(
                            Job.worker_id == self.worker_id,
                            Job.worker_id == None,  # noqa: E711 legacy/unowned
                        ),
                    )
                    .all()
                )
                if stuck_jobs:
                    logger.info(f"Recovering {len(stuck_jobs)} stuck jobs...")
                    for job in stuck_jobs:
                        job.status = "pending"
                        job.started_at = None
                        job.worker_id = None
                        job.locked_at = None
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
        logger.info("Queue Worker Loop Started (worker_id=%s)", self.worker_id)
        while self.is_running:
            try:
                # Atomically claim the next eligible job. Safe to run from N
                # replicas: claim_next_job() hands each queued job to exactly one
                # worker (Postgres FOR UPDATE SKIP LOCKED / SQLite guarded
                # conditional UPDATE). The claim flips it to 'processing' and
                # stamps this worker's id in the same transaction.
                claimed = await asyncio.to_thread(self.claim_next_job)

                if claimed:
                    await self._process_job(
                        claimed["id"], claimed["type"], claimed["payload"]
                    )
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
                # A process is the cancellation boundary. asyncio.to_thread cannot
                # stop its thread on timeout, so retrying used to overlap the first
                # attempt and duplicate rows/provider spend/external writes.
                from apps.api.services.job_process_runner import run_job_subprocess

                timeout = JOB_TIMEOUTS.get(job_type, DEFAULT_JOB_TIMEOUT)
                await run_job_subprocess(
                    job_id, job_type, payload, timeout=timeout
                )
            else:
                raise Exception(f"No handler for job type {job_type}")

        except Exception as e:
            from apps.api.services.job_process_runner import JobProcessTimeout

            if isinstance(e, JobProcessTimeout):
                logger.error("Job %s (%s) timed out: %s", job_id, job_type, e)
                error = str(e)
                status = "failed"
            else:
                logger.exception("Job %s failed", job_id)
                error = str(e)
                status = "failed"

            # Retry logic happens in the DB update below.
        except asyncio.CancelledError:
            # run_job_subprocess has already killed the child. Leave the claimed
            # row for heartbeat recovery rather than falsely completing it.
            raise
        finally:
            heartbeat_task.cancel()
            # Cancellation is cooperative. Await the heartbeat so it cannot
            # leak into the surrounding event loop as a pending 30-second sleep.
            with suppress(asyncio.CancelledError):
                await heartbeat_task

        # Update DB
        will_retry = False
        job_state_persisted = False
        cancelled_externally = False
        try:
            with SessionLocal() as db:
                # Serialize finalization against an API cancellation. If cancel
                # wins this lock first we preserve ``cancelled``; if the child
                # has already finished and finalization wins first, a later
                # cancel's status predicate correctly becomes a no-op.
                job = (
                    db.query(Job)
                    .filter(Job.id == job_id)
                    .with_for_update()
                    .first()
                )
                if job:
                    # Release ownership: the claim is finished one way or another.
                    # A requeued (pending) job must be unowned so any worker can
                    # re-claim it; terminal jobs simply no longer hold a lock.
                    job.worker_id = None
                    job.locked_at = None
                    if job.status == "cancelled":
                        cancelled_externally = True
                        job.completed_at = job.completed_at or datetime.now(timezone.utc)
                    elif status == "failed":
                        # Check Retry
                        if (job.retry_count or 0) < (job.max_retries or 3):
                            will_retry = True
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
                    job_state_persisted = True
        except Exception as e:
            logger.error(f"Failed to update job status: {e}")

        # Reconcile domain-owned state only after the queue transition commits.
        # A child may have been SIGKILLed on timeout, so this cannot live solely
        # inside the handler process.
        failure_handler = self.failure_handlers.get(job_type)
        if (
            status == "failed"
            and job_state_persisted
            and not cancelled_externally
            and failure_handler
        ):
            try:
                await asyncio.to_thread(
                    failure_handler,
                    job_id,
                    payload,
                    error or "job attempt failed",
                    will_retry,
                )
            except Exception:
                logger.exception(
                    "Failure reconciliation failed for Job %s (%s)",
                    job_id,
                    job_type,
                )

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
                logger.warning(f"Heartbeat failed for job {job_id}: {e}")

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
                        logger.warning(
                            f"Job {job.id} detected dead (heartbeat timeout). Recovering..."
                        )
                        job.status = (
                            "pending"  # Will trigger retry count check on next pickup?
                        )
                        # Actually we should increment retry here to avoid loops
                        job.retry_count = (job.retry_count or 0) + 1
                        job.error = "Heartbeat Timeout"
                        # Release the dead worker's claim so another worker can
                        # re-claim it cleanly.
                        job.worker_id = None
                        job.locked_at = None
                        job.next_run_at = datetime.now(timezone.utc) + timedelta(minutes=1)

                    if dead_jobs:
                        db.commit()

            except Exception as e:
                logger.error(f"Monitor loop error: {e}")


# Global Instance
queue_service = QueueService()
