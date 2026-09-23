"""Queue-owned external-action claims; uncertain dispatch is never replayable."""
import hashlib
import json

from apps.api.services.workbook.batch_attempts import _owned_job, batch_owner


def summarize_output_attempts(attempts):
    """Public counts only; never expose destinations, cell keys or raw receipts.

    Awaiting receipt can still be in flight. A failed response does not prove
    that the destination made no change. No journal means unknown, not zero.
    """
    if not isinstance(attempts, dict):
        return None
    counts = dict(total=len(attempts), awaiting_receipt=0, succeeded=0, failed=0, unknown=0)
    for attempt in attempts.values():
        if not isinstance(attempt, dict):
            counts["unknown"] += 1
        elif attempt.get("state") == "dispatch_claimed":
            counts["awaiting_receipt"] += 1
        elif attempt.get("state") == "recorded" and isinstance(attempt.get("result"), dict):
            success = attempt["result"].get("success")
            counts["succeeded" if success is True else "failed" if success is False else "unknown"] += 1
        else:
            counts["unknown"] += 1
    return counts


async def execute_output_with_journal(session_factory, execute, **request):
    """Fence queued sends across retries; direct calls retain their existing path.

    A claim without a receipt is uncertain, even if the process died before the
    actual network request. Never infer that this is permission to resend.
    """
    owner = batch_owner.get()
    if owner is None:
        return await execute(**request)
    if (request["workbook_id"] != owner["workbook_id"]
            or request["workspace_id"] != owner["workspace_id"]):
        raise ValueError("Output does not match queue ownership")
    row_id = request["lead_data"].get("__row_id")
    subject = f"row:{row_id}" if row_id is not None else f"lead:{request['lead_id']}"
    cell_key = f"{subject}/{request['col_config']['id']}"
    claim = claim_output_attempt(session_factory, cell_key=cell_key, contract=request, **owner)
    if claim["action"] == "reuse":
        return claim["result"]
    if claim["action"] == "reconcile":
        return {"success": False, "value": None, "error": "output_delivery_requires_review"}
    # No database transaction is held while contacting the destination.
    result = await execute(**request)
    succeeded = ("success" not in result or result["success"] is True) and not bool(result.get("error"))
    receipt = {"success": succeeded, "value": result.get("value") if succeeded else None,
               "error": result.get("error") or (None if succeeded else "output_failed")}
    record_output_result(session_factory, cell_key=cell_key, contract_hash=claim["contract_hash"],
                         result=receipt, **owner)
    return receipt


def claim_output_attempt(session_factory, *, cell_key: str, contract: dict, **owner):
    if not cell_key or not isinstance(contract, dict) or not contract:
        raise ValueError("Output identity and contract are required")
    digest = hashlib.sha256(json.dumps(contract, sort_keys=True, separators=(",", ":"), allow_nan=False).encode()).hexdigest()
    with session_factory() as db:
        job = _owned_job(db, **owner)
        payload = dict(job.payload or {})
        attempts = dict(payload.get("output_attempts") or {})
        prior = attempts.get(cell_key)
        if prior is not None:
            if not isinstance(prior, dict) or prior.get("contract_hash") != digest:
                raise ValueError("Output contract changed; review required")
            return {**prior, "action": "reuse" if prior.get("state") == "recorded" else "reconcile"}
        attempt = {"contract_hash": digest, "state": "dispatch_claimed"}
        attempts[cell_key] = attempt
        job.payload = {**payload, "output_attempts": attempts}
        db.commit()
        return {**attempt, "action": "dispatch"}


def record_output_result(session_factory, *, cell_key: str, contract_hash: str, result: dict, **owner):
    if not isinstance(result, dict) or type(result.get("success")) is not bool:
        raise ValueError("Output result requires explicit success")
    receipt = {key: result.get(key) for key in ("success", "value", "error")}
    with session_factory() as db:
        job = _owned_job(db, **owner)
        payload = dict(job.payload or {})
        attempts = dict(payload.get("output_attempts") or {})
        prior = attempts.get(cell_key)
        if not isinstance(prior, dict) or prior.get("contract_hash") != contract_hash:
            raise ValueError("Output claim does not match result")
        if prior.get("state") == "recorded" and prior.get("result") != receipt:
            raise ValueError("Output result conflicts with committed receipt")
        attempts[cell_key] = {**prior, "state": "recorded", "result": receipt}
        job.payload = {**payload, "output_attempts": attempts}
        db.commit()
