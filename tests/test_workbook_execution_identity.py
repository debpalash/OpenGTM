import asyncio
import pytest
from apps.api.services.workbook.execution_identity import current_execution, execution_scope


def test_concurrent_runs_and_child_cells_keep_separate_stable_identity():
    async def run(job):
        with execution_scope("workspace", "book", job) as identity:
            async def child():
                await asyncio.sleep(0)
                assert current_execution.get() == identity
                return identity.attempt_key(row_identity="row:1", column_id="email", provider="fixture")
            keys = await asyncio.gather(child(), child())
            assert keys[0] == keys[1]
            return keys[0]
    async def scenario():
        return await asyncio.gather(run(1), run(2), run(1))
    first, second, retry = asyncio.run(scenario())
    assert first == retry and first != second
    assert current_execution.get() is None


def test_direct_runs_do_not_inherit_queue_identity_and_exceptions_restore_scope():
    with execution_scope("workspace", "book", 1) as outer:
        with execution_scope("workspace", "book", None):
            assert current_execution.get() is None
        with pytest.raises(RuntimeError):
            with execution_scope("other", "other-book", 2):
                raise RuntimeError("cancelled")
        assert current_execution.get() == outer
        assert outer.attempt_key(row_identity="row:1", column_id="a", provider="p") != outer.attempt_key(row_identity="lead:1", column_id="a", provider="p")
    assert current_execution.get() is None
