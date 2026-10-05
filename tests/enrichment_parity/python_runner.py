"""Run the Python workbook handler for the enrichment parity harness.

Executed in its own process by tests/test_enrichment_go_parity_pg.py:

    uv run python -m tests.enrichment_parity.python_runner \
        --app-url postgresql+psycopg://... --connectors DIR --steps steps.json --out out.json

It drives ``handle_run_workbook`` unchanged (the handler a ``run_workbook`` or
``run_workbook_connector`` job runs in production) against a PostgreSQL
database as a NOSUPERUSER NOBYPASSRLS role, with the queue lease in the payload
exactly as ``QueueService`` injects it. Three things differ from production and
none changes what is stored:

* the killable provider process pool (``provider_runner``) is replaced by a
  thread that calls the same ``_provider_job`` with the same deadline, because
  the pool's worker processes would not inherit these patches;
* ``check_url`` accepts the loopback simulator (it refuses loopback by design);
* Redis broadcasts are disabled so no live service is touched.
"""
from __future__ import annotations

import argparse
import asyncio
import json
import os
import sys
import tempfile
from pathlib import Path


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--app-url", required=True)
    parser.add_argument("--owner-url", required=True)
    parser.add_argument("--connectors", required=True)
    parser.add_argument("--sim-base", required=True)
    parser.add_argument("--steps", required=True)
    parser.add_argument("--out", required=True)
    args = parser.parse_args()

    os.environ["DATABASE_URL"] = args.app_url
    os.environ["DATA_DIR"] = tempfile.mkdtemp(prefix="opengtm-parity-data-")
    os.environ["CONNECTOR_SIGNATURE_POLICY"] = "optional"
    os.environ["REDIS_URL"] = "redis://127.0.0.1:1"  # never reached: broadcasts are disabled below

    import apps.api.services.leadgen.enrichment.declarative.manifest as manifest

    manifest.MANIFESTS_DIR = Path(args.connectors)

    import apps.api.core.url_guard as url_guard

    real_check = url_guard.check_url

    def check_url(url, *a, **kw):
        if url.startswith(args.sim_base):
            return url
        return real_check(url, *a, **kw)

    url_guard.check_url = check_url

    from apps.api.services.workbook import enrichment, provider_runner

    enrichment._make_redis = lambda: None

    async def run_provider(provider_name, lead, timeout):
        task = asyncio.to_thread(provider_runner._provider_job, provider_name, lead.to_dict())
        return await asyncio.wait_for(task, timeout=timeout)

    enrichment.run_provider = run_provider

    import psycopg

    spec = json.load(open(args.steps))
    owner = psycopg.connect(args.owner_url, autocommit=True)
    results = []
    try:
        for step in spec:
            job_id = step["job"]
            text, worker_id, locked_at = owner.execute(
                "SELECT payload::text, worker_id, locked_at FROM jobs WHERE id = %s", (job_id,)).fetchone()
            payload = json.loads(text)
            payload["__queue_lease"] = {"worker_id": worker_id, "locked_at": locked_at.isoformat()}
            error = None
            try:
                asyncio.run(enrichment.handle_run_workbook(job_id, payload))
            except Exception as exc:  # the queue would record str(exc) as the attempt failure
                error = f"{type(exc).__name__}: {exc}"
            results.append({"job": job_id, "error": error})
    finally:
        owner.close()
    json.dump(results, open(args.out, "w"), indent=1)
    return 0


if __name__ == "__main__":
    sys.exit(main())
