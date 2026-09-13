"""Controlled, self-cleaning load verifier for the durable SQL queue."""

import math
import threading
import time
import uuid
from collections import defaultdict
from datetime import datetime, timezone

from apps.api.database import SessionLocal, engine
from apps.api.models import Job
from apps.api.services.queue_service import QueueService

CONFIRMATION = "RUN QUEUE LOAD TEST"


def _percentile(values: list[float], percentile: float) -> float:
    if not values:
        return 0.0
    ordered = sorted(values)
    index = min(len(ordered) - 1, max(0, math.ceil(percentile * len(ordered)) - 1))
    return ordered[index]


def run_queue_load_test(
    *,
    jobs: int = 500,
    tenants: int = 20,
    claimers: int = 16,
    tenant_cap: int = 2,
    hold_ms: int = 5,
    confirmation: str = "",
    allow_sqlite: bool = False,
) -> dict:
    """Exercise concurrent claims and verify queue safety invariants."""
    if confirmation != CONFIRMATION:
        raise ValueError(f"confirmation must equal {CONFIRMATION}")
    if engine.dialect.name != "postgresql" and not allow_sqlite:
        raise RuntimeError(
            "controlled queue load validation requires PostgreSQL "
            "(use allow_sqlite only for harness tests)"
        )
    if (
        not 1 <= jobs <= 100_000
        or not 1 <= tenants <= jobs
        or not 1 <= claimers <= 128
        or not 1 <= tenant_cap <= 64
        or not 0 <= hold_ms <= 10_000
    ):
        raise ValueError("load parameters are outside safe bounds")

    with SessionLocal() as db:
        active_before = db.query(Job).filter(
            Job.status.in_(["pending", "processing"])
        ).count()
        if active_before:
            raise RuntimeError(
                "queue must have no pending or processing jobs before a controlled load test"
            )

    tag = f"queue-load:{uuid.uuid4().hex}:"
    started = time.perf_counter()
    claimed_ids: set[int] = set()
    duplicates: list[int] = []
    active: defaultdict[str, int] = defaultdict(int)
    maximum: defaultdict[str, int] = defaultdict(int)
    latencies: list[float] = []
    failures: list[str] = []
    state_lock = threading.Lock()
    stop = threading.Event()
    safe_to_cleanup = False
    services = [QueueService() for _ in range(claimers)]
    for service in services:
        service.max_active_per_workspace = tenant_cap

    try:
        with SessionLocal() as db:
            now = datetime.now(timezone.utc)
            db.add_all(
                [
                    Job(
                        type="queue_load_probe",
                        payload={"workspace_id": f"load-tenant-{index % tenants}"},
                        workspace_id=f"load-tenant-{index % tenants}",
                        priority=1,
                        fire_key=f"{tag}{index}",
                        status="pending",
                        created_at=now,
                        next_run_at=now,
                        max_retries=0,
                    )
                    for index in range(jobs)
                ]
            )
            db.commit()

        def consume(service: QueueService) -> None:
            while not stop.is_set():
                before = time.perf_counter()
                try:
                    claimed = service.claim_next_job(fire_key_prefix=tag)
                    elapsed = time.perf_counter() - before
                    if claimed is None:
                        with SessionLocal() as db:
                            pending = db.query(Job).filter(
                                Job.fire_key.like(f"{tag}%"), Job.status == "pending"
                            ).count()
                        if pending == 0:
                            return
                        time.sleep(0.002)
                        continue
                    workspace_id = str(
                        (claimed.get("payload") or {}).get("workspace_id", "")
                    )
                    with state_lock:
                        latencies.append(elapsed)
                        if claimed["id"] in claimed_ids:
                            duplicates.append(claimed["id"])
                        else:
                            claimed_ids.add(claimed["id"])
                        active[workspace_id] += 1
                        maximum[workspace_id] = max(
                            maximum[workspace_id], active[workspace_id]
                        )
                    try:
                        time.sleep(hold_ms / 1000)
                        with SessionLocal() as db:
                            row = db.query(Job).filter(
                                Job.id == claimed["id"],
                                Job.fire_key.like(f"{tag}%"),
                            ).one()
                            row.status = "completed"
                            row.completed_at = datetime.now(timezone.utc)
                            row.worker_id = None
                            row.locked_at = None
                            db.commit()
                    finally:
                        with state_lock:
                            active[workspace_id] -= 1
                except Exception as exc:
                    with state_lock:
                        failures.append(f"{type(exc).__name__}: {exc}")
                        should_stop = len(failures) >= 50
                    if should_stop:
                        stop.set()
                    else:
                        time.sleep(0.01)

        threads = [
            threading.Thread(target=consume, args=(service,)) for service in services
        ]
        for thread in threads:
            thread.start()
        deadline = time.monotonic() + 300
        for thread in threads:
            thread.join(timeout=max(0, deadline - time.monotonic()))
        alive = sum(thread.is_alive() for thread in threads)
        if alive:
            stop.set()
            for thread in threads:
                thread.join(timeout=10)
            alive = sum(thread.is_alive() for thread in threads)
        safe_to_cleanup = alive == 0

        duration = time.perf_counter() - started
        with SessionLocal() as db:
            completed = db.query(Job).filter(
                Job.fire_key.like(f"{tag}%"), Job.status == "completed"
            ).count()
            pending = db.query(Job).filter(
                Job.fire_key.like(f"{tag}%"), Job.status == "pending"
            ).count()
        violations = {
            tenant: count for tenant, count in maximum.items() if count > tenant_cap
        }
        report = {
            "ok": completed == jobs
            and pending == 0
            and not duplicates
            and not violations
            and not failures
            and alive == 0,
            "run_tag": tag,
            "dialect": engine.dialect.name,
            "jobs": jobs,
            "tenants": tenants,
            "claimers": claimers,
            "tenant_cap": tenant_cap,
            "completed": completed,
            "pending": pending,
            "duplicate_claims": len(duplicates),
            "cap_violations": violations,
            "claimer_failures": failures[:20],
            "alive_claimers": alive,
            "duration_seconds": round(duration, 3),
            "throughput_jobs_per_second": round(completed / duration, 2)
            if duration
            else 0.0,
            "claim_latency_ms": {
                "p50": round(_percentile(latencies, 0.50) * 1000, 3),
                "p95": round(_percentile(latencies, 0.95) * 1000, 3),
                "p99": round(_percentile(latencies, 0.99) * 1000, 3),
            },
            "max_active_by_tenant": dict(maximum),
        }
        if not report["ok"]:
            raise RuntimeError(f"queue load validation failed: {report}")
        return report
    finally:
        if safe_to_cleanup:
            with SessionLocal() as db:
                db.query(Job).filter(Job.fire_key.like(f"{tag}%")).delete(
                    synchronize_session=False
                )
                db.commit()
