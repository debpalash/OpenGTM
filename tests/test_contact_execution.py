"""Spend boundaries across concurrent requests, retries and lost processes."""

import asyncio

import pytest
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker

from apps.api.services.leadgen.contact_execution import ContactExecution, execute_contact_once


@pytest.fixture
def factory(tmp_path):
    engine = create_engine(f"sqlite:///{tmp_path}/claims.db")
    ContactExecution.__table__.create(engine)
    yield sessionmaker(bind=engine)
    engine.dispose()


def test_simultaneous_requests_and_restart_replay_spend_once(factory):
    calls = []

    async def scenario():
        started, release = asyncio.Event(), asyncio.Event()

        async def provider():
            calls.append("billed")
            started.set()
            await release.wait()
            return {"ok": True, "people": [{"person_id": "p1"}]}

        args = dict(workspace_id="W1", action_id="A1", contract={"people": ["p1"]},
                    operation=provider, session_factory=factory)
        first = asyncio.create_task(execute_contact_once(**args))
        await started.wait()
        pending = await execute_contact_once(**args)
        assert pending["execution_status"] == "running"
        assert pending["automatic_retry_allowed"] is False
        release.set()
        assert (await first)["execution_status"] == "completed"
        # No process memory/cache participates in replay; use a new session factory.
        args["session_factory"] = sessionmaker(bind=factory.kw["bind"])
        replay = await execute_contact_once(**args)
        assert replay["reused"] is True
        assert replay["people"] == [{"person_id": "p1"}]

    asyncio.run(scenario())
    assert calls == ["billed"]


def test_changed_contract_rejected_and_tenants_isolated(factory):
    calls = []

    async def provider():
        calls.append("billed")
        return {"ok": True}

    async def scenario():
        args = dict(workspace_id="W1", action_id="A1", contract={"domain": "stripe.com"},
                    operation=provider, session_factory=factory)
        await execute_contact_once(**args)
        conflict = await execute_contact_once(**{**args, "contract": {"domain": "paypal.com"}})
        assert conflict["execution_status"] == "conflict"
        other = await execute_contact_once(**{**args, "workspace_id": "W2"})
        assert other["execution_status"] == "completed"

    asyncio.run(scenario())
    assert len(calls) == 2


def test_abandoned_claim_expires_without_rebilling(factory):
    async def provider():
        raise AssertionError("A possibly billed request must never be replayed")

    async def scenario():
        # Simulate process death after insert, by cancelling its active operation.
        started = asyncio.Event()

        async def interrupted():
            started.set()
            await asyncio.Event().wait()

        args = dict(workspace_id="W1", action_id="A1", contract={}, session_factory=factory)
        task = asyncio.create_task(execute_contact_once(**args, operation=interrupted))
        await started.wait()
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task
        # Hard death skips exception cleanup: recreate that persisted state.
        with factory() as db:
            db.query(ContactExecution).update({"status": "running", "expires_at": 0})
            db.commit()
        result = await execute_contact_once(**args, operation=provider)
        assert result["execution_status"] == "uncertain"
        assert result["automatic_retry_allowed"] is False
        assert "usage" in result["recovery"]
        assert (await execute_contact_once(**args, operation=provider))["execution_status"] == "uncertain"

    asyncio.run(scenario())
    with factory() as db:
        assert db.query(ContactExecution).one().status == "uncertain"


def test_timeout_is_bounded_and_failed_results_are_replayed(factory):
    calls = []

    async def stuck():
        calls.append("possibly_billed")
        await asyncio.Event().wait()

    async def failed():
        calls.append("failed")
        return {"ok": False, "error": "provider_unavailable", "people": []}

    async def scenario():
        args = dict(workspace_id="W1", action_id="A1", contract={}, session_factory=factory)
        uncertain = await execute_contact_once(**args, operation=stuck, timeout_seconds=0.01)
        assert uncertain["execution_status"] == "uncertain"
        await execute_contact_once(**args, operation=stuck)
        args["action_id"] = "A2"
        result = await execute_contact_once(**args, operation=failed)
        replay = await execute_contact_once(**args, operation=failed)
        assert result["execution_status"] == "failed"
        assert replay["error"] == "provider_unavailable"
        assert replay["reused"] is True

    asyncio.run(scenario())
    assert calls == ["possibly_billed", "failed"]
