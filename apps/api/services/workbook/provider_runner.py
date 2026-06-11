"""Killable, process-isolated provider execution — the core of the scalable
enrichment engine (Track B).

THE PROBLEM this solves: a workbook provider can make a blocking call that
ignores its own timeout — a dead SMTP host, an un-timeout'd socket, a wedged
scraper. On a *thread* that is unrecoverable: Python cannot kill a thread, so the
thread leaks and, run after run, exhausts any pool until the whole enrichment
wedges (we proved this six ways — every in-process isolation just moved the
freeze point).

A *process* can be killed. We run each provider call in a pebble ProcessPool with
a hard per-call timeout. On timeout pebble SIGKILLs the worker and respawns a
fresh one, so a hung provider costs exactly one cell — never the run. This is the
unit that lets enrichment actually scale.
"""
import asyncio
import logging
import multiprocessing as mp
import os
from concurrent.futures import TimeoutError as _FuturesTimeout

from pebble import ProcessPool
from pebble.common import ProcessExpired

logger = logging.getLogger("workbook.provider_runner")

_pool: ProcessPool | None = None


def _get_pool() -> ProcessPool:
    """Lazily create the process pool (so importing this module is cheap and we
    don't spawn workers until the first real run).

    Uses the `forkserver` start method, not `spawn`/`fork`:
      - `spawn` re-imports the parent's __main__ (uvicorn) in every worker — slow
        and error-prone.
      - `fork` from this multi-threaded server process is unsafe on macOS.
      - `forkserver` runs a clean, single-threaded fork source that preloads the
        provider registry once; workers fork from it cheaply and safely.
    """
    global _pool
    if _pool is None:
        workers = int(os.getenv("WORKBOOK_PROVIDER_WORKERS", "8"))
        try:
            # Keep the fork source CLEAN — do NOT preload the providers module.
            # Importing it pulls in libraries that touch macOS Objective-C
            # frameworks, and forking after ObjC init crashes the child. Workers
            # import the registry themselves, after the fork, where it's safe.
            ctx = mp.get_context("forkserver")
        except (ValueError, RuntimeError):
            ctx = mp.get_context()  # platform default fallback
        _pool = ProcessPool(max_workers=workers, context=ctx)
        _pool._wb_workers = workers  # remember size for ensure_workers()
        logger.info(
            f"provider_runner: started ProcessPool ({workers} killable workers, "
            f"start_method={ctx.get_start_method()})"
        )
    return _pool


def ensure_workers(n: int | None) -> None:
    """Make the pool have `n` killable workers (user-configurable from Settings).
    Recreates the pool only when the count actually changes — cheap no-op
    otherwise. Called once at the start of each run."""
    global _pool
    if not n or n <= 0:
        return
    n = max(1, min(64, int(n)))
    os.environ["WORKBOOK_PROVIDER_WORKERS"] = str(n)
    if _pool is not None and getattr(_pool, "_wb_workers", None) != n:
        logger.info(f"provider_runner: resizing pool to {n} workers")
        shutdown()
    _get_pool()


def _provider_job(provider_name: str, lead_dict: dict) -> dict | None:
    """Runs INSIDE a worker process. Importing the providers module auto-registers
    the registry (`_init_providers()` runs on import), so the worker is
    self-contained. Returns a plain dict to avoid cross-process class-identity
    issues on unpickle."""
    from apps.api.services.workbook.providers import get_provider
    from apps.api.services.leadgen.models import Lead

    provider = get_provider(provider_name)
    if provider is None:
        return None
    res = asyncio.run(provider.enrich(Lead.from_dict(lead_dict)))
    return {
        "provider": res.provider,
        "success": bool(res.success),
        "fields": dict(res.fields or {}),
        "confidence": float(res.confidence or 0.0),
        "error": res.error or "",
    }


async def run_provider(provider_name: str, lead, timeout: float) -> dict | None:
    """Run provider.enrich(lead) in a killable subprocess.

    Returns the result dict (keys: provider/success/fields/confidence/error), or
    None if the provider isn't registered. Raises asyncio.TimeoutError if the
    provider exceeded `timeout` (its worker process is killed and replaced).
    """
    pool = _get_pool()
    future = pool.schedule(_provider_job, args=(provider_name, lead.to_dict()), timeout=timeout)
    try:
        return await asyncio.wrap_future(future)
    except _FuturesTimeout:
        # pebble killed the worker at the deadline — surface as a normal timeout.
        raise asyncio.TimeoutError
    except ProcessExpired as e:
        # Worker died (segfault / OOM / killed mid-call). Treat as a failed attempt.
        raise RuntimeError(f"provider worker expired: {e}")


def shutdown() -> None:
    global _pool
    if _pool is not None:
        try:
            _pool.stop()
            _pool.join(timeout=5)
        except Exception:
            pass
        _pool = None
