"""Child-process entrypoint for one durable job invocation."""

from __future__ import annotations

import asyncio
import json
import logging
import sys


async def _dispatch(job_id: int, job_type: str, payload: dict) -> None:
    from apps.api.services.job_registry import register_job_handlers
    from apps.api.services.queue_service import QueueService

    registry = QueueService()
    register_job_handlers(registry)
    handler = registry.handlers.get(job_type)
    if handler is None:
        raise LookupError(f"No handler registered for job type {job_type!r}")
    await handler(job_id, payload)


def main() -> int:
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s [job-child:%(process)d] [%(name)s] %(levelname)s: %(message)s",
    )
    if len(sys.argv) != 3:
        logging.error("usage: python -m apps.api.job_process JOB_ID JOB_TYPE")
        return 2

    try:
        job_id = int(sys.argv[1])
        job_type = sys.argv[2]
        payload = json.load(sys.stdin)
        if not isinstance(payload, dict):
            raise TypeError("job payload must be a JSON object")
        asyncio.run(_dispatch(job_id, job_type, payload))
    except Exception:
        logging.exception("Job child failed")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
