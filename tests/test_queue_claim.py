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


def test_workspace_ownership_and_active_cap_prevent_tenant_monopoly():
    service = QueueService()
    service.max_active_per_workspace = 1
    with SessionLocal() as db:
        first = service.add_job(db, "run_workbook", {"workspace_id": "ws-a", "n": 1})
        second = service.add_job(db, "run_workbook", {"workspace_id": "ws-a", "n": 2})
        other = service.add_job(db, "run_workbook", {"workspace_id": "ws-b", "n": 3})
        assert first.workspace_id == second.workspace_id == "ws-a"
        assert other.workspace_id == "ws-b"
        first_id, second_id, other_id = first.id, second.id, other.id

    claimed_first = service.claim_next_job()
    claimed_other = service.claim_next_job()
    assert claimed_first["id"] == first_id
    assert claimed_other["id"] == other_id
    assert service.claim_next_job() is None

    with SessionLocal() as db:
        row = db.query(Job).filter(Job.id == first_id).one()
        row.status = "completed"; row.worker_id = None; db.commit()
    assert service.claim_next_job()["id"] == second_id


def test_postgres_claim_serializes_tenant_cap_with_advisory_lock(monkeypatch):
    service = QueueService(); service.max_active_per_workspace = 2
    statements = []

    class Result:
        def __init__(self, row=None, scalar=None): self.row, self.scalar = row, scalar
        def fetchone(self): return self.row
        def scalar_one(self): return self.scalar

    class FakeDb:
        def execute(self, statement, params):
            sql = str(statement); statements.append(sql)
            if "SELECT j.id" in sql: return Result((17, "ws-a"))
            if "SELECT COUNT(*)" in sql: return Result(scalar=0)
            return Result()
        def commit(self): pass
        def rollback(self): pass

    monkeypatch.setattr(service, "_load_claimed", lambda db, job_id: {"id": job_id})
    assert service._claim_next_job_postgres(FakeDb()) == {"id": 17}
    assert any("pg_advisory_xact_lock" in statement for statement in statements)
    advisory_index = statements.index(next(s for s in statements if "pg_advisory_xact_lock" in s))
    recheck_index = statements.index(next(s for s in statements if s.strip().startswith("SELECT COUNT(*) FROM jobs WHERE")))
    assert advisory_index < recheck_index


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


def _route(job_type: str, executor: str) -> None:
    from apps.api.models import JobExecutorRoute
    with SessionLocal() as db:
        db.merge(JobExecutorRoute(job_type=job_type, executor=executor))
        db.commit()


def _drop_routes(*job_types: str) -> None:
    from apps.api.models import JobExecutorRoute
    with SessionLocal() as db:
        db.query(JobExecutorRoute).filter(
            JobExecutorRoute.job_type.in_(job_types)
        ).delete(synchronize_session=False)
        db.commit()


def test_python_never_claims_go_routed_job_types():
    """Executor routing: Python and Go workers share the jobs table, so a type
    routed to Go must stay pending for Python while unrouted and explicitly
    Python-routed types are claimed exactly as before."""
    _route("route_go_only", "go")
    _route("route_py_explicit", "python")
    try:
        with SessionLocal() as db:
            go_ids = {queue_service.add_job(db, "route_go_only", {"i": i}, priority=5).id for i in range(3)}
            # Seeded by the migration: the first Go-only type.
            go_ids.add(queue_service.add_job(db, "plugin_run", {}, priority=5).id)
            py_ids = {queue_service.add_job(db, "route_py_explicit", {}).id,
                      queue_service.add_job(db, "route_unrouted", {}).id}
        claimed = []
        while (job := queue_service.claim_next_job()) is not None:
            claimed.append(job["id"])
        assert sorted(claimed) == sorted(py_ids)
        with SessionLocal() as db:
            pending = {j.id for j in db.query(Job).filter(Job.status == "pending")}
        assert pending == go_ids
    finally:
        _drop_routes("route_go_only", "route_py_explicit")


def test_rerouting_back_to_python_releases_pending_jobs():
    """Rollback path: flipping a route to python makes queued jobs claimable."""
    _route("route_flip", "go")
    try:
        with SessionLocal() as db:
            job_id = queue_service.add_job(db, "route_flip", {}).id
        assert queue_service.claim_next_job() is None
        _route("route_flip", "python")
        assert queue_service.claim_next_job()["id"] == job_id
    finally:
        _drop_routes("route_flip")


def test_python_recovery_leaves_go_routed_claims_to_go():
    """The Go worker reaps its own stale claims and runs its own failure
    reconciliation, so Python recovery must skip Go-routed types."""
    from datetime import datetime, timedelta, timezone

    _route("route_go_reap", "go")
    service = QueueService()
    stale = datetime.now(timezone.utc) - timedelta(minutes=30)
    try:
        with SessionLocal() as db:
            ids = {}
            for job_type in ("route_go_reap", "route_py_reap"):
                job = service.add_job(db, job_type, {})
                job.status, job.last_heartbeat = "processing", stale
                job.worker_id, job.locked_at = service.worker_id, stale
                ids[job_type] = job.id
            db.commit()

        assert service.reap_dead_jobs_once() == 1
        service.recover_jobs()
        with SessionLocal() as db:
            status = {j.type: j.status for j in db.query(Job).filter(Job.id.in_(ids.values()))}
        assert status == {"route_go_reap": "processing", "route_py_reap": "pending"}
    finally:
        _drop_routes("route_go_reap")


# ── retention_enforce: the first migrated job type ───────────────────────────
# It has a Go executor (apps/server/internal/jobs/retention) but must stay
# Python-owned until an operator runs `opengtm routes set retention_enforce go`
# and must return to Python on `... python`. The generic routing tests above
# prove the mechanism; these pin it for this concrete type.

def test_retention_enforce_is_python_owned_until_routed():
    from apps.api.models import JobExecutorRoute

    with SessionLocal() as db:
        # Migrations seed plugin_run -> go, and deliberately nothing for retention.
        routes = {r.job_type: r.executor for r in db.query(JobExecutorRoute)}
    assert routes.get("plugin_run") == "go"
    assert "retention_enforce" not in routes

    with SessionLocal() as db:
        job_id = queue_service.add_job(db, "retention_enforce", {"workspace_id": "ws-r"}).id
    claimed = queue_service.claim_next_job()
    assert claimed is not None and claimed["id"] == job_id


def test_retention_enforce_cutover_and_rollback_are_routing_only():
    with SessionLocal() as db:
        job_id = queue_service.add_job(
            db, "retention_enforce", {"workspace_id": "ws-r", "run_id": "run-1"},
            fire_key="retention-run:run-1",
        ).id
    try:
        _route("retention_enforce", "go")  # cutover: Python must stop claiming it
        assert queue_service.claim_next_job() is None
        with SessionLocal() as db:
            assert db.query(Job).filter(Job.id == job_id).one().status == "pending"

        _route("retention_enforce", "python")  # rollback: Python picks it up again
        claimed = queue_service.claim_next_job()
        assert claimed is not None and claimed["id"] == job_id
    finally:
        _drop_routes("retention_enforce")


def test_python_recovery_leaves_go_routed_retention_claims_to_go():
    """A stale retention_enforce claim is reaped (and reconciled) by whichever
    executor owns the type, never by both: Go's reaper runs its own
    reconciler, so Python reaping a Go-routed claim would skip it."""
    from datetime import datetime, timedelta, timezone

    service = QueueService()
    reconciled = []
    service.register_failure_handler(
        "retention_enforce", lambda job_id, payload, reason, will_retry: reconciled.append((job_id, will_retry)))
    stale = datetime.now(timezone.utc) - timedelta(minutes=30)
    with SessionLocal() as db:
        job = service.add_job(db, "retention_enforce", {"workspace_id": "ws-r"})
        job.status, job.last_heartbeat = "processing", stale
        job.worker_id, job.locked_at = service.worker_id, stale
        db.commit()
        job_id = job.id

    try:
        _route("retention_enforce", "go")
        assert service.reap_dead_jobs_once() == 0
        with SessionLocal() as db:
            assert db.query(Job).filter(Job.id == job_id).one().status == "processing"
        assert reconciled == []

        _route("retention_enforce", "python")  # rolled back: Python owns the claim again
        assert service.reap_dead_jobs_once() == 1
        with SessionLocal() as db:
            assert db.query(Job).filter(Job.id == job_id).one().status == "pending"
        assert reconciled == [(job_id, True)]
    finally:
        _drop_routes("retention_enforce")


def test_python_registers_the_retention_executor_for_rollback():
    """Rollback needs the Python handler and its failure reconciler to stay
    registered; the Go port does not remove them."""
    from apps.api.services.job_registry import register_job_handlers

    service = QueueService()
    assert "retention_enforce" in register_job_handlers(service)
    assert "retention_enforce" in service.failure_handlers


# ── run_workbook_connector: the connector enrichment slice (M2) ──────────────
# Go executor in apps/server/internal/jobs/enrich. Python-owned until
# `opengtm routes set run_workbook_connector go`; back with `... python`. Only
# the type is routed: run_workbook itself is untouched.

def test_run_workbook_connector_is_python_owned_until_routed_and_run_workbook_never_moves():
    from apps.api.models import JobExecutorRoute

    with SessionLocal() as db:
        routes = {r.job_type: r.executor for r in db.query(JobExecutorRoute)}
    assert "run_workbook_connector" not in routes and "run_workbook" not in routes

    with SessionLocal() as db:
        connector_id = queue_service.add_job(db, "run_workbook_connector", {"workspace_id": "ws-c"}).id
        legacy_id = queue_service.add_job(db, "run_workbook", {"workspace_id": "ws-c"}).id
    try:
        _route("run_workbook_connector", "go")  # cutover
        claimed = queue_service.claim_next_job()
        assert claimed is not None and claimed["id"] == legacy_id  # run_workbook is still Python's
        assert queue_service.claim_next_job() is None  # the connector job waits for Go
        _route("run_workbook_connector", "python")  # rollback: one routing change, no deploy
        assert queue_service.claim_next_job()["id"] == connector_id
    finally:
        _drop_routes("run_workbook_connector")


def test_python_recovery_leaves_go_routed_connector_claims_to_go():
    from datetime import datetime, timedelta, timezone

    service = QueueService()
    stale = datetime.now(timezone.utc) - timedelta(minutes=30)
    with SessionLocal() as db:
        job = service.add_job(db, "run_workbook_connector", {"workspace_id": "ws-c"})
        job.status, job.last_heartbeat = "processing", stale
        job.worker_id, job.locked_at = service.worker_id, stale
        db.commit()
        job_id = job.id
    try:
        _route("run_workbook_connector", "go")
        assert service.reap_dead_jobs_once() == 0
        _route("run_workbook_connector", "python")
        assert service.reap_dead_jobs_once() == 1
        with SessionLocal() as db:
            assert db.query(Job).filter(Job.id == job_id).one().status == "pending"
    finally:
        _drop_routes("run_workbook_connector")
