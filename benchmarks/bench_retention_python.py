"""Python retention_enforce benchmark, the counterpart of
apps/server/internal/jobs/retention/bench_test.go. See benchmarks/README.md.

For each row count it seeds a workspace with N expired and a few recent rows in
each of the eight purged tables (benchmarks/retention_seed.sql), then runs the
production code unchanged, as the NOSUPERUSER NOBYPASSRLS role, in two modes:

* ``python-handler``  ``handle_retention_enforce`` called in-process: the
                      handler alone, the like-for-like comparison with Go's
                      ``Worker.Handle``.
* ``python-job``      the production path for a job: ``claim_next_job`` then
                      ``QueueService._process_job``, which spawns a child
                      process (``python -m apps.api.job_process``) that imports
                      the full handler registry, runs the real handler, and
                      finalizes the job. Timed from enqueue to completed.
"""

from __future__ import annotations

import argparse
import asyncio
import json
import os
import platform
import sys
import time

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
if ROOT not in sys.path:
    sys.path.insert(0, ROOT)

TABLES = ["governance_audit_events", "llm_usage_daily", "signals", "destination_deliveries",
          "destination_inbound_receipts", "audience_membership_events", "playbook_results", "outreach_sends"]


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--owner-url", required=True, help="postgresql:// superuser URL (setup only)")
    ap.add_argument("--app-url", required=True, help="postgresql+psycopg:// URL of the NOBYPASSRLS role")
    ap.add_argument("--seed-sql", required=True)
    ap.add_argument("--rows", default="1000,20000,100000")
    ap.add_argument("--kept", type=int, default=100)
    ap.add_argument("--reps", type=int, default=1)
    ap.add_argument("--rep-offset", type=int, default=0)
    ap.add_argument("--out", required=True)
    args = ap.parse_args()

    # Must precede any apps.api import: settings and the engine read the env.
    os.environ["DATABASE_URL"] = args.app_url
    os.environ["RUN_INLINE_WORKER"] = "0"
    import psycopg

    import apps.api.core.config as _cfg

    assert os.path.realpath(_cfg.__file__).startswith(os.path.realpath(ROOT)), _cfg.__file__
    from apps.api.services.governance import retention
    from apps.api.services.job_registry import register_job_handlers
    from apps.api.services.queue_service import QueueService

    seed_sql = open(args.seed_sql).read()
    sizes = [int(x) for x in args.rows.split(",")]
    owner = psycopg.connect(args.owner_url, autocommit=True)

    def reset() -> None:
        owner.execute(
            "TRUNCATE " + ", ".join(TABLES) + ", playbook_runs, research_playbooks, destination_runs, "
            "audience_destinations, audiences, retention_runs, retention_policies, retention_schedules, jobs CASCADE")
        # No leftover route from the Go run: Python must own the type.
        owner.execute("DELETE FROM job_executor_routes WHERE job_type = 'retention_enforce'")

    def seed(ws: str, rows: int) -> None:
        owner.execute(seed_sql + f"\nSELECT pg_temp.bench_seed('{ws}', {rows}, {args.kept});")
        owner.execute("ANALYZE")

    def insert_job(ws: str, status: str) -> int:
        payload = json.dumps({"workspace_id": ws, "run_id": f"run-{ws}"})
        if status == "processing":
            row = owner.execute(
                """INSERT INTO jobs (type, payload, workspace_id, priority, status, created_at, next_run_at, max_retries,
                                     retry_count, worker_id, locked_at, last_heartbeat)
                   VALUES ('retention_enforce', %s::json, %s, 1, 'processing', LOCALTIMESTAMP, LOCALTIMESTAMP, 3, 0,
                           'bench', LOCALTIMESTAMP, LOCALTIMESTAMP) RETURNING id""", (payload, ws)).fetchone()
        else:
            row = owner.execute(
                """INSERT INTO jobs (type, payload, workspace_id, priority, status, created_at, next_run_at, max_retries, retry_count)
                   VALUES ('retention_enforce', %s::json, %s, 1, 'pending', LOCALTIMESTAMP, LOCALTIMESTAMP, 3, 0)
                   RETURNING id""", (payload, ws)).fetchone()
        return row[0]

    def verify(ws: str, rows: int) -> int:
        status, counts = owner.execute("SELECT status, deleted_counts::text FROM retention_runs WHERE id = %s",
                                       (f"run-{ws}",)).fetchone()
        if status != "completed":
            raise RuntimeError(f"run status = {status}")
        for table in TABLES:
            (n,) = owner.execute(f"SELECT count(*) FROM {table} WHERE workspace_id = %s", (ws,)).fetchone()
            if n != args.kept:
                raise RuntimeError(f"{table} left {n} rows, want {args.kept}")
        deleted = sum(json.loads(counts).values())
        if deleted != 8 * rows:
            raise RuntimeError(f"deleted {deleted} rows, want {8 * rows}")
        return deleted

    qs = QueueService(concurrency=1)
    register_job_handlers(qs)

    cases: list[dict] = []

    def run_one(mode: str, rows: int, rep: int, record: bool) -> None:
        reset()
        ws = f"bench-{mode}-{rows}-{rep}"
        seed(ws, rows)
        if mode == "python-handler":
            job_id = insert_job(ws, "processing")
            payload = {"workspace_id": ws, "run_id": f"run-{ws}"}
            t0 = time.perf_counter()
            asyncio.run(retention.handle_retention_enforce(job_id, payload))
            wall = time.perf_counter() - t0
        else:
            t0 = time.perf_counter()
            job_id = insert_job(ws, "pending")
            claimed = qs.claim_next_job()
            if claimed is None or claimed["id"] != job_id:
                raise RuntimeError(f"claimed {claimed!r}, expected job {job_id}")
            asyncio.run(qs._process_job(claimed["id"], claimed["type"], claimed["payload"], locked_at=claimed["locked_at"]))
            wall = time.perf_counter() - t0
            (status,) = owner.execute("SELECT status FROM jobs WHERE id = %s", (job_id,)).fetchone()
            if status != "completed":
                raise RuntimeError(f"job ended as {status}")
        deleted = verify(ws, rows)
        if record:
            cases.append({"mode": mode, "rows": rows, "kept": args.kept, "rep": rep, "wall_ms": wall * 1000, "deleted": deleted})
            print(f"{mode} rows={rows} rep={rep}: {wall * 1000:.0f} ms ({deleted} rows deleted)", flush=True)

    for mode in ("python-handler", "python-job"):
        run_one(mode, 500, 0, False)  # warm-up, discarded
    for rep in range(args.reps):
        for rows in sizes:
            for mode in ("python-handler", "python-job"):
                run_one(mode, rows, args.rep_offset + rep, True)

    owner.close()
    with open(args.out, "w") as f:
        json.dump({"engine": "python", "python_version": platform.python_version(), "cases": cases}, f, indent=2)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
