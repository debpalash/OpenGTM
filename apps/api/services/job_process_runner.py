"""Killable subprocess boundary for durable job handlers."""

from __future__ import annotations

import asyncio
import json
import os
import signal
import sys
from typing import Any


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
) -> None:
    """Run one registered handler in an independently killable Python process."""
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

    try:
        await asyncio.wait_for(asyncio.shield(communication), timeout=timeout)
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

    if process.returncode != 0:
        raise JobProcessError(
            f"job {job_id} ({job_type}) child exited with code {process.returncode}"
        )
