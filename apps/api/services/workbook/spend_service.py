"""Short, committed spend-attempt transitions; never perform network I/O here.

Reservation admission and final settlement are deliberately separate from these
guards. An uncertain dispatched attempt cannot be released by a retry/cancel.
"""
import time
import uuid
import hashlib
import json
from decimal import Decimal, ROUND_CEILING, ROUND_FLOOR

from sqlalchemy import update, func

from .spend_models import WorkbookSpendAttempt
from .models import Workbook


def reserve_attempt(*, session_factory, workspace_id: str, workbook_id: str,
                    run_id: str, row_identity: str, column_id: str, provider: str,
                    attempt_key: str, exposure_microusd: int, cell_limit_microusd: int,
                    cost_basis: dict, operation_contract: dict) -> dict:
    """Reserve catalog exposure, not invoice spend. No dispatch authorization.

    All admissions serialize on the workbook row. The no-op UPDATE also obtains
    SQLite's writer lock before reading headroom. Provider calls happen later,
    only after a successful guarded dispatch transition.
    """
    identities = (workspace_id, workbook_id, run_id, row_identity, column_id, provider, attempt_key)
    if not all(isinstance(v, str) and 0 < len(v) <= 255 for v in identities):
        raise ValueError("Bounded scoped identities are required")
    if any(type(v) is not int or v < 0 or v > 2**63 - 1 for v in (exposure_microusd, cell_limit_microusd)):
        raise ValueError("Exposure and cell limit must be nonnegative integer micro-USD")
    if not isinstance(cost_basis, dict) or not cost_basis:
        raise ValueError("An explicit cost basis is required")
    if not isinstance(operation_contract, dict) or not operation_contract:
        raise ValueError("An explicit provider operation contract is required")
    contract = dict(workbook_id=workbook_id, run_id=run_id, row_identity=row_identity,
                    column_id=column_id, provider=provider, exposure=exposure_microusd,
                    cell_limit=cell_limit_microusd, cost_basis=cost_basis,
                    operation=operation_contract)
    serialized = json.dumps(contract, sort_keys=True, separators=(",", ":"), allow_nan=False)
    digest = hashlib.sha256(serialized.encode()).hexdigest()
    with session_factory() as db:
        locked = db.execute(update(Workbook).where(Workbook.id == workbook_id,
            Workbook.workspace_id == workspace_id).values(budget_spent_usd=Workbook.budget_spent_usd))
        if locked.rowcount != 1:
            return {"ok": False, "reason": "workbook_not_found"}
        previous = db.query(WorkbookSpendAttempt).filter_by(workspace_id=workspace_id, attempt_key=attempt_key).first()
        if previous:
            if previous.contract_hash != digest:
                return {"ok": False, "reason": "contract_conflict"}
            return {"ok": True, "id": previous.id, "status": previous.status, "contract_hash": digest, "reused": True}
        wb = db.get(Workbook, workbook_id)
        scoped = db.query(func.coalesce(func.sum(WorkbookSpendAttempt.reserved_microusd), 0)).filter(
            WorkbookSpendAttempt.workspace_id == workspace_id,
            WorkbookSpendAttempt.workbook_id == workbook_id)
        outstanding = scoped.filter(WorkbookSpendAttempt.status.in_(["reserved", "dispatched", "uncertain"])).scalar()
        cell_exposure = db.query(func.coalesce(func.sum(func.coalesce(
            WorkbookSpendAttempt.settled_microusd, WorkbookSpendAttempt.reserved_microusd)), 0)).filter(
            WorkbookSpendAttempt.workspace_id == workspace_id, WorkbookSpendAttempt.workbook_id == workbook_id,
            WorkbookSpendAttempt.run_id == run_id,
            WorkbookSpendAttempt.row_identity == row_identity, WorkbookSpendAttempt.column_id == column_id,
            WorkbookSpendAttempt.status != "released").scalar()
        if cell_exposure + exposure_microusd > cell_limit_microusd:
            return {"ok": False, "reason": "cell_budget"}
        cap = Decimal(str(wb.budget_max_usd or 0))
        spent = Decimal(str(wb.budget_spent_usd or 0))
        if not cap.is_finite() or not spent.is_finite() or cap < 0 or spent < 0:
            return {"ok": False, "reason": "invalid_workbook_budget"}
        if cap > 0 and (int((spent * 1000000).to_integral_value(rounding=ROUND_CEILING)) + outstanding + exposure_microusd >
                        int((cap * 1000000).to_integral_value(rounding=ROUND_FLOOR))):
            return {"ok": False, "reason": "workbook_budget"}
        attempt_id, now = str(uuid.uuid4()), time.time()
        db.add(WorkbookSpendAttempt(id=attempt_id, workspace_id=workspace_id, workbook_id=workbook_id,
            run_id=run_id, row_identity=row_identity, column_id=column_id, provider=provider,
            attempt_key=attempt_key, contract_hash=digest, reserved_microusd=exposure_microusd,
            cost_basis=json.loads(serialized)["cost_basis"], status="reserved", created_at=now, updated_at=now))
        db.commit()
        return {"ok": True, "id": attempt_id, "status": "reserved", "contract_hash": digest, "reused": False}


def settle_attempt(*, session_factory, workspace_id: str, workbook_id: str,
                   attempt_id: str, contract_hash: str, charged_microusd: int,
                   accounting_basis: str, result: dict) -> dict:
    """Atomically persist a known outcome and account cost exactly once.

    Catalog accounting is explicitly distinguished from vendor-confirmed cost.
    Overruns are recorded, never silently clipped to the original reservation.
    Uncertain attempts require a separate audited reconciliation path.
    """
    if type(charged_microusd) is not int or not 0 <= charged_microusd <= 2**63 - 1:
        raise ValueError("Invalid settlement amount")
    if accounting_basis not in {"catalog_estimate", "vendor_confirmed", "confirmed_nonbillable"}:
        raise ValueError("Explicit accounting basis required")
    if accounting_basis == "confirmed_nonbillable" and charged_microusd != 0:
        raise ValueError("Nonbillable outcomes cannot have a charge")
    receipt = json.loads(json.dumps({"accounting_basis": accounting_basis, "result": result}, allow_nan=False))
    with session_factory() as db:
        locked = db.execute(update(Workbook).where(Workbook.id == workbook_id,
            Workbook.workspace_id == workspace_id).values(budget_spent_usd=Workbook.budget_spent_usd))
        if locked.rowcount != 1:
            return {"ok": False, "reason": "workbook_not_found"}
        attempt = db.query(WorkbookSpendAttempt).filter_by(id=attempt_id, workspace_id=workspace_id,
            workbook_id=workbook_id, contract_hash=contract_hash).with_for_update().first()
        if attempt is None:
            return {"ok": False, "reason": "attempt_not_found"}
        if attempt.status == "settled":
            if attempt.settled_microusd != charged_microusd or attempt.result != receipt:
                return {"ok": False, "reason": "settlement_conflict"}
            return {"ok": True, "reused": True}
        if attempt.status != "dispatched":
            return {"ok": False, "reason": "invalid_state"}
        attempt.status = "settled"
        attempt.settled_microusd = charged_microusd
        attempt.result = receipt
        attempt.updated_at = time.time()
        db.execute(update(Workbook).where(Workbook.id == workbook_id, Workbook.workspace_id == workspace_id)
            .values(budget_spent_usd=func.coalesce(Workbook.budget_spent_usd, 0) + charged_microusd / 1000000))
        db.commit()
        return {"ok": True, "reused": False}


def transition_attempt(*, session_factory, workspace_id: str, workbook_id: str,
                       attempt_id: str, contract_hash: str, action: str) -> bool:
    """Return True only for the caller that committed the allowed transition.

    A False dispatch result never authorizes another provider call. No-op,
    duplicate, wrong-tenant and stale-contract requests are all fail-closed.
    Settlement requires a separate atomic budget-accounting transaction.
    """
    transitions = {
        "dispatch": ("reserved", "dispatched"),
        "release_before_dispatch": ("reserved", "released"),
        "mark_uncertain": ("dispatched", "uncertain"),
    }
    if action not in transitions:
        raise ValueError("Unsupported spend transition")
    if not all(isinstance(value, str) and value for value in
               (workspace_id, workbook_id, attempt_id, contract_hash)):
        raise ValueError("Scoped attempt identity and contract hash are required")
    previous, following = transitions[action]
    with session_factory() as db:
        run_predicates = []
        if action == "dispatch":
            from apps.api.services.workbook.batch_attempts import batch_owner, fence_workbook_run_state
            owner = batch_owner.get()
            if owner is not None:
                if owner["workspace_id"] != workspace_id:
                    return False
                try:
                    fence_workbook_run_state(db, workbook_id)
                except ValueError:
                    return False
                run_predicates.append(WorkbookSpendAttempt.run_id == f"job:{owner['job_id']}")
        result = db.execute(update(WorkbookSpendAttempt).where(
            WorkbookSpendAttempt.id == attempt_id,
            WorkbookSpendAttempt.workspace_id == workspace_id,
            WorkbookSpendAttempt.workbook_id == workbook_id,
            WorkbookSpendAttempt.contract_hash == contract_hash,
            WorkbookSpendAttempt.status == previous,
            *run_predicates,
        ).values(status=following, updated_at=time.time()))
        changed = result.rowcount == 1
        db.commit()
        return changed


async def execute_reserved_attempt(*, operation, reservation: dict) -> dict:
    """Dispatch an injected provider operation only after durable admission.

    Operation returns an explicit charged_microusd/accounting_basis/result
    envelope. Missing or invalid accounting is uncertain, never assumed free.
    Callers must supply a stable approved attempt key, not generate one on retry.
    Queued workbook callers also fence dispatch against their active lease.
    """
    admitted = reserve_attempt(**reservation)
    if not admitted["ok"]:
        return admitted
    identity = {key: reservation[key] for key in ("session_factory", "workspace_id", "workbook_id")}
    identity.update(attempt_id=admitted["id"], contract_hash=admitted["contract_hash"])
    if admitted["status"] == "settled":
        with reservation["session_factory"]() as db:
            receipt = db.query(WorkbookSpendAttempt).filter_by(id=admitted["id"],
                workspace_id=reservation["workspace_id"], workbook_id=reservation["workbook_id"],
                contract_hash=admitted["contract_hash"], status="settled").one()
            return {"ok": True, "reused": True, "attempt_id": receipt.id,
                    "charged_microusd": receipt.settled_microusd, **receipt.result}
    if not transition_attempt(**identity, action="dispatch"):
        return {"ok": False, "reason": "attempt_not_dispatchable", "attempt_id": admitted["id"],
                "automatic_retry_allowed": False}
    try:
        outcome = await operation()
        if not isinstance(outcome, dict) or not isinstance(outcome.get("result"), dict):
            raise ValueError("Provider operation must return an accounting envelope")
        settled = settle_attempt(**identity, charged_microusd=outcome["charged_microusd"],
            accounting_basis=outcome["accounting_basis"], result=outcome["result"])
        if not settled["ok"]:
            raise RuntimeError("Provider outcome settlement was not confirmed")
        return {"ok": True, "reused": False, "attempt_id": admitted["id"],
                "charged_microusd": outcome["charged_microusd"],
                "accounting_basis": outcome["accounting_basis"], "result": outcome["result"]}
    except BaseException:
        # Includes task cancellation. If persistence itself is unavailable the
        # durable dispatched status still blocks a second provider invocation.
        try:
            transition_attempt(**identity, action="mark_uncertain")
        except Exception:
            pass
        raise
