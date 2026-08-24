from apps.api.services.job_registry import register_job_handlers
from apps.api.services.queue_service import queue_service


EXPECTED_JOB_TYPES = {
    "ambitionbox_import",
    "bulk_enrich",
    "collect",
    "data_collector_import",
    "download_link",
    "outreach_inbound_poll",
    "refresh_workbook",
    "run_workbook",
    "send",
    "signal_scan",
    "source_health_check",
    "source_workbook",
    "trigger_eval",
    "watch_poll",
}


def test_shared_registry_is_complete():
    queue_service.handlers.clear()
    queue_service.failure_handlers.clear()
    registered = register_job_handlers(queue_service)

    assert registered == EXPECTED_JOB_TYPES
    assert set(queue_service.handlers) == EXPECTED_JOB_TYPES
    assert set(queue_service.failure_handlers) == {"ambitionbox_import", "collect"}
