"""Canonical registration for every durable queue job handler.

Both the optional in-API worker and the standalone worker consume the same
registry.  Keeping the imports here prevents a newly introduced job type from
working in development while failing with ``No handler`` in production.
"""

from __future__ import annotations

from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from apps.api.services.queue_service import QueueService


def register_job_handlers(queue: "QueueService") -> frozenset[str]:
    """Register all supported durable job types and return their names."""
    from apps.api.services.automations.engine import handle_trigger_eval
    from apps.api.services.leadgen.job_runner import (
        handle_bulk_enrich,
        handle_collect,
        reconcile_collect_job_failure,
    )
    from apps.api.services.leadgen.source_health import handle_source_health_check
    from apps.api.services.leadgen.scrapers.data_collector_import import (
        handle_data_collector_import,
    )
    from apps.api.services.outreach.inbound import handle_inbound_poll
    from apps.api.services.outreach.sending import handle_send
    from apps.api.services.poller.engine import handle_watch_poll
    from apps.api.services.workbook.enrichment import handle_run_workbook
    from apps.api.services.workbook.ambitionbox_import import (
        handle_ambitionbox_import,
        reconcile_ambitionbox_job_failure,
    )
    from apps.api.services.workbook.refresh import (
        handle_refresh_workbook,
        handle_signal_scan,
    )
    from apps.api.services.workbook.source_engine import handle_source_workbook
    from apps.api.workers.download import handle_download_link

    handlers = {
        "download_link": handle_download_link,
        "run_workbook": handle_run_workbook,
        "ambitionbox_import": handle_ambitionbox_import,
        "data_collector_import": handle_data_collector_import,
        "source_workbook": handle_source_workbook,
        "collect": handle_collect,
        "bulk_enrich": handle_bulk_enrich,
        "refresh_workbook": handle_refresh_workbook,
        "signal_scan": handle_signal_scan,
        "trigger_eval": handle_trigger_eval,
        "send": handle_send,
        "outreach_inbound_poll": handle_inbound_poll,
        "watch_poll": handle_watch_poll,
        "source_health_check": handle_source_health_check,
    }
    for job_type, handler in handlers.items():
        queue.register_handler(job_type, handler)
    queue.register_failure_handler(
        "ambitionbox_import", reconcile_ambitionbox_job_failure
    )
    queue.register_failure_handler("collect", reconcile_collect_job_failure)
    return frozenset(handlers)
