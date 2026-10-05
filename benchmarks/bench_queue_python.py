"""Python QueueService benchmark (claim + complete), the counterpart of
apps/server/internal/queue/bench_test.go. See benchmarks/README.md.

Two modes, because production Python always runs a handler in a child process:

* ``inproc``     the production claim and finalization code, with the process
                 boundary replaced by an in-process call of a no-op handler.
                 This isolates queue bookkeeping and is the like-for-like
                 comparison with the Go queue.
* ``subprocess`` the unmodified production path: ``run_job_subprocess`` spawns
                 ``python -m apps.api.job_process`` per job, which imports the
                 full handler registry. Only the handler is a no-op.

Each concurrency slot mirrors QueueService._worker_loop (claim in a thread,
then _process_job), except that an empty claim retries after 1 ms instead of
sleeping 1 s: the backlog is finite and the idle poll would dominate the tail.
"""

from __future__ import annotations

import argparse
import asyncio
import json
import math
import os
import platform
import sys
import time

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
if ROOT not in sys.path:
    sys.path.insert(0, ROOT)


def stats(samples_s: list[float]) -> dict:
    if not samples_s:
        return {"n": 0, "mean": 0, "p50": 0, "p95": 0, "p99": 0, "max": 0}
    ms = sorted(s * 1000 for s in samples_s)

    def pick(p: float) -> float:
        # Nearest-rank, identical to the Go harness.
        rank = math.ceil(p / 100 * len(ms)) - 1
        return ms[min(max(rank, 0), len(ms) - 1)]

    return {
        "n": len(ms),
        "mean": sum(ms) / len(ms),
        "p50": pick(50),
        "p95": pick(95),
        "p99": pick(99),
        "max": ms[-1],
    }


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--owner-url", required=True, help="postgresql:// superuser URL (setup only)")
    ap.add_argument("--app-url", required=True, help="postgresql+psycopg:// URL of the NOBYPASSRLS role")
    ap.add_argument("--mode", choices=["inproc", "subprocess"], required=True)
    ap.add_argument("--jobs", type=int, default=2000)
    ap.add_argument("--warmup", type=int, default=200)
    ap.add_argument("--concurrency", default="1,4,16")
    ap.add_argument("--reps", type=int, default=1)
    ap.add_argument("--rep-offset", type=int, default=0)
    ap.add_argument("--out", required=True)
    args = ap.parse_args()

    # Must precede any apps.api import: settings and the engine read the env.
    os.environ["DATABASE_URL"] = args.app_url
    os.environ["RUN_INLINE_WORKER"] = "0"
    import psycopg

    import apps.api.core.config as _cfg

    # Guard against importing a different checkout than the sandbox copy.
    assert os.path.realpath(_cfg.__file__).startswith(os.path.realpath(ROOT)), _cfg.__file__
    from apps.api.database import SessionLocal
    from apps.api.services import job_process_runner
    from apps.api.services.queue_service import QueueService

    mode = args.mode
    engine_label = "python-" + mode

    if mode == "inproc":
        # Keep the pre-flight claim check run_job_subprocess performs, drop the
        # fork/exec and the 1 s claim monitor.
        async def run_inproc(job_id, job_type, payload, *, timeout, should_continue=None, poll_interval=1.0):
            if should_continue and not await asyncio.to_thread(should_continue):
                raise job_process_runner.JobProcessError("claim cancelled or lost")
            await HANDLERS[job_type](job_id, payload)

        job_process_runner.run_job_subprocess = run_inproc
    else:
        real_exec = asyncio.create_subprocess_exec

        async def exec_bench_child(program, *a, **kw):
            # Same interpreter, same flags, same stdin protocol; only the module
            # differs (production child + a no-op handler for the bench type).
            if a[:2] == ("-m", "apps.api.job_process"):
                a = ("-m", "benchmarks.bench_child", *a[2:])
                kw["stdout"] = asyncio.subprocess.DEVNULL
                kw["stderr"] = asyncio.subprocess.DEVNULL
                kw["cwd"] = ROOT
            return await real_exec(program, *a, **kw)

        asyncio.create_subprocess_exec = exec_bench_child

    async def noop(job_id, payload):
        return None

    HANDLERS = {}

    levels = [int(x) for x in args.concurrency.split(",")]

    def reset():
        # Private throwaway database: every case starts from an empty table.
        with psycopg.connect(args.owner_url, autocommit=True) as c:
            c.execute("TRUNCATE jobs")
            c.execute("VACUUM ANALYZE jobs")

    async def run_case(job_type: str, conc: int, n: int) -> dict:
        qs = QueueService(concurrency=conc)
        qs.register_handler(job_type, noop)
        HANDLERS[job_type] = noop

        t0 = time.perf_counter()
        with SessionLocal() as db:
            for _ in range(n):
                qs.add_job(db, job_type, {})
        enqueue_rate = n / (time.perf_counter() - t0)
        with psycopg.connect(args.owner_url, autocommit=True) as c:
            c.execute("ANALYZE jobs")

        claims: list[float] = []
        procs: list[float] = []
        cycles: list[float] = []
        state = {"done": 0, "first": None, "last": None}

        async def slot():
            while state["done"] < n:
                t0 = time.perf_counter()
                claimed = await asyncio.to_thread(qs.claim_next_job)
                t1 = time.perf_counter()
                if claimed is None:
                    await asyncio.sleep(0.001)
                    continue
                await qs._process_job(
                    claimed["id"], claimed["type"], claimed["payload"], locked_at=claimed["locked_at"]
                )
                t2 = time.perf_counter()
                claims.append(t1 - t0)
                procs.append(t2 - t1)
                cycles.append(t2 - t0)
                if state["first"] is None:
                    state["first"] = t0
                state["last"] = t2
                state["done"] += 1

        await asyncio.gather(*(slot() for _ in range(conc)))
        wall = state["last"] - state["first"]

        with psycopg.connect(args.owner_url) as c:
            completed = c.execute(
                "SELECT count(*) FROM jobs WHERE type = %s AND status = 'completed'", (job_type,)
            ).fetchone()[0]
        if completed != n:
            raise RuntimeError(f"{engine_label} c={conc}: completed {completed} of {n} jobs")
        return {
            "engine": "python",
            "mode": engine_label,
            "concurrency": conc,
            "jobs": n,
            "wall_s": wall,
            "jobs_per_sec": n / wall,
            "enqueue_per_sec": enqueue_rate,
            "claim_ms": stats(claims),
            "process_ms": stats(procs),
            "cycle_ms": stats(cycles),
            "failed": 0,
        }

    async def run_all() -> list[dict]:
        cases = []
        for rep in range(args.reps):
            for c in levels:
                job_type = f"bench_noop_{mode}_c{c}_r{args.rep_offset + rep}"
                reset()
                await run_case(job_type, c, args.warmup)  # warm-up, discarded
                reset()
                res = await run_case(job_type, c, args.jobs)
                res["rep"] = args.rep_offset + rep
                cases.append(res)
                print(
                    f"{engine_label} c={c} rep={res['rep']}: {res['jobs_per_sec']:.1f} jobs/s, "
                    f"cycle p50={res['cycle_ms']['p50']:.2f}ms p99={res['cycle_ms']['p99']:.2f}ms",
                    flush=True,
                )
        return cases

    cases = asyncio.run(run_all())
    with open(args.out, "w") as f:
        json.dump(
            {"engine": "python", "mode": engine_label, "python_version": platform.python_version(), "cases": cases},
            f,
            indent=2,
        )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
