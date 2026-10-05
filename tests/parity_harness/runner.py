"""Python-side helpers for the job parity runners.

A runner (``tests/<job>_parity/python_runner.py``) drives the unchanged Python
handler in its own process, so the process time zone can be set per scenario:

    uv run python -m tests.retention_parity.python_runner --db-url ... --scenarios ... --out ...

It connects as a NOSUPERUSER NOBYPASSRLS role (so forced RLS applies) and
freezes the clock of the modules under test to the scenario instant. This
module holds what every runner needs.
"""
from __future__ import annotations

import argparse
import asyncio
import json
import os
import sys
import time
from datetime import datetime
from typing import Callable, Iterable


def base_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser()
    parser.add_argument("--db-url")
    parser.add_argument("--scenarios")
    parser.add_argument("--pure")
    parser.add_argument("--out", required=True)
    parser.add_argument("--tz", default="")
    return parser


def apply_timezone(tz: str) -> None:
    if tz:
        os.environ["TZ"] = tz
        time.tzset()


def freeze_datetime(frozen: datetime, *modules) -> None:
    """Replace ``datetime`` in each module with a class whose ``now`` returns ``frozen``.

    Only ``datetime.now`` is frozen; construction, arithmetic and parsing keep
    working because the replacement subclasses the real class.
    """

    class Frozen(datetime):
        @classmethod
        def now(cls, tz=None):
            return frozen if tz is not None else frozen.replace(tzinfo=None)

    for module in modules:
        module.datetime = Frozen


def rls_session_factory(db_url: str, pool_size: int = 4):
    """(sessionmaker, dispose) connecting as the RLS-enforced application role."""
    os.environ["DATABASE_URL"] = db_url
    from tests.pg_rls_support import rls_app_session

    return rls_app_session(db_url, pool_size=pool_size)


def owner_connection(db_url: str):
    import psycopg

    return psycopg.connect(db_url.replace("postgresql+psycopg://", "postgresql://"), autocommit=True)


def load_scenarios(path: str) -> dict:
    with open(path) as fh:
        return json.load(fh)


def run_steps(spec: dict, owner, dispatch: Callable[[dict, int, dict], None], out: str) -> None:
    """Replay ``spec['steps']``: load each job's payload and call ``dispatch(step, job_id, payload)``.

    Whatever a step raises is recorded as ``str(exc)`` (the queue records the
    same text as the attempt failure); the results are written to ``out``.
    """
    results = []
    for step in spec["steps"]:
        job_id = step["job"]
        (text,) = owner.execute("SELECT payload::text FROM jobs WHERE id = %s", (job_id,)).fetchone()
        payload = json.loads(text)
        error = None
        try:
            result = dispatch(step, job_id, payload)
            if asyncio.iscoroutine(result):
                asyncio.run(result)
        except Exception as exc:
            error = str(exc)
        results.append({"job": job_id, "op": step["op"], "error": error})
    with open(out, "w") as fh:
        json.dump(results, fh, indent=1)


def main(run_scenarios: Callable, run_pure: Callable | None = None) -> int:
    args = base_parser().parse_args()
    apply_timezone(args.tz)
    if args.pure:
        assert run_pure is not None, "this runner has no pure mode"
        run_pure(args)
    else:
        run_scenarios(args)
    return 0


if __name__ == "__main__":  # pragma: no cover
    sys.exit("import this module from a runner")
