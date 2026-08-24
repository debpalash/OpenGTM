import asyncio

import pytest

from apps.api.services.job_process_runner import (
    JobProcessError,
    JobProcessTimeout,
    run_job_subprocess,
)


def test_unknown_job_type_fails_in_child_without_payload_in_argv():
    with pytest.raises(JobProcessError, match="child exited"):
        asyncio.run(
            run_job_subprocess(
                123,
                "definitely_not_registered",
                {"secret": "must-travel-via-stdin"},
                timeout=10,
            )
        )


def test_timeout_terminates_child(monkeypatch):
    terminated = asyncio.Event()

    class FakeProcess:
        pid = 999999
        returncode = None

        async def communicate(self, _payload):
            await terminated.wait()

        async def wait(self):
            await terminated.wait()
            self.returncode = -15
            return self.returncode

    fake = FakeProcess()

    async def fake_create(*_args, **_kwargs):
        return fake

    async def fake_terminate(process, grace_seconds=5.0):
        assert process is fake
        terminated.set()
        await process.wait()

    monkeypatch.setattr(asyncio, "create_subprocess_exec", fake_create)
    monkeypatch.setattr(
        "apps.api.services.job_process_runner._terminate_process_tree", fake_terminate
    )

    with pytest.raises(JobProcessTimeout, match="exceeded"):
        asyncio.run(run_job_subprocess(7, "source_workbook", {}, timeout=0.001))
    assert fake.returncode == -15
