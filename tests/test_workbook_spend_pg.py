"""Workbook spend reservations under real PostgreSQL concurrency and FORCE RLS.

The SQLite suite serializes writers globally, which hides row-lock mistakes.
These tests race many connections as the non-superuser app role and assert the
budget, replay and single-dispatch invariants hold (PG-gated).
"""

import os
import threading
import uuid

import pytest

TEST_DATABASE_URL = os.getenv("TEST_DATABASE_URL")

pytestmark = pytest.mark.skipif(
    not TEST_DATABASE_URL,
    reason="TEST_DATABASE_URL not set (Postgres spend tests skipped)",
)


@pytest.fixture(scope="module")
def app_session():
    from tests.pg_rls_support import rls_app_session
    factory, dispose = rls_app_session(TEST_DATABASE_URL, pool_size=25)
    yield factory
    dispose()


def _workbook(app_session, cap_usd):
    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.workbook.models import Workbook
    ws, wid = f"ws_spend_{uuid.uuid4().hex[:8]}", f"wb_{uuid.uuid4().hex[:8]}"
    with workspace_scope(ws), app_session() as db:
        db.add(Workbook(id=wid, workspace_id=ws, name="Spend", budget_max_usd=cap_usd, budget_spent_usd=0))
        db.commit()
    return ws, wid


def _race(n, fn):
    barrier = threading.Barrier(n)
    results, errors = [None] * n, []

    def run(i):
        try:
            barrier.wait(timeout=10)
            results[i] = fn(i)
        except Exception as exc:
            errors.append((i, repr(exc)))

    threads = [threading.Thread(target=run, args=(i,)) for i in range(n)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    assert not errors, errors
    return results


def _args(app_session, ws, wid, **overrides):
    return dict(session_factory=app_session, workspace_id=ws, workbook_id=wid, run_id="job:1",
                column_id="email", provider="fixture", exposure_microusd=20000,
                cell_limit_microusd=20000, cost_basis={"kind": "catalog_estimate"},
                operation_contract={"inputs": {"company": "Acme"}}, **overrides)


def test_concurrent_reservations_never_exceed_workbook_cap(app_session):
    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.workbook.spend_models import WorkbookSpendAttempt
    from apps.api.services.workbook.spend_service import reserve_attempt

    ws, wid = _workbook(app_session, cap_usd=0.10)  # fits exactly five 0.02 reservations

    def reserve(i):
        with workspace_scope(ws):
            return reserve_attempt(**_args(app_session, ws, wid, row_identity=f"row:{i}",
                                           attempt_key=f"key:{i}"))

    results = _race(20, reserve)
    assert sum(r["ok"] for r in results) == 5
    assert {r["reason"] for r in results if not r["ok"]} == {"workbook_budget"}
    with workspace_scope(ws), app_session() as db:
        rows = db.query(WorkbookSpendAttempt).filter_by(workspace_id=ws, workbook_id=wid).all()
        assert len(rows) == 5 and sum(r.reserved_microusd for r in rows) == 100000


def test_concurrent_replays_of_one_attempt_create_one_reservation(app_session):
    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.workbook.spend_models import WorkbookSpendAttempt
    from apps.api.services.workbook.spend_service import reserve_attempt

    ws, wid = _workbook(app_session, cap_usd=1.0)

    def reserve(_):
        with workspace_scope(ws):
            return reserve_attempt(**_args(app_session, ws, wid, row_identity="row:1", attempt_key="same"))

    results = _race(10, reserve)
    assert all(r["ok"] for r in results)
    assert len({r["id"] for r in results}) == 1
    assert sum(not r.get("reused") for r in results) == 1
    with workspace_scope(ws), app_session() as db:
        assert db.query(WorkbookSpendAttempt).filter_by(workspace_id=ws, workbook_id=wid).count() == 1


def test_competing_dispatchers_authorize_exactly_one_call(app_session):
    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.workbook.spend_service import reserve_attempt, transition_attempt

    ws, wid = _workbook(app_session, cap_usd=1.0)
    with workspace_scope(ws):
        reserved = reserve_attempt(**_args(app_session, ws, wid, row_identity="row:1", attempt_key="k"))

    def dispatch(_):
        with workspace_scope(ws):
            return transition_attempt(session_factory=app_session, workspace_id=ws, workbook_id=wid,
                                      attempt_id=reserved["id"], contract_hash=reserved["contract_hash"],
                                      action="dispatch")

    assert sorted(_race(10, dispatch)) == [False] * 9 + [True]


def test_reservations_are_invisible_and_unwritable_across_tenants(app_session):
    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.workbook.spend_models import WorkbookSpendAttempt
    from apps.api.services.workbook.spend_service import reserve_attempt

    ws, wid = _workbook(app_session, cap_usd=1.0)
    with workspace_scope(ws):
        assert reserve_attempt(**_args(app_session, ws, wid, row_identity="row:1", attempt_key="k"))["ok"]
    other = f"ws_spend_{uuid.uuid4().hex[:8]}"
    with workspace_scope(other):
        # Another tenant cannot see the workbook, so it cannot reserve against it.
        assert reserve_attempt(**_args(app_session, ws, wid, row_identity="row:2",
                                       attempt_key="k2"))["reason"] == "workbook_not_found"
        with app_session() as db:
            assert db.query(WorkbookSpendAttempt).filter_by(workbook_id=wid).count() == 0
