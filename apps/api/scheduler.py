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
    from apps.api.services.audiences.scheduler import bootstrap_audience_schedules
    from apps.api.services.governance.retention import bootstrap_retention_schedules
    from apps.api.services.playbooks.scheduler import bootstrap_playbook_schedules

    return (
        ("signal_scan", bootstrap_signal_scan),
        ("automations", bootstrap_schedules),
        ("outreach", bootstrap_outreach_schedules),
        ("outreach_inbound", bootstrap_inbound_schedules),
        ("intent_poller", bootstrap_watch_schedules),
        ("source_health", bootstrap_source_health),
        ("audiences", bootstrap_audience_schedules),
        ("retention", bootstrap_retention_schedules),
        ("playbooks", bootstrap_playbook_schedules),
    )


def reconcile_once(leadership=None) -> dict[str, object]:
    """Run every bootstrap once.

    With ``leadership`` (a :class:`~apps.api.services.scheduler_lease.Leadership`)
    a bootstrap runs only while this process holds that scheduler's lease; its
    commits are fenced by the lease token. Without it every bootstrap runs, which
    is right for exactly one scheduler process (the default).
    """
    results: dict[str, object] = {}
    for name, bootstrap in _bootstrap_functions():
        try:
            if leadership is None:
                results[name] = bootstrap()
                continue
            with leadership.lead(name) as lease:
                if lease is None:
                    results[name] = "skipped: another replica leads"
                else:
                    results[name] = bootstrap()
        except Exception as exc:  # one subsystem must not starve the others
            logger.exception("Scheduler reconciliation failed for %s", name)
            results[name] = f"error: {exc}"
    return results


def build_leadership():
    """Leadership for this process, or None when election is off or unavailable.

    Election needs PostgreSQL (the lease table is created by migration
    9b3d5f7a2c41). Turning it on against SQLite is a configuration error, not a
    silent no-op: a second scheduler on another host would then run unguarded.
    """
    from apps.api.core.config import settings

    if not settings.SCHEDULER_LEADER_ELECTION:
        return None
    from apps.api.database import IS_SQLITE, SessionLocal, engine
    from apps.api.services import scheduler_lease as lease

    if IS_SQLITE:
        raise RuntimeError(
            "SCHEDULER_LEADER_ELECTION=true requires PostgreSQL; this database is SQLite"
        )
    lease.install_session_fence(SessionLocal)
    names = [name for name, _ in _bootstrap_functions()]
    leadership = lease.Leadership(
        engine, names, ttl_seconds=max(3, settings.SCHEDULER_LEASE_TTL_SECONDS)
    )
    leadership.poll()
    leadership.start()
    return leadership


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

    leadership = build_leadership()
    logger.info(
        "Scheduler ready; reconciliation interval=%ss leader_election=%s",
        interval, leadership is not None,
    )
    try:
        while not stop.is_set():
            logger.info("Scheduler reconciliation: %s", reconcile_once(leadership))
            try:
                await asyncio.wait_for(stop.wait(), timeout=interval)
            except asyncio.TimeoutError:
                pass
    finally:
        if leadership is not None:
            leadership.stop()  # hand the leases over so a peer leads at once
    logger.info("Scheduler stopped")


def main() -> None:
    asyncio.run(run_scheduler())


if __name__ == "__main__":
    main()
