"""Benchmark-only job child: the production child plus one no-op handler.

``apps.api.job_process`` is what QueueService spawns for every job. It imports
the full production handler registry, which is where most of the per-job cost
lives. This wrapper keeps all of that and only registers a no-op handler for
the benchmark's job type (argv[2]) so the measured cost is the process
boundary, not any real work. The production registry is not modified.
"""

from __future__ import annotations

import sys

import apps.api.job_process as job_process
import apps.api.services.job_registry as job_registry

_production_register = job_registry.register_job_handlers


async def _noop(job_id: int, payload: dict) -> None:
    return None


def _register(queue):
    names = _production_register(queue)
    queue.register_handler(sys.argv[2], _noop)
    return names


job_registry.register_job_handlers = _register

if __name__ == "__main__":
    raise SystemExit(job_process.main())
