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

import argparse
import asyncio
import json
import os
import sys
import time
from datetime import datetime


def _setup_clock(frozen: datetime):
    from apps.api.services.governance import retention

    class Frozen(datetime):
        @classmethod
        def now(cls, tz=None):
            return frozen if tz is not None else frozen.replace(tzinfo=None)

    retention.datetime = Frozen
    return retention


def run_scenarios(args) -> None:
    os.environ["DATABASE_URL"] = args.db_url
    spec = json.load(open(args.scenarios))
    frozen = datetime.fromisoformat(spec["frozen_now"])

    import psycopg

    from tests.pg_rls_support import rls_app_session

    factory, dispose = rls_app_session(args.db_url, pool_size=4)
    retention = _setup_clock(frozen)
    retention.SessionLocal = factory

    owner = psycopg.connect(args.db_url.replace("postgresql+psycopg://", "postgresql://"), autocommit=True)
    results = []
    try:
        for step in spec["steps"]:
            job_id = step["job"]
            (text,) = owner.execute("SELECT payload::text FROM jobs WHERE id = %s", (job_id,)).fetchone()
            payload = json.loads(text)
            error = None
            try:
                if step["op"] == "enforce":
                    asyncio.run(retention.handle_retention_enforce(job_id, payload))
                else:
                    message = step["error"] * step.get("error_repeat", 1)
                    retention.reconcile_retention_job_failure(job_id, payload, message, step["will_retry"])
            except Exception as exc:  # the queue records str(exc) as the attempt failure
                error = str(exc)
            results.append({"job": job_id, "op": step["op"], "error": error})
    finally:
        owner.close()
        dispose()
    json.dump(results, open(args.out, "w"), indent=1)


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
    parser = argparse.ArgumentParser()
    parser.add_argument("--db-url")
    parser.add_argument("--scenarios")
    parser.add_argument("--pure")
    parser.add_argument("--out", required=True)
    parser.add_argument("--tz", default="")
    args = parser.parse_args()
    if args.tz:
        os.environ["TZ"] = args.tz
        time.tzset()
    (run_pure if args.pure else run_scenarios)(args)
    return 0


if __name__ == "__main__":
    sys.exit(main())
