"""Run the Python research_playbook_schedule handler for the Go parity harness.

    uv run python -m tests.playbook_schedule_parity.python_runner \
        --db-url postgresql+psycopg://... --scenarios scenarios.json --out out.json

Drives apps/api/services/playbooks/scheduler.py unchanged (``handle_playbook_schedule``,
which uses ``schedule_next``, ``remove_schedule`` and ``queue_service.add_job``) against
a PostgreSQL database as a NOSUPERUSER NOBYPASSRLS role under forced row-level
security, with ``datetime.now`` of the scheduler module frozen to the scenario instant.
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
    from apps.api.services.playbooks import scheduler

    runner.freeze_datetime(frozen, scheduler)
    scheduler.SessionLocal = factory
    owner = runner.owner_connection(args.db_url)

    def dispatch(step, job_id, payload):
        assert step["op"] == "tick", step
        return registry.handlers["research_playbook_schedule"](job_id, payload)

    try:
        runner.run_steps(spec, owner, dispatch, args.out)
    finally:
        owner.close()
        dispose()


def main() -> int:
    return runner.main(run_scenarios)


if __name__ == "__main__":
    sys.exit(main())
