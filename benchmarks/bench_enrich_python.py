"""Python side of the enrichment benchmark (see benchmarks/README.md, "Enrichment").

Runs one already-seeded ``run_workbook`` job through the production handler,
``handle_run_workbook``, in-process (so the per-job child process of
``QueueService`` is not part of the measurement; it would only add about a
second per job). Two ways of running the provider call:

* ``pool``     the unmodified production path: ``provider_runner.run_provider``
               schedules the call on a pebble ProcessPool of killable workers.
* ``threads``  the call runs on a thread of the same process: no process
               isolation, no pickling, so it is the cheapest Python can be and a
               lower bound on its cost.

Redis broadcasts are disabled (there is no Redis in the benchmark); production
Python publishes one message per cell, so this slightly favours Python. The
sandbox's ``sitecustomize.py`` (written by run_enrich.py) lets the SSRF guard
accept the loopback simulator, in this process and in the pool's workers.
"""

from __future__ import annotations

import argparse
import asyncio
import json
import os
import sys
import time

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
if ROOT not in sys.path:
    sys.path.insert(0, ROOT)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--owner-url", required=True)
    ap.add_argument("--app-url", required=True)
    ap.add_argument("--job", type=int, required=True)
    ap.add_argument("--mode", choices=["pool", "threads"], required=True)
    ap.add_argument("--out", required=True)
    args = ap.parse_args()

    # Must precede any apps.api import: settings and the engine read the env.
    os.environ["DATABASE_URL"] = args.app_url
    os.environ["RUN_INLINE_WORKER"] = "0"
    os.environ["CONNECTOR_SIGNATURE_POLICY"] = "optional"
    os.environ["REDIS_URL"] = "redis://127.0.0.1:1"

    import psycopg

    import apps.api.core.config as _cfg

    assert os.path.realpath(_cfg.__file__).startswith(os.path.realpath(ROOT)), _cfg.__file__
    from apps.api.services.workbook import enrichment, provider_runner

    enrichment._make_redis = lambda: None
    with psycopg.connect(args.owner_url) as c:
        text, worker_id, locked_at = c.execute(
            "SELECT payload::text, worker_id, locked_at FROM jobs WHERE id = %s", (args.job,)
        ).fetchone()
    payload = json.loads(text)
    payload["__queue_lease"] = {"worker_id": worker_id, "locked_at": locked_at.isoformat()}

    if args.mode == "threads":
        # The pool bounds in-flight provider calls to its worker count; keep that bound.
        gate = None

        async def run_provider(provider_name, lead, timeout):
            nonlocal gate
            if gate is None:
                gate = asyncio.Semaphore(int(payload.get("provider_workers") or 8))
            async with gate:
                task = asyncio.to_thread(provider_runner._provider_job, provider_name, lead.to_dict())
                return await asyncio.wait_for(task, timeout=timeout)

        enrichment.run_provider = run_provider

    async def go() -> float:
        # Warm the pool outside the measurement so process start is not billed
        # to the first cells (the production pool is long-lived across jobs).
        if args.mode == "pool":
            provider_runner.ensure_workers(payload.get("provider_workers"))
            from apps.api.services.leadgen.models import Lead

            warm = [
                provider_runner.run_provider(payload["warm_provider"], Lead(), 30)
                for _ in range(int(payload.get("provider_workers") or 8))
            ] if payload.get("warm_provider") else []
            await asyncio.gather(*warm, return_exceptions=True)
        start = time.perf_counter()
        await enrichment.handle_run_workbook(args.job, payload)
        return time.perf_counter() - start

    wall = asyncio.run(go())
    if args.mode == "pool":
        provider_runner.shutdown()
    import resource

    # Largest single process: this one or, after shutdown() reaped them, a pool worker (KiB on Linux).
    rss_kib = max(resource.getrusage(resource.RUSAGE_SELF).ru_maxrss, resource.getrusage(resource.RUSAGE_CHILDREN).ru_maxrss)
    with open(args.out, "w") as f:
        json.dump({"engine": "python", "mode": args.mode, "wall_s": wall, "max_rss_mb": rss_kib / 1024}, f)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
