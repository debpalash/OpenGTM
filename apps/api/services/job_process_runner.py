"""Killable subprocess boundary for durable job handlers."""

from __future__ import annotations

import asyncio
import json
import os
import signal
import sys
from typing import Any, Callable


class JobProcessError(RuntimeError):
    """A job child exited without completing successfully."""


class JobProcessTimeout(JobProcessError):
    """A job child exceeded its wall-clock deadline and was terminated."""


async def _terminate_process_tree(
    process: asyncio.subprocess.Process, grace_seconds: float = 5.0
) -> None:
    """Terminate the child's process group, escalating to SIGKILL if needed."""
    if process.returncode is not None:
        return

    try:
        os.killpg(process.pid, signal.SIGTERM)
    except (ProcessLookupError, PermissionError):
        try:
            process.terminate()
        except ProcessLookupError:
            return

    try:
        await asyncio.wait_for(process.wait(), timeout=grace_seconds)
        return
    except asyncio.TimeoutError:
        pass

    try:
        os.killpg(process.pid, signal.SIGKILL)
    except (ProcessLookupError, PermissionError):
        try:
            process.kill()
        except ProcessLookupError:
            return
    await process.wait()


async def run_job_subprocess(
    job_id: int,
    job_type: str,
    payload: dict[str, Any],
    *,
    timeout: float,
    should_continue: Callable[[], bool] | None = None,
    poll_interval: float = 1.0,
) -> None:
    """Run one registered handler in an independently killable Python process."""
    if should_continue and not await asyncio.to_thread(should_continue):
        raise JobProcessError(f"job {job_id} ({job_type}) claim cancelled or lost")
    process = await asyncio.create_subprocess_exec(
        sys.executable,
        "-m",
        "apps.api.job_process",
        str(job_id),
        job_type,
        stdin=asyncio.subprocess.PIPE,
        # Keep normal logs attached to the worker/container. Payloads (which may
        # contain secrets) travel over stdin and never appear in process args.
        stdout=None,
        stderr=None,
        start_new_session=True,
    )
    encoded_payload = json.dumps(payload, separators=(",", ":")).encode("utf-8")
    communication = asyncio.create_task(process.communicate(encoded_payload))

    async def monitor_claim() -> None:
        while True:
            if not await asyncio.to_thread(should_continue):
                raise JobProcessError(f"job {job_id} ({job_type}) claim cancelled or lost")
            await asyncio.sleep(poll_interval)

    monitor = asyncio.create_task(monitor_claim()) if should_continue else None

    try:
        waiting = {communication}
        if monitor:
            waiting.add(monitor)
        done, _ = await asyncio.wait(waiting, timeout=timeout, return_when=asyncio.FIRST_COMPLETED)
        if not done:
            raise asyncio.TimeoutError
        if monitor and monitor in done:
            await monitor
        await communication
    except asyncio.TimeoutError as exc:
        await _terminate_process_tree(process)
        await asyncio.gather(communication, return_exceptions=True)
        raise JobProcessTimeout(
            f"job {job_id} ({job_type}) exceeded {timeout:g}s"
        ) from exc
    except asyncio.CancelledError:
        # Worker shutdown/redeploy must not orphan a child that can keep making
        # external writes after its lease is reclaimed by another replica.
        await _terminate_process_tree(process)
        await asyncio.gather(communication, return_exceptions=True)
        raise
    except Exception:
        await _terminate_process_tree(process)
        await asyncio.gather(communication, return_exceptions=True)
        raise
    finally:
        if monitor:
            monitor.cancel()
            await asyncio.gather(monitor, return_exceptions=True)

    if process.returncode != 0:
        raise JobProcessError(
            f"job {job_id} ({job_type}) child exited with code {process.returncode}"
        )
