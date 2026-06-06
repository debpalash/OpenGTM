"""
BullMQ Worker — Processes workbook enrichment jobs from Redis.

This runs as a separate process (or Docker container) and picks up jobs
enqueued by the Workbook API's /run endpoint.

Job types:
  - enrich_cell: Enrich a single cell (col_id + row_id + provider_chain)
  - enrich_row: Enrich all enrichment columns for a row (creates child jobs)

Run:
  python -m apps.api.services.workbook.worker

Or via Docker:
  command: python -m apps.api.services.workbook.worker
"""

import asyncio
import json
import logging
import os
import signal
import sys

# Add project root to path for imports
sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", "..", "..")))

from apps.api.database import SessionLocal
from apps.api.services.workbook.enrichment import enrich_cell
from apps.api.services.workbook.models import Workbook, WorkbookRow

logger = logging.getLogger("workbook.worker")
logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(name)s] %(levelname)s: %(message)s")


# ── Redis config ──────────────────────────────────────────────────────────

REDIS_URL = os.getenv("REDIS_URL", "redis://localhost:6379")


# ── Job Processor ─────────────────────────────────────────────────────────

async def process_enrich_cell(job_data: dict, redis_client=None) -> dict:
    """Process a single cell enrichment job.

    Expected job_data:
        {
            "workbook_id": "abc-123",
            "row_id": 42,
            "col_id": "email",
            "provider_chain": ["mailscout", "crosslinked", "ddg_search"],
        }
    """
    workbook_id = job_data.get("workbook_id")
    row_id = job_data.get("row_id")
    col_id = job_data.get("col_id")

    if not all([workbook_id is not None, row_id is not None, col_id]):
        return {"success": False, "error": "missing_fields"}

    # Create a new DB session for this job
    db = SessionLocal()
    try:
        # ── Reconstruct enrich_cell args from the workbook definition ──
        # The job payload only carries identifiers; enrich_cell needs the
        # full column config, the row's lead data, and the columns list.
        wb = db.query(Workbook).filter(Workbook.id == workbook_id).first()
        if not wb:
            return {"success": False, "error": "workbook_not_found"}

        columns_config = wb.columns_config or []
        col_config = next((c for c in columns_config if c.get("id") == col_id), None)
        if not col_config:
            return {"success": False, "error": f"column_not_found:{col_id}"}

        lead_data = _load_row_data(db, workbook_id, row_id)
        if lead_data is None:
            return {"success": False, "error": f"row_not_found:{row_id}"}

        result = await enrich_cell(
            db=db,
            workbook_id=workbook_id,
            lead_id=lead_data["id"],
            col_id=col_id,
            col_config=col_config,
            lead_data=lead_data,
            columns_config=columns_config,
            redis_client=redis_client,
        )
        return result
    except Exception as e:
        logger.error(f"Job failed: workbook={workbook_id} row={row_id} col={col_id} error={e}")
        return {"success": False, "error": str(e)[:200]}
    finally:
        db.close()


def _load_row_data(db, workbook_id: str, row_id: int) -> dict | None:
    """Resolve the row's data dict for enrichment.

    `row_id` is the lead id used by the API (WorkbookRow.lead_id, or the row's
    own id when there is no backing lead — see routers/workbooks.py run_workbook).
    Mirrors the inline path's `{"id": ..., **row.data}` shape.
    """
    # v2: WorkbookRow store
    row = (
        db.query(WorkbookRow)
        .filter(
            WorkbookRow.workbook_id == workbook_id,
            (WorkbookRow.lead_id == row_id) | (WorkbookRow.id == row_id),
        )
        .first()
    )
    if row is not None:
        return {"id": row.lead_id or row.id, **(row.data or {})}

    # v1 legacy: read straight from the leads DB
    try:
        from apps.api.services.leadgen.db import LeadDB

        lead_db = LeadDB()
        try:
            lead = lead_db.get_lead(row_id)
        finally:
            lead_db.close()
        if lead is not None:
            return lead.to_dict()
    except Exception as e:
        logger.warning(f"Failed to load v1 lead {row_id}: {e}")
    return None


# ── Worker Runner ─────────────────────────────────────────────────────────

async def run_worker():
    """Run the BullMQ worker process.

    This attempts to use BullMQ Python if available, otherwise falls back
    to a simple Redis-based polling worker for development.
    """
    logger.info("Starting Yupcha Workbook Worker...")
    logger.info(f"Redis: {REDIS_URL}")

    try:
        # Try BullMQ Python SDK
        from bullmq import Worker as BullMQWorker

        async def processor(job, token):
            """BullMQ job processor."""
            logger.info(f"Processing job: {job.name} (id={job.id})")

            import redis.asyncio as aioredis
            r = aioredis.from_url(REDIS_URL)

            try:
                result = await process_enrich_cell(job.data, redis_client=r)

                # Report progress
                if hasattr(job, 'updateProgress'):
                    await job.updateProgress({
                        "status": "complete" if result.get("success") else "error",
                        "provider": result.get("provider"),
                    })

                return result
            finally:
                await r.close()

        worker = BullMQWorker(
            "enrichment",
            processor,
            {
                "connection": REDIS_URL,
                "concurrency": int(os.getenv("WORKER_CONCURRENCY", "10")),
            },
        )

        logger.info("✓ BullMQ Worker started (enrichment queue)")
        logger.info(f"  Concurrency: {os.getenv('WORKER_CONCURRENCY', '10')}")

        # Keep running until shutdown
        shutdown_event = asyncio.Event()

        def signal_handler(sig, frame):
            logger.info(f"Received signal {sig}, shutting down...")
            shutdown_event.set()

        signal.signal(signal.SIGTERM, signal_handler)
        signal.signal(signal.SIGINT, signal_handler)

        await shutdown_event.wait()

        logger.info("Closing worker...")
        await worker.close()
        logger.info("✓ Worker shut down cleanly")

    except ImportError:
        # BullMQ not installed — use fallback Redis polling worker
        logger.warning("BullMQ not installed. Using fallback Redis polling worker.")
        logger.warning("Install BullMQ: pip install bullmq")
        await _run_fallback_worker()


async def _run_fallback_worker():
    """Fallback worker that polls Redis lists directly (dev mode).

    When BullMQ Python SDK is not installed, this provides basic
    job processing using raw Redis BRPOP.
    """
    import redis.asyncio as aioredis

    r = aioredis.from_url(REDIS_URL, decode_responses=True)

    logger.info("✓ Fallback Redis worker started (BRPOP mode)")
    logger.info("  Queue: yupcha:enrichment:jobs")

    shutdown = False

    def handle_signal(sig, frame):
        nonlocal shutdown
        shutdown = True

    signal.signal(signal.SIGTERM, handle_signal)
    signal.signal(signal.SIGINT, handle_signal)

    while not shutdown:
        try:
            # Block-pop from the job queue
            result = await r.brpop("yupcha:enrichment:jobs", timeout=5)
            if result is None:
                continue

            _, job_json = result
            job_data = json.loads(job_json)

            logger.info(f"Processing job: {job_data.get('col_id')} for row {job_data.get('row_id')}")

            outcome = await process_enrich_cell(job_data, redis_client=r)
            logger.info(f"  Result: {'✓' if outcome.get('success') else '✗'} {outcome.get('provider', outcome.get('error', ''))}")

        except asyncio.CancelledError:
            break
        except Exception as e:
            logger.error(f"Worker error: {e}")
            await asyncio.sleep(1)

    await r.close()
    logger.info("✓ Fallback worker shut down")


# ── Enqueue Helper (called from API) ──────────────────────────────────────

async def enqueue_enrichment_job(
    workbook_id: str,
    row_id: int,
    col_id: str,
    provider_chain: list,
    redis_url: str = None,
) -> bool:
    """Enqueue an enrichment job for the worker to process.

    Tries BullMQ first, falls back to raw Redis LPUSH.
    """
    url = redis_url or REDIS_URL

    job_data = {
        "workbook_id": workbook_id,
        "row_id": row_id,
        "col_id": col_id,
        "provider_chain": provider_chain,
    }

    try:
        # Try BullMQ
        from bullmq import Queue
        queue = Queue("enrichment", {"connection": url})
        await queue.add(
            f"cell-{row_id}-{col_id}",
            job_data,
            {
                "attempts": 3,
                "backoff": {"type": "exponential", "delay": 2000},
            },
        )
        await queue.close()
        return True

    except ImportError:
        # Fallback: raw Redis LPUSH
        import redis.asyncio as aioredis
        r = aioredis.from_url(url, decode_responses=True)
        await r.lpush("yupcha:enrichment:jobs", json.dumps(job_data))
        await r.close()
        return True

    except Exception as e:
        logger.error(f"Failed to enqueue job: {e}")
        return False


# ── Main ──────────────────────────────────────────────────────────────────

if __name__ == "__main__":
    asyncio.run(run_worker())
