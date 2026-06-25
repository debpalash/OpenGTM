"""Standalone job worker — horizontally scalable.

This is the production job processor for the durable SQL job queue (the `jobs`
table driven by ``apps.api.services.queue_service.QueueService``). It is the
SAME queue the in-API background worker uses; the difference is that this runs
as its own process so you can scale it to N replicas behind one Postgres.

Concurrency safety
------------------
Every replica claims work via ``queue_service.claim_next_job()``, which is
atomic and dialect-aware:

  * Postgres → ``SELECT ... FOR UPDATE SKIP LOCKED LIMIT 1`` then flip to
    ``processing`` in the same transaction. Two replicas never grab the same
    job (so no double-charge), and a row another replica is claiming is skipped
    rather than blocked on.
  * SQLite (dev/tests) → a guarded conditional ``UPDATE`` whose ``rowcount``
    confirms the claim.

So this file is safe to run as ``replicas: N`` in docker-compose / k8s.

Run
---
    python -m apps.api.worker

Docker:
    command: python -m apps.api.worker

Relationship to the in-API worker
---------------------------------
When you run these standalone replicas, set ``RUN_INLINE_WORKER=0`` on the API
so it stops processing jobs itself (it still ENQUEUES them). In single-process
dev the API keeps its inline worker (default) and you don't need this process at
all.
"""

import asyncio
import logging
import os
import signal
import sys

# Make `apps.api...` importable when launched as a module or a file.
sys.path.insert(
    0, os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
)

logger = logging.getLogger("apps.api.worker")


def _configure_logging() -> None:
    logging.basicConfig(
        level=os.getenv("LOG_LEVEL", "INFO").upper(),
        format="%(asctime)s [%(name)s] %(levelname)s: %(message)s",
    )


def _register_handlers() -> None:
    """Register every job-type handler — mirrors apps/api/main.py.

    The worker process does not import the FastAPI app, so it must wire up the
    same handlers itself, otherwise a claimed job would have "no handler".
    """
    from apps.api.services.queue_service import queue_service
    from apps.api.workers.download import handle_download_link
    from apps.api.services.workbook.enrichment import handle_run_workbook
    from apps.api.services.workbook.source_engine import handle_source_workbook
    from apps.api.services.workbook.refresh import (
        handle_refresh_workbook,
        handle_signal_scan,
    )
    from apps.api.services.automations.engine import handle_trigger_eval
    from apps.api.services.outreach.sending import handle_send
    from apps.api.services.outreach.inbound import handle_inbound_poll
    from apps.api.services.poller.engine import handle_watch_poll

    queue_service.register_handler("download_link", handle_download_link)
    queue_service.register_handler("run_workbook", handle_run_workbook)
    queue_service.register_handler("source_workbook", handle_source_workbook)
    queue_service.register_handler("refresh_workbook", handle_refresh_workbook)
    queue_service.register_handler("signal_scan", handle_signal_scan)
    queue_service.register_handler("trigger_eval", handle_trigger_eval)
    queue_service.register_handler("send", handle_send)
    queue_service.register_handler("outreach_inbound_poll", handle_inbound_poll)
    queue_service.register_handler("watch_poll", handle_watch_poll)


async def run_worker() -> None:
    """Run the standalone claim/process loop until a shutdown signal."""
    _configure_logging()

    # Ensure the schema exists / is up to date (alembic upgrade head, with a
    # create_all fallback). Honours YUPCHA_DB_INIT just like the API. Safe to
    # run from multiple replicas — alembic is idempotent and serialises via the
    # alembic_version table.
    try:
        from apps.api.db_init import init_db

        init_db()
    except Exception as e:  # pragma: no cover - defensive
        logger.warning(f"init_db skipped/failed (continuing): {e}")

    from apps.api.services.queue_service import queue_service

    _register_handlers()

    logger.info("Standalone worker starting (worker_id=%s)", queue_service.worker_id)

    # start_worker() recovers THIS worker's previously-owned jobs, then spawns
    # the claim loop + heartbeat reaper as asyncio tasks.
    await queue_service.start_worker()
    logger.info("Standalone worker ready — claiming jobs")

    loop = asyncio.get_running_loop()
    stop = asyncio.Event()

    def _request_shutdown() -> None:
        logger.info("Shutdown signal received — stopping worker...")
        stop.set()

    for sig in (signal.SIGTERM, signal.SIGINT):
        try:
            loop.add_signal_handler(sig, _request_shutdown)
        except NotImplementedError:  # pragma: no cover - non-unix
            signal.signal(sig, lambda *_: _request_shutdown())

    await stop.wait()

    await queue_service.stop_worker()
    # Give in-flight loop iterations a moment to notice is_running=False.
    await asyncio.sleep(1.5)
    logger.info("Standalone worker shut down cleanly")


def main() -> None:
    asyncio.run(run_worker())


if __name__ == "__main__":
    main()
