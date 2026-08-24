"""Dedicated reconciliation loop for recurring durable jobs."""

from __future__ import annotations

import asyncio
import logging
import os
import signal

logger = logging.getLogger("apps.api.scheduler")


def _bootstrap_functions():
    from apps.api.services.automations.engine import bootstrap_schedules
    from apps.api.services.leadgen.source_health import bootstrap_source_health
    from apps.api.services.outreach.inbound import bootstrap_inbound_schedules
    from apps.api.services.outreach.sending import bootstrap_outreach_schedules
    from apps.api.services.poller.engine import bootstrap_watch_schedules
    from apps.api.services.workbook.refresh import bootstrap_signal_scan

    return (
        ("signal_scan", bootstrap_signal_scan),
        ("automations", bootstrap_schedules),
        ("outreach", bootstrap_outreach_schedules),
        ("outreach_inbound", bootstrap_inbound_schedules),
        ("intent_poller", bootstrap_watch_schedules),
        ("source_health", bootstrap_source_health),
    )


def reconcile_once() -> dict[str, object]:
    results: dict[str, object] = {}
    for name, bootstrap in _bootstrap_functions():
        try:
            results[name] = bootstrap()
        except Exception as exc:  # one subsystem must not starve the others
            logger.exception("Scheduler reconciliation failed for %s", name)
            results[name] = f"error: {exc}"
    return results


async def run_scheduler() -> None:
    logging.basicConfig(
        level=os.getenv("LOG_LEVEL", "INFO").upper(),
        format="%(asctime)s [%(name)s] %(levelname)s: %(message)s",
    )
    interval = max(10, int(os.getenv("SCHEDULER_RECONCILE_SECONDS", "60")))
    stop = asyncio.Event()
    loop = asyncio.get_running_loop()

    for sig in (signal.SIGTERM, signal.SIGINT):
        try:
            loop.add_signal_handler(sig, stop.set)
        except NotImplementedError:  # pragma: no cover - non-Unix
            signal.signal(sig, lambda *_: stop.set())

    logger.info("Scheduler ready; reconciliation interval=%ss", interval)
    while not stop.is_set():
        logger.info("Scheduler reconciliation: %s", reconcile_once())
        try:
            await asyncio.wait_for(stop.wait(), timeout=interval)
        except asyncio.TimeoutError:
            pass
    logger.info("Scheduler stopped")


def main() -> None:
    asyncio.run(run_scheduler())


if __name__ == "__main__":
    main()
