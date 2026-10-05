"""Run the Python retention_enforce handler for the Go parity harness.

Executed in its own process (so the process time zone can be set) by
tests/test_retention_go_parity_pg.py:

    uv run python -m tests.retention_parity.python_runner \
        --db-url postgresql+psycopg://... --scenarios scenarios.json --out out.json

It drives apps/api/services/governance/retention.py unchanged: the real
``handle_retention_enforce`` and ``reconcile_retention_job_failure`` against a
PostgreSQL database, connected as a NOSUPERUSER NOBYPASSRLS role so forced
row-level security applies, with ``datetime.now`` frozen to the scenario
instant. ``--pure`` instead evaluates the side-effect-free helpers
(``normalized_days`` and the cutoff arithmetic of ``_targets``) over a corpus.
"""
from __future__ import annotations

import json
import os
import sys
from datetime import datetime

from tests.parity_harness import runner


def run_scenarios(args) -> None:
    spec = runner.load_scenarios(args.scenarios)
    frozen = datetime.fromisoformat(spec["frozen_now"])
    factory, dispose = runner.rls_session_factory(args.db_url)

    from apps.api.services.governance import retention

    runner.freeze_datetime(frozen, retention)
    retention.SessionLocal = factory

    owner = runner.owner_connection(args.db_url)

    def dispatch(step, job_id, payload):
        if step["op"] == "enforce":
            return retention.handle_retention_enforce(job_id, payload)
        message = step["error"] * step.get("error_repeat", 1)
        return retention.reconcile_retention_job_failure(job_id, payload, message, step["will_retry"])

    try:
        runner.run_steps(spec, owner, dispatch, args.out)
    finally:
        owner.close()
        dispose()


def run_pure(args) -> None:
    os.environ.setdefault("DATABASE_URL", "sqlite://")
    cases = json.load(open(args.pure))
    from apps.api.services.governance import retention

    out = {"normalize": [], "cutoff": []}
    for value in cases["normalize"]:
        try:
            result = retention.normalized_days(value)
            out["normalize"].append({"ok": result, "order": list(result)})
        except Exception as exc:
            out["normalize"].append({"error": str(exc)})

    class _Query:
        def filter(self, *clauses):
            return clauses[1].right.value  # the bound cutoff of `column < value`

    class _Db:
        def query(self, _model):
            return _Query()

    for case in cases["cutoff"]:
        now = datetime.fromisoformat(case["now"])
        values, error = [], None
        try:
            for _category, value in retention._targets(_Db(), "ws", case["snapshot"], now):
                if isinstance(value, datetime):
                    values.append({"t": "timestamp", "v": value.isoformat()})
                elif isinstance(value, str):
                    values.append({"t": "date", "v": value})
                else:
                    values.append({"t": "epoch", "v": value})
        except Exception as exc:
            error = str(exc)
        out["cutoff"].append({"values": values, "error": error})
    json.dump(out, open(args.out, "w"), indent=1)


def main() -> int:
    return runner.main(run_scenarios, run_pure)


if __name__ == "__main__":
    sys.exit(main())
