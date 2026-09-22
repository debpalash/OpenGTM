"""Reservation storage constraints; no provider calls or application database."""
import importlib
import uuid

import pytest
from alembic.migration import MigrationContext
from alembic.operations import Operations
from sqlalchemy import create_engine, inspect
from sqlalchemy.exc import IntegrityError

from apps.api.services.workbook.spend_models import WorkbookSpendAttempt
from apps.api.services.workbook.spend_service import transition_attempt
from sqlalchemy.orm import sessionmaker
from apps.api.services.workbook.models import Workbook
from apps.api.services.workbook.spend_service import reserve_attempt, settle_attempt
from apps.api.services.workbook.spend_service import execute_reserved_attempt


def test_migration_constraints_and_roundtrip(monkeypatch):
    migration = importlib.import_module("migrations.versions.1d2e3f405162_workbook_spend_attempts")
    engine = create_engine("sqlite:///:memory:")
    with engine.begin() as connection:
        monkeypatch.setattr(migration, "op", Operations(MigrationContext.configure(connection)))
        migration.upgrade()
        table = WorkbookSpendAttempt.__table__
        assert {c["name"] for c in inspect(connection).get_columns(table.name)} == set(table.columns.keys())
        record = dict(id=str(uuid.uuid4()), workspace_id="one", workbook_id="book", run_id="run",
                      row_identity="row:1", column_id="email", provider="fixture", attempt_key="attempt",
                      contract_hash="a" * 64, reserved_microusd=20000, cost_basis={"kind": "catalog_estimate"},
                      status="reserved", created_at=1, updated_at=1)
        connection.execute(table.insert().values(**record))
        for overrides in [{}, {"attempt_key": "negative", "reserved_microusd": -1},
                          {"attempt_key": "invalid", "status": "free"}]:
            with pytest.raises(IntegrityError), connection.begin_nested():
                connection.execute(table.insert().values(**{**record, "id": str(uuid.uuid4()), **overrides}))
        connection.execute(table.insert().values(**{**record, "id": str(uuid.uuid4()), "workspace_id": "two"}))
        assert len(connection.execute(table.select()).all()) == 2
        migration.downgrade()
        assert table.name not in inspect(connection).get_table_names()
    engine.dispose()


@pytest.fixture
def attempts(tmp_path):
    engine = create_engine(f"sqlite:///{tmp_path}/spend.db")
    WorkbookSpendAttempt.__table__.create(engine)
    factory = sessionmaker(bind=engine, autoflush=False)
    with factory() as db:
        db.add(WorkbookSpendAttempt(id="attempt", workspace_id="one", workbook_id="book",
            run_id="run", row_identity="row:1", column_id="email", provider="fixture",
            attempt_key="key", contract_hash="a" * 64, reserved_microusd=20000,
            cost_basis={"kind": "catalog_estimate"}, status="reserved", created_at=1, updated_at=1))
        db.commit()
    yield factory
    engine.dispose()


@pytest.mark.parametrize("state", ["active", "cancelled", "replacement", "wrong_workspace", "wrong_run"])
def test_spend_dispatch_requires_active_queue_owner(attempts, state):
    from datetime import datetime, timezone
    from apps.api.models import Job
    from apps.api.services.workbook.batch_attempts import batch_lease_scope
    locked = datetime.now(timezone.utc)
    with attempts() as db:
        Job.__table__.create(db.get_bind())
        db.get(WorkbookSpendAttempt, "attempt").run_id = "job:2" if state == "wrong_run" else "job:1"
        db.add(Job(id=1, type="run_workbook", workspace_id="one",
                   status="cancelled" if state == "cancelled" else "processing",
                   worker_id="replacement" if state == "replacement" else "owner",
                   locked_at=locked, payload={"workbook_id": "book"}))
        db.commit()
    payload = {"workspace_id": "other" if state == "wrong_workspace" else "one", "workbook_id": "book",
               "__queue_lease": {"worker_id": "owner", "locked_at": locked.isoformat()}}
    with batch_lease_scope(1, payload):
        allowed = transition_attempt(session_factory=attempts, workspace_id="one", workbook_id="book",
            attempt_id="attempt", contract_hash="a" * 64, action="dispatch")
    assert allowed is (state == "active")
    with attempts() as db:
        receipt = db.get(WorkbookSpendAttempt, "attempt")
        assert receipt.status == ("dispatched" if allowed else "reserved")
        assert receipt.reserved_microusd == 20000


def test_competing_dispatchers_only_one_can_dispatch(attempts):
    from concurrent.futures import ThreadPoolExecutor
    from threading import Barrier
    barrier = Barrier(2)
    def dispatch():
        barrier.wait(timeout=5)
        return transition_attempt(session_factory=attempts, workspace_id="one", workbook_id="book",
            attempt_id="attempt", contract_hash="a" * 64, action="dispatch")
    with ThreadPoolExecutor(max_workers=2) as pool:
        results = [future.result() for future in [pool.submit(dispatch), pool.submit(dispatch)]]
    assert sorted(results) == [False, True]
    args = dict(session_factory=attempts, workspace_id="one", workbook_id="book",
                attempt_id="attempt", contract_hash="a" * 64)
    assert not transition_attempt(**args, action="release_before_dispatch")
    assert transition_attempt(**args, action="mark_uncertain")
    assert not transition_attempt(**args, action="dispatch")
    assert not transition_attempt(**args, action="release_before_dispatch")
    with attempts() as db:
        receipt = db.get(WorkbookSpendAttempt, "attempt")
        assert receipt.status == "uncertain"
        assert receipt.reserved_microusd == 20000
        assert receipt.cost_basis == {"kind": "catalog_estimate"}


def test_scoped_contract_guards_and_pre_dispatch_cancel(attempts):
    args = dict(session_factory=attempts, workspace_id="one", workbook_id="book",
                attempt_id="attempt", contract_hash="a" * 64)
    for override in [{"workspace_id": "two"}, {"workbook_id": "other"}, {"contract_hash": "b" * 64}]:
        assert not transition_attempt(**{**args, **override}, action="dispatch")
    assert transition_attempt(**args, action="release_before_dispatch")
    assert not transition_attempt(**args, action="dispatch")
    assert not transition_attempt(**args, action="release_before_dispatch")
    with pytest.raises(ValueError):
        transition_attempt(**args, action="settle")


def test_reservations_compete_for_last_allowance_and_replay(attempts):
    from concurrent.futures import ThreadPoolExecutor
    from threading import Barrier
    with attempts() as db:
        Workbook.__table__.create(db.get_bind())
        db.add(Workbook(id="budget-book", workspace_id="one", name="Budget", budget_max_usd=0.02, budget_spent_usd=0))
        db.commit()
    args = dict(session_factory=attempts, workspace_id="one", workbook_id="budget-book", run_id="run",
                column_id="email", provider="fixture", exposure_microusd=20000,
                cell_limit_microusd=20000, cost_basis={"kind": "catalog_estimate"},
                operation_contract={"inputs": {"company": "Acme"}, "target": "email"})
    barrier = Barrier(2)
    def reserve(index):
        barrier.wait(timeout=5)
        return reserve_attempt(**args, row_identity=f"row:{index}", attempt_key=f"key:{index}")
    with ThreadPoolExecutor(max_workers=2) as pool:
        results = [future.result() for future in [pool.submit(reserve, 0), pool.submit(reserve, 1)]]
    winner = next(index for index, result in enumerate(results) if result["ok"])
    assert results[1 - winner] == {"ok": False, "reason": "workbook_budget"}
    replay_args = dict(args, row_identity=f"row:{winner}", attempt_key=f"key:{winner}")
    replay = reserve_attempt(**replay_args)
    assert replay == {**results[winner], "reused": True}
    assert reserve_attempt(**{**replay_args, "exposure_microusd": 10000})["reason"] == "contract_conflict"
    assert reserve_attempt(**{**replay_args, "operation_contract": {"inputs": {"company": "Different"}, "target": "email"}})["reason"] == "contract_conflict"
    assert reserve_attempt(**{**replay_args, "workspace_id": "foreign"})["reason"] == "workbook_not_found"
    assert reserve_attempt(**{**replay_args, "attempt_key": "next"})["reason"] == "cell_budget"
    transition_args = dict(session_factory=attempts, workspace_id="one", workbook_id="budget-book",
        attempt_id=replay["id"], contract_hash=replay["contract_hash"])
    assert transition_attempt(**transition_args, action="dispatch")
    assert transition_attempt(**transition_args, action="mark_uncertain")
    assert reserve_attempt(**args, row_identity="row:new", attempt_key="new")["reason"] == "workbook_budget"


@pytest.mark.parametrize("charge", [0, 10000, 30000])
def test_settlement_is_atomic_idempotent_and_preserves_overruns(attempts, charge):
    from concurrent.futures import ThreadPoolExecutor
    with attempts() as db:
        Workbook.__table__.create(db.get_bind())
        db.add(Workbook(id="book", workspace_id="one", name="Budget", budget_max_usd=0.02, budget_spent_usd=0))
        db.commit()
    args = dict(session_factory=attempts, workspace_id="one", workbook_id="book",
                attempt_id="attempt", contract_hash="a" * 64)
    settlement = dict(args, charged_microusd=charge, accounting_basis="catalog_estimate", result={"success": True})
    assert settle_attempt(**settlement)["reason"] == "invalid_state"
    assert transition_attempt(**args, action="dispatch")
    with ThreadPoolExecutor(max_workers=2) as pool:
        results = [future.result() for future in [pool.submit(settle_attempt, **settlement), pool.submit(settle_attempt, **settlement)]]
    assert all(result["ok"] for result in results)
    assert sorted(result["reused"] for result in results) == [False, True]
    assert settle_attempt(**{**settlement, "charged_microusd": charge + 1})["reason"] == "settlement_conflict"
    with attempts() as db:
        assert db.get(Workbook, "book").budget_spent_usd == pytest.approx(charge / 1000000)
        receipt = db.get(WorkbookSpendAttempt, "attempt")
        assert receipt.settled_microusd == charge
        assert receipt.reserved_microusd == 20000
        assert receipt.status == "settled"


@pytest.mark.parametrize("failure", [None, "timeout", "cancel", "missing_accounting"])
def test_reserved_execution_replays_or_blocks_uncertain_calls(attempts, failure):
    import asyncio
    with attempts() as db:
        Workbook.__table__.create(db.get_bind())
        db.add(Workbook(id="execution", workspace_id="one", name="Execution", budget_max_usd=0.02, budget_spent_usd=0))
        db.commit()
    reservation = dict(session_factory=attempts, workspace_id="one", workbook_id="execution", run_id="run",
        row_identity="row:1", column_id="email", provider="fixture", attempt_key="execution-key",
        exposure_microusd=20000, cell_limit_microusd=20000, cost_basis={"kind": "catalog_estimate"},
        operation_contract={"inputs": {"company": "Acme"}, "instructions": "Find partnerships"})
    calls = []
    async def provider():
        calls.append("called")
        # A second writer must be possible while provider work is in progress.
        with attempts() as db:
            db.query(Workbook).filter_by(id="execution").update({"name": "Still responsive"})
            db.commit()
        if failure == "timeout":
            raise TimeoutError("Response lost")
        if failure == "cancel":
            raise asyncio.CancelledError()
        if failure == "missing_accounting":
            return {"result": {"success": False}}
        return {"charged_microusd": 10000, "accounting_basis": "catalog_estimate", "result": {"value": "fixture"}}
    if failure:
        with pytest.raises((TimeoutError, asyncio.CancelledError, KeyError)):
            asyncio.run(execute_reserved_attempt(operation=provider, reservation=reservation))
    else:
        first = asyncio.run(execute_reserved_attempt(operation=provider, reservation=reservation))
        assert first["ok"] and not first["reused"]
    retry = asyncio.run(execute_reserved_attempt(operation=provider, reservation=reservation))
    changed = asyncio.run(execute_reserved_attempt(operation=provider, reservation={**reservation,
        "operation_contract": {"inputs": {"company": "Acme"}, "instructions": "Find engineering"}}))
    assert changed == {"ok": False, "reason": "contract_conflict"}
    assert calls == ["called"]
    if failure:
        assert retry["reason"] == "attempt_not_dispatchable"
        assert not retry["automatic_retry_allowed"]
    else:
        assert retry["reused"] and retry["result"] == {"value": "fixture"}
    with attempts() as db:
        receipt = db.query(WorkbookSpendAttempt).filter_by(workbook_id="execution").one()
        assert receipt.status == ("uncertain" if failure else "settled")
        assert db.get(Workbook, "execution").budget_spent_usd == (0 if failure else 0.01)


@pytest.mark.parametrize("success", [True, False])
def test_queued_agent_uses_reservations_without_duplicate_provider_or_debit(attempts, monkeypatch, success):
    import asyncio
    from types import SimpleNamespace
    from apps.api.services.workbook import agent_column
    from apps.api.services.workbook.execution_identity import execution_scope
    from apps.api.services.workbook.planner_models import ProviderStat
    with attempts() as db:
        Workbook.__table__.create(db.get_bind())
        ProviderStat.__table__.create(db.get_bind())
        db.add(Workbook(id="agent-book", workspace_id="one", name="Agent", budget_max_usd=0.02, budget_spent_usd=0))
        db.commit()
    calls = []
    async def runner(name, lead, timeout):
        calls.append(name)
        return {"provider": name, "success": success, "fields": {"email": "fixture@example.com"} if success else {}, "confidence": 0.9}
    monkeypatch.setattr(agent_column, "SessionLocal", attempts)
    monkeypatch.setattr(agent_column, "run_provider", runner)
    monkeypatch.setattr(agent_column, "get_provider", lambda name: SimpleNamespace(default_confidence=0.9))
    monkeypatch.setattr(agent_column._planner, "provider_cost", lambda name: 0.02)
    monkeypatch.setattr(agent_column._planner, "is_paid", lambda name: True)
    monkeypatch.setattr(agent_column, "_save_trace", lambda *args: None)
    async def run():
        with execution_scope("one", "agent-book", 123), attempts() as db:
            result = await agent_column.run_agent_cell(db, "agent-book", 1,
                {"id": "email", "tools": ["fixture"]}, {"id": 1, "company": "Acme", "__row_id": 1})
            db.commit()
            return result
    first, retry = asyncio.run(run()), asyncio.run(run())
    assert calls == ["fixture"]
    if success:
        assert first["value"] == retry["value"] == "fixture@example.com"
    else:
        assert first["error"] == "agent_accounting_uncertain"
        assert retry["error"] == "agent_attempt_not_dispatchable"
    with attempts() as db:
        assert db.get(Workbook, "agent-book").budget_spent_usd == (0.02 if success else 0)
        receipt = db.query(WorkbookSpendAttempt).filter_by(workbook_id="agent-book").one()
        assert receipt.row_identity == "row:1"
        assert receipt.status == ("settled" if success else "uncertain")


@pytest.mark.parametrize("price", [None, -1, float("nan"), float("inf"), "bad"])
def test_queued_agent_refuses_unknown_price_before_provider_call(attempts, monkeypatch, price):
    import asyncio
    from types import SimpleNamespace
    from apps.api.services.workbook import agent_column, providers
    from apps.api.services.workbook.execution_identity import execution_scope
    from apps.api.services.workbook.planner_models import ProviderStat
    with attempts() as db:
        Workbook.__table__.create(db.get_bind())
        ProviderStat.__table__.create(db.get_bind())
        db.add(Workbook(id="unknown-book", workspace_id="one", name="Unknown", budget_max_usd=1))
        db.commit()
    provider = SimpleNamespace(default_confidence=0.9, cost_per_lookup=price)
    monkeypatch.setattr(providers, "get_provider", lambda name: provider)
    monkeypatch.setattr(agent_column, "get_provider", lambda name: provider)
    monkeypatch.setattr(agent_column, "_save_trace", lambda *args: None)
    async def forbidden(*args, **kwargs):
        pytest.fail("Unknown-price provider must not execute")
    monkeypatch.setattr(agent_column, "run_provider", forbidden)
    async def run():
        with execution_scope("one", "unknown-book", 123), attempts() as db:
            return await agent_column.run_agent_cell(db, "unknown-book", 1,
                {"id": "email", "tools": ["unknown_fixture"]}, {"id": 1, "company": "Example", "__row_id": 1})
    result = asyncio.run(run())
    assert result["error"] == "agent_provider_price_unknown"
    with attempts() as db:
        assert db.query(WorkbookSpendAttempt).filter_by(workbook_id="unknown-book").count() == 0
        assert (db.get(Workbook, "unknown-book").budget_spent_usd or 0) == 0


def test_concurrent_queued_agent_cells_share_last_lookup_allowance(attempts, monkeypatch):
    import asyncio
    from types import SimpleNamespace
    from apps.api.services.workbook import agent_column
    from apps.api.services.workbook.execution_identity import execution_scope
    from apps.api.services.workbook.planner_models import ProviderStat
    with attempts() as db:
        Workbook.__table__.create(db.get_bind())
        ProviderStat.__table__.create(db.get_bind())
        db.add(Workbook(id="shared", workspace_id="one", name="Shared", budget_max_usd=0.02, budget_spent_usd=0))
        db.commit()
    calls = []
    async def runner(name, lead, timeout):
        calls.append(lead.id)
        # Yield while exposure is reserved but not yet settled, allowing the
        # second cell to observe precisely the oversubscription danger window.
        await asyncio.sleep(0)
        return {"provider": name, "success": True, "fields": {"email": "fixture@example.com"}, "confidence": 0.9}
    monkeypatch.setattr(agent_column, "SessionLocal", attempts)
    monkeypatch.setattr(agent_column, "run_provider", runner)
    monkeypatch.setattr(agent_column, "get_provider", lambda name: SimpleNamespace(default_confidence=0.9))
    monkeypatch.setattr(agent_column._planner, "provider_cost", lambda name: 0.02)
    monkeypatch.setattr(agent_column._planner, "is_paid", lambda name: True)
    monkeypatch.setattr(agent_column, "_save_trace", lambda *args: None)
    async def cell(row):
        with attempts() as db:
            result = await agent_column.run_agent_cell(db, "shared", row,
                {"id": "email", "tools": ["fixture"]}, {"id": row, "company": "Acme", "__row_id": row})
            db.commit()
            return result
    async def run():
        with execution_scope("one", "shared", 456):
            return await asyncio.gather(cell(1), cell(2))
    results = asyncio.run(run())
    assert len(calls) == 1
    assert sum(result["value"] is not None for result in results) == 1
    assert [r["error"] for r in results if r["error"]] == ["agent_workbook_budget"]
    with attempts() as db:
        assert db.get(Workbook, "shared").budget_spent_usd == 0.02
        receipts = db.query(WorkbookSpendAttempt).filter_by(workbook_id="shared").all()
        assert len(receipts) == 1 and receipts[0].status == "settled"


@pytest.mark.parametrize("first_kind", ["agent", "waterfall"])
def test_agent_and_waterfall_share_reservations(attempts, monkeypatch, first_kind):
    import asyncio
    from types import SimpleNamespace
    from apps.api.services.workbook import agent_column, enrichment, planner
    from apps.api.services.workbook.execution_identity import execution_scope
    from apps.api.services.workbook.models import WorkbookRow, WorkbookEnrichment
    from apps.api.services.workbook.planner_models import ProviderStat
    from apps.api.core.tenancy import workspace_scope
    with attempts() as db:
        for table in [Workbook.__table__, WorkbookRow.__table__, WorkbookEnrichment.__table__, ProviderStat.__table__]:
            table.create(db.get_bind())
        db.add(Workbook(id="mixed", workspace_id="one", name="Mixed", budget_max_usd=0.02, budget_spent_usd=0))
        db.add(WorkbookRow(id=1, workbook_id="mixed", workspace_id="one", position=0, data={"company": "Acme"}, enrichments={}))
        db.commit()
    calls = []
    async def runner(name, lead, timeout):
        calls.append(name)
        await asyncio.sleep(0)
        return {"provider": name, "success": True, "fields": {"email": "fixture@example.com"}, "confidence": 0.9}
    for module in [agent_column, enrichment]:
        monkeypatch.setattr(module, "SessionLocal", attempts)
        monkeypatch.setattr(module, "run_provider", runner)
        monkeypatch.setattr(module, "get_provider", lambda name: SimpleNamespace(default_confidence=0.9))
    monkeypatch.setattr(agent_column, "_save_trace", lambda *args: None)
    monkeypatch.setattr(planner, "provider_cost", lambda name: 0.02)
    monkeypatch.setattr(planner, "is_paid", lambda name: True)
    async def cell(kind):
        with attempts() as db:
            data = {"id": 1, "company": "Acme", "__row_id": 1, "__lead_id": None}
            if kind == "agent":
                result = await agent_column.run_agent_cell(db, "mixed", 1,
                    {"id": "agent", "tools": ["fixture"]}, data)
            else:
                column = {"id": "waterfall", "type": "waterfall", "target_field": "email", "waterfall": ["fixture"], "verify": False}
                result = await enrichment.enrich_cell(db, "mixed", 1, "waterfall", column, data, [column], force=True)
            db.commit()
            return result
    async def scenario():
        with workspace_scope("one"), execution_scope("one", "mixed", 789):
            other = "waterfall" if first_kind == "agent" else "agent"
            return await asyncio.gather(cell(first_kind), cell(other))
    results = asyncio.run(scenario())
    assert calls == ["fixture"]
    assert sum(bool(result.get("value")) for result in results) == 1
    errors = [result.get("error") for result in results if not result.get("value")]
    assert errors == (["workbook_budget"] if first_kind == "agent" else ["agent_workbook_budget"])
    with attempts() as db:
        assert db.get(Workbook, "mixed").budget_spent_usd == 0.02
        receipts = db.query(WorkbookSpendAttempt).filter_by(workbook_id="mixed").all()
        assert len(receipts) == 1 and receipts[0].status == "settled"
