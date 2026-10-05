"""Run the Python audience_refresh handler for the Go parity harness.

    uv run python -m tests.audience_refresh_parity.python_runner \
        --db-url postgresql+psycopg://... --scenarios scenarios.json --out out.json

Drives apps/api/services/audiences unchanged: ``handle_audience_refresh`` (which runs
``refresh_audience_observed``, ``schedule_next``, the membership-event emitter and
``enqueue_audience_syncs``) and ``reconcile_audience_refresh_failure``, obtained from the
same registry a production job child builds. It runs against PostgreSQL as a NOSUPERUSER
NOBYPASSRLS role under forced row-level security, with ``datetime.now`` frozen in the
modules that read the clock and the page size lowered to the scenario's.

Two things replace state the Go server cannot read and that is not under test: the
workspace metadata lookup (a local SQLite file; ``workspace_slug``) returns a made-up slug
for every workspace, and the lead store connects as the RLS role of this run.
"""
from __future__ import annotations

import sys
from datetime import datetime

from tests.parity_harness import runner


def run_scenarios(args) -> None:
    spec = runner.load_scenarios(args.scenarios)
    frozen = datetime.fromisoformat(spec["frozen_now"])
    factory, dispose = runner.rls_session_factory(args.db_url)

    registry = runner.production_registry()
    from apps.api.core.config import settings
    from apps.api.services.audiences import refresh, scheduler
    from apps.api.services.leadgen import models as lead_models
    from apps.api.services.leadgen import store as lead_store
    from apps.api.services.workspace import manager as workspace_manager

    runner.freeze_datetime(frozen, scheduler, refresh, lead_models)
    scheduler.SessionLocal = factory
    lead_store.SessionLocal = factory
    refresh.AUDIENCE_REFRESH_PAGE_SIZE = int(spec["page_size"])
    workspace_manager.workspace_slug = lambda workspace_id: f"slug-{workspace_id}"
    settings.AUTOMATIONS_ENABLED = False
    owner = runner.owner_connection(args.db_url)

    def dispatch(step, job_id, payload):
        op = step["op"]
        if op == "config":
            settings.AUTOMATIONS_ENABLED = bool(step["automations"])
            return None
        if op == "refresh":
            return registry.handlers["audience_refresh"](job_id, payload)
        if op == "reconcile":
            message = step["error"] * step.get("error_repeat", 1)
            return registry.failure_handlers["audience_refresh"](job_id, payload, message, step["will_retry"])
        raise AssertionError(f"unknown op {op!r}")

    try:
        runner.run_steps(spec, owner, dispatch, args.out)
    finally:
        owner.close()
        dispose()


def main() -> int:
    return runner.main(run_scenarios)


if __name__ == "__main__":
    sys.exit(main())
