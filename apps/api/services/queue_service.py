import asyncio
from contextlib import suppress
import logging
import os
import socket
import uuid
from datetime import datetime, timedelta, timezone
from typing import Dict, Any, Optional, Callable, Awaitable

from sqlalchemy import select, update, or_, text, cast, String, func
from sqlalchemy.orm import Session

from apps.api.database import SessionLocal, engine
from apps.api.models import Job
from apps.api.core.config import settings

logger = logging.getLogger(__name__)


def _timestamp_matches(column, value):
    # Historical SQLite claims were written by the raw datetime adapter with
    # an offset. Preserve recovery of those leases during a rolling upgrade.
    if engine.dialect.name == "sqlite" and value is not None and value.tzinfo:
        return or_(column == value, cast(column, String) == value.isoformat(" "))
    return column == value


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
    "audience_refresh": 900,
    "audience_destination_sync": 1800,
    "research_playbook_run": 3600,
    "research_playbook_schedule": 300,
    "retention_enforce": 1800,
}


class QueueService:
    def __init__(self, concurrency: Optional[int] = None, shutdown_grace: Optional[int] = None):
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
        self.concurrency = max(1, min(64, concurrency if concurrency is not None else settings.WORKER_CONCURRENCY))
        self.shutdown_grace = max(0, min(300, shutdown_grace if shutdown_grace is not None else settings.WORKER_SHUTDOWN_GRACE_SECONDS))
        self.max_active_per_workspace = max(0, min(64, settings.WORKER_MAX_ACTIVE_PER_WORKSPACE))
        self._worker_tasks: list[asyncio.Task] = []
        self._monitor_task: Optional[asyncio.Task] = None
        self._active_slots: set[int] = set()

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
            workspace_id=str(payload.get("workspace_id")) if payload.get("workspace_id") else None,
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

    def claim_next_job(
        self,
        db: Optional[Session] = None,
        fire_key_prefix: Optional[str] = None,
    ) -> Optional[Dict[str, Any]]:
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
                return self._claim_next_job_postgres(db, fire_key_prefix)
            return self._claim_next_job_sqlite(db, fire_key_prefix)
        finally:
            if owns_session:
                db.close()

    def _eligible_clause(self) -> str:
        # Shared SQL predicate: a pending job whose next_run_at is due or unset.
        return (
            "j.status = 'pending' "
            "AND (j.next_run_at IS NULL OR j.next_run_at <= :now) "
            # CAST: PostgreSQL cannot infer the type of a NULL-only parameter
            # ("could not determine data type of parameter"), which made every
            # claim fail when no prefix filter was supplied.
            "AND (CAST(:fire_key_prefix AS TEXT) IS NULL "
            "OR j.fire_key LIKE CAST(:fire_key_prefix AS TEXT)) "
            "AND (j.workspace_id IS NULL OR :tenant_cap = 0 OR "
            "(SELECT COUNT(*) FROM jobs active WHERE active.status = 'processing' "
            "AND active.workspace_id = j.workspace_id) < :tenant_cap)"
        )

    def _claim_next_job_postgres(
        self, db: Session, fire_key_prefix: Optional[str] = None
    ) -> Optional[Dict[str, Any]]:
        now = datetime.now(timezone.utc)
        for _ in range(100):
            # Row lock prevents double-grab. The tenant advisory lock serializes
            # cap rechecks across replicas that selected DIFFERENT rows for the
            # same workspace before either claim became visible.
            row = db.execute(
                text(
                    f"SELECT j.id, j.workspace_id FROM jobs j WHERE {self._eligible_clause()} "
                    "ORDER BY j.priority DESC, (SELECT COUNT(*) FROM jobs active WHERE active.status = 'processing' AND active.workspace_id = j.workspace_id) ASC, j.next_run_at ASC, j.created_at ASC "
                    "FOR UPDATE SKIP LOCKED LIMIT 1"
                ),
                {
                    "now": now,
                    "tenant_cap": self.max_active_per_workspace,
                    "fire_key_prefix": f"{fire_key_prefix}%"
                    if fire_key_prefix
                    else None,
                },
            ).fetchone()
            if row is None:
                db.rollback()
                return None
            job_id, workspace_id = row[0], row[1]
            if workspace_id and self.max_active_per_workspace:
                db.execute(text("SELECT pg_advisory_xact_lock(hashtext(:workspace_id))"), {"workspace_id": workspace_id})
                active = db.execute(text("SELECT COUNT(*) FROM jobs WHERE status='processing' AND workspace_id=:workspace_id"), {"workspace_id": workspace_id}).scalar_one()
                if active >= self.max_active_per_workspace:
                    db.rollback()
                    continue
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
        logger.warning("claim_next_job: gave up after 100 tenant-cap retries")
        return None

    def _claim_next_job_sqlite(
        self, db: Session, fire_key_prefix: Optional[str] = None
    ) -> Optional[Dict[str, Any]]:
        # Match SQLAlchemy's DateTime serialization, including six fractional
        # digits. Raw sqlite datetime adapters otherwise append a UTC offset,
        # making subsequent ORM lease comparisons fail despite equal instants.
        now = datetime.now(timezone.utc).strftime("%Y-%m-%d %H:%M:%S.%f")
        # Loop over candidates: a conditional UPDATE guarded by status='pending'.
        # rowcount==1 means we won; rowcount==0 means another writer already
        # claimed that id, so move to the next candidate. Bounded by a re-query
        # each iteration so we never spin forever.
        for _ in range(100):
            row = db.execute(
                text(
                    f"SELECT j.id FROM jobs j WHERE {self._eligible_clause()} "
                    "ORDER BY j.priority DESC, (SELECT COUNT(*) FROM jobs active WHERE active.status = 'processing' AND active.workspace_id = j.workspace_id) ASC, j.next_run_at ASC, j.created_at ASC "
                    "LIMIT 1"
                ),
                {
                    "now": now,
                    "tenant_cap": self.max_active_per_workspace,
                    "fire_key_prefix": f"{fire_key_prefix}%"
                    if fire_key_prefix
                    else None,
                },
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
        return {"id": job.id, "type": job.type, "payload": job.payload,
                "locked_at": job.locked_at}

    def _recover_attempts(self, *, startup: bool = False) -> None:
        """Recover expired claims with a compare-and-swap against their lease."""
        now = datetime.now(timezone.utc)
        threshold = now - timedelta(minutes=5)
        recovered = []
        with SessionLocal() as db:
            query = db.query(Job).filter(Job.status == "processing")
            if startup:
                query = query.filter(or_(Job.worker_id == self.worker_id,
                                         Job.worker_id == None))  # noqa: E711
            else:
                query = query.filter(or_(Job.last_heartbeat < threshold,
                                         Job.last_heartbeat == None))  # noqa: E711
            for job in query.all():
                retries = job.retry_count or 0
                limit = job.max_retries if job.max_retries is not None else 3
                retry = retries < limit
                reason = "Recovered from crash" if startup else "Heartbeat Timeout"
                # A concurrent heartbeat, cancellation, or another reaper wins
                # by changing any of these fields. Never overwrite that change.
                changed = db.query(Job).filter(
                    Job.id == job.id, Job.status == "processing",
                    Job.worker_id == job.worker_id, _timestamp_matches(Job.locked_at, job.locked_at),
                    _timestamp_matches(Job.last_heartbeat, job.last_heartbeat),
                    Job.retry_count == job.retry_count,
                ).update({
                    Job.status: "pending" if retry else "failed",
                    Job.retry_count: retries + 1 if retry else retries,
                    Job.worker_id: None, Job.locked_at: None,
                    Job.started_at: None, Job.last_heartbeat: None,
                    Job.next_run_at: now + timedelta(minutes=2 ** retries) if retry else None,
                    Job.completed_at: None if retry else now,
                    Job.error: reason if retry else f"Final Failure: {reason}",
                }, synchronize_session=False)
                if changed:
                    recovered.append((job.id, job.type, job.payload, reason, retry))
            db.commit()
        for job_id, job_type, payload, reason, retry in recovered:
            callback = self.failure_handlers.get(job_type)
            if callback:
                try:
                    callback(job_id, payload, reason, retry)
                except Exception:
                    logger.exception("Failure reconciliation failed for recovered job %s", job_id)

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
            self._recover_attempts(startup=True)
        except Exception as e:
            logger.error(f"Failed to recover jobs: {e}")

    def _transition_dead_jobs(self, db, jobs, reason: str, now: datetime) -> list[tuple]:
        """Apply the normal retry ceiling to abandoned processing claims."""
        notices = []
        for job in jobs:
            retries = job.retry_count or 0
            max_retries = job.max_retries if job.max_retries is not None else 3
            will_retry = retries < max_retries
            job.worker_id = None
            job.locked_at = None
            if will_retry:
                job.status = "pending"
                job.retry_count = retries + 1
                job.started_at = None
                job.next_run_at = now + timedelta(minutes=2 ** retries)
                job.error = f"Retry {job.retry_count}: {reason}"
            else:
                job.status = "failed"
                job.completed_at = now
                job.error = f"Final Failure: {reason}"
            notices.append((job.id, job.type, dict(job.payload or {}), reason, will_retry))
        db.commit()
        return notices

    def _reconcile_dead_jobs(self, notices: list[tuple]) -> None:
        """Notify domain ledgers after recovered queue state is committed."""
        for job_id, job_type, payload, reason, will_retry in notices:
            handler = self.failure_handlers.get(job_type)
            if handler is None:
                continue
            try:
                handler(job_id, payload, reason, will_retry)
            except Exception:
                logger.exception(
                    "Failure reconciliation failed for recovered Job %s (%s)",
                    job_id, job_type,
                )

    def reap_dead_jobs_once(self, now: Optional[datetime] = None) -> int:
        """Recover stale heartbeat claims once, safely across worker replicas."""
        now = now or datetime.now(timezone.utc)
        threshold = now - timedelta(minutes=5)
        with SessionLocal() as db:
            query = db.query(Job).filter(
                Job.status == "processing",
                or_(Job.last_heartbeat < threshold, Job.last_heartbeat == None),
            )
            if db.get_bind().dialect.name == "postgresql":
                query = query.with_for_update(skip_locked=True)
            dead_jobs = query.all()
            for job in dead_jobs:
                logger.warning(
                    "Job %s detected dead (heartbeat timeout). Recovering...", job.id
                )
            notices = self._transition_dead_jobs(db, dead_jobs, "Heartbeat Timeout", now)
        self._reconcile_dead_jobs(notices)
        return len(notices)

    async def start_worker(self):
        if self.is_running:
            return
        self.is_running = True
        self._shutdown_event.clear()

        # Recover jobs before starting loop
        self.recover_jobs()

        logger.info("Starting Queue Worker with %s slot(s)...", self.concurrency)
        self._worker_tasks = [asyncio.create_task(self._worker_loop(slot), name=f"queue-slot-{slot}") for slot in range(self.concurrency)]
        self._monitor_task = asyncio.create_task(self._monitor_heartbeats(), name="queue-heartbeat-monitor")

    async def stop_worker(self):
        self.is_running = False
        self._shutdown_event.set()
        logger.info("Stopping Queue Worker; draining %s slot(s)...", len(self._worker_tasks))
        if self._worker_tasks:
            try:
                async with asyncio.timeout(self.shutdown_grace):
                    await asyncio.gather(*self._worker_tasks)
            except TimeoutError:
                logger.warning("Worker drain exceeded %ss; cancelling active slots", self.shutdown_grace)
                for task in self._worker_tasks: task.cancel()
                await asyncio.gather(*self._worker_tasks, return_exceptions=True)
        if self._monitor_task:
            self._monitor_task.cancel()
            with suppress(asyncio.CancelledError): await self._monitor_task
        self._worker_tasks = []
        self._monitor_task = None

    async def _worker_loop(self, slot: int = 0):
        logger.info("Queue Worker Slot Started (worker_id=%s slot=%s)", self.worker_id, slot)
        while self.is_running:
            try:
                # Atomically claim the next eligible job. Safe to run from N
                # replicas: claim_next_job() hands each queued job to exactly one
                # worker (Postgres FOR UPDATE SKIP LOCKED / SQLite guarded
                # conditional UPDATE). The claim flips it to 'processing' and
                # stamps this worker's id in the same transaction.
                claimed = await asyncio.to_thread(self.claim_next_job)

                if claimed:
                    self._active_slots.add(slot)
                    try:
                        await self._process_job(
                            claimed["id"], claimed["type"], claimed["payload"],
                            locked_at=claimed["locked_at"],
                        )
                    finally:
                        self._active_slots.discard(slot)
                else:
                    try: await asyncio.wait_for(self._shutdown_event.wait(), timeout=1)
                    except TimeoutError: pass

            except Exception as e:
                logger.error(f"Worker loop error: {e}")
                try: await asyncio.wait_for(self._shutdown_event.wait(), timeout=5)
                except TimeoutError: pass

    def metrics(self, db: Session) -> Dict[str, Any]:
        """Aggregate queue health without exposing job payloads or credentials."""
        now = datetime.now(timezone.utc)
        counts = {status: count for status, count in db.query(Job.status, func.count(Job.id)).group_by(Job.status).all()}
        types = {job_type: count for job_type, count in db.query(Job.type, func.count(Job.id)).filter(Job.status.in_(["pending", "processing"])).group_by(Job.type).all()}
        oldest = db.query(Job.created_at).filter(Job.status == "pending").order_by(Job.created_at.asc()).first()
        active_workers = db.query(Job.worker_id).filter(Job.status == "processing", Job.worker_id.isnot(None)).distinct().count()
        oldest_created = oldest[0].replace(tzinfo=timezone.utc) if oldest and oldest[0].tzinfo is None else oldest[0] if oldest else None
        oldest_age = max(0.0, (now - oldest_created).total_seconds()) if oldest_created else 0.0
        return {"worker_id": self.worker_id, "configured_concurrency": self.concurrency, "max_active_per_workspace": self.max_active_per_workspace, "local_active_slots": len(self._active_slots), "active_workers": active_workers, "counts": counts, "active_by_type": types, "oldest_pending_age_seconds": round(oldest_age, 3), "observed_at": now.isoformat()}

    async def _process_job(self, job_id: int, job_type: str, payload: Dict, *, locked_at=None):
        logger.info(f"Processing Job {job_id} ({job_type})")
        handler = self.handlers.get(job_type)

        error = None
        status = "completed"

        # Start Heartbeat Task for this job
        if locked_at is None:
            with SessionLocal() as db:
                claim = db.query(Job).filter(Job.id == job_id,
                    Job.worker_id == self.worker_id, Job.status == "processing").first()
                if claim is None:
                    return
                locked_at = claim.locked_at
        heartbeat_task = asyncio.create_task(self._job_heartbeat(job_id, locked_at))

        try:
            if handler:
                # A process is the cancellation boundary. asyncio.to_thread cannot
                # stop its thread on timeout, so retrying used to overlap the first
                # attempt and duplicate rows/provider spend/external writes.
                from apps.api.services.job_process_runner import run_job_subprocess

                timeout = JOB_TIMEOUTS.get(job_type, DEFAULT_JOB_TIMEOUT)
                await run_job_subprocess(
                    job_id, job_type, {**payload, "__queue_lease": {
                        "worker_id": self.worker_id, "locked_at": locked_at.isoformat() if locked_at else None,
                    }} if job_type == "run_workbook" else payload, timeout=timeout,
                    should_continue=lambda: self._claim_is_active(job_id, locked_at),
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
                    .filter(Job.id == job_id, Job.worker_id == self.worker_id,
                            _timestamp_matches(Job.locked_at, locked_at),
                            Job.status.in_(["processing", "cancelled"]))
                    .with_for_update()
                    .first()
                )
                if job:
                    previous_status = job.status
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
                        if (job.retry_count or 0) < (job.max_retries if job.max_retries is not None else 3):
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

                    # SQLite ignores FOR UPDATE. Repeat the lease + status
                    # predicate in the write itself so cancellation/reclaim
                    # between the read and update cannot be overwritten.
                    values = {name: getattr(job, name) for name in (
                        "worker_id", "locked_at", "status", "completed_at",
                        "retry_count", "next_run_at", "error",
                    )}
                    db.expunge(job)
                    changed = db.query(Job).filter(
                        Job.id == job_id, Job.worker_id == self.worker_id,
                        _timestamp_matches(Job.locked_at, locked_at),
                        Job.status == previous_status,
                    ).update(values, synchronize_session=False)
                    db.commit()
                    job_state_persisted = bool(changed)
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

    def _claim_is_active(self, job_id: int, locked_at) -> bool:
        with SessionLocal() as db:
            return db.query(Job.id).filter(
                Job.id == job_id, Job.worker_id == self.worker_id,
                _timestamp_matches(Job.locked_at, locked_at), Job.status == "processing",
            ).first() is not None

    def _renew_claim(self, job_id: int, locked_at) -> None:
        with SessionLocal() as db:
            db.query(Job).filter(
                Job.id == job_id, Job.worker_id == self.worker_id,
                _timestamp_matches(Job.locked_at, locked_at), Job.status == "processing",
            ).update({Job.last_heartbeat: datetime.now(timezone.utc)}, synchronize_session=False)
            db.commit()

    async def _job_heartbeat(self, job_id: int, locked_at):
        while True:
            try:
                await asyncio.sleep(self.heartbeat_interval)
                await asyncio.to_thread(self._renew_claim, job_id, locked_at)
            except asyncio.CancelledError:
                break
            except Exception as e:
                logger.warning(f"Heartbeat failed for job {job_id}: {e}")

    async def _monitor_heartbeats(self):
        """Monitor for dead jobs (processing but no heartbeat)."""
        while self.is_running:
            try:
                await asyncio.sleep(60)  # Check every minute
                await asyncio.to_thread(self._recover_attempts)

            except Exception as e:
                logger.error(f"Monitor loop error: {e}")


# Global Instance
queue_service = QueueService()
