"""Durable batch dispatch claims. No network calls occur inside these transactions.

One AI prepass per queued workbook job. Integration must honor the returned
action: only the first committed claim permits dispatch; no acknowledgement
means reconciliation, never permission to create another vendor batch.
"""
import hashlib
import json
from contextlib import contextmanager
from contextvars import ContextVar
from datetime import datetime

from apps.api.models import Job
from apps.api.services.queue_service import _timestamp_matches

batch_owner = ContextVar("workbook_batch_owner", default=None)


class BatchRecoveryRequired(RuntimeError):
    """Never fall through to synchronous work after an uncertain batch attempt."""


@contextmanager
def batch_lease_scope(job_id, payload):
    lease = payload.get("__queue_lease") or {}
    owner = None
    if lease.get("worker_id") and lease.get("locked_at"):
        owner = dict(job_id=job_id, workspace_id=payload.get("workspace_id"),
                     workbook_id=payload.get("workbook_id"), worker_id=lease["worker_id"],
                     locked_at=datetime.fromisoformat(lease["locked_at"]))
    token = batch_owner.set(owner)
    try:
        yield
    finally:
        batch_owner.reset(token)


def _owned_job(db, *, job_id, workspace_id, workbook_id, worker_id, locked_at):
    if not workspace_id or not workbook_id or not worker_id or locked_at is None:
        raise ValueError("Batch claims require scoped queue ownership")
    predicates = (Job.id == job_id, Job.type == "run_workbook",
                  Job.workspace_id == workspace_id, Job.worker_id == worker_id,
                  Job.status == "processing", _timestamp_matches(Job.locked_at, locked_at))
    # Acquire the writer lock before reading JSON, including on SQLite where
    # SELECT FOR UPDATE alone has no effect. Failed ownership changes nothing.
    if db.query(Job).filter(*predicates).update(
        {Job.retry_count: Job.retry_count}, synchronize_session=False
    ) != 1:
        raise ValueError("Batch queue lease is no longer active")
    job = db.query(Job).filter(*predicates).populate_existing().one()
    if (job.payload or {}).get("workbook_id") != workbook_id:
        raise ValueError("Batch workbook does not match queue job")
    return job


def claim_batch_attempt(session_factory, *, contract: dict, **ownership):
    if not isinstance(contract, dict) or not contract:
        raise ValueError("Batch input contract is required")
    digest = hashlib.sha256(json.dumps(contract, sort_keys=True, separators=(",", ":"),
                                      allow_nan=False).encode()).hexdigest()
    with session_factory() as db:
        job = _owned_job(db, **ownership)
        payload = dict(job.payload or {})
        previous = payload.get("ai_batch_attempt")
        if previous is not None:
            if not isinstance(previous, dict) or previous.get("contract_hash") != digest:
                raise ValueError("Batch input contract changed")
            return {**previous, "action": "retrieve" if previous.get("vendor_batch_id") else "reconcile"}
        attempt = {"contract_hash": digest, "state": "dispatch_claimed", "vendor_batch_id": None}
        job.payload = {**payload, "ai_batch_attempt": attempt}
        db.commit()
        return {**attempt, "action": "dispatch"}


def current_batch_attempt(session_factory, **ownership):
    """Read under current ownership before choosing any non-batch fallback."""
    with session_factory() as db:
        job = _owned_job(db, **ownership)
        attempt = (job.payload or {}).get("ai_batch_attempt")
        return dict(attempt) if isinstance(attempt, dict) else attempt


def fence_batch_result(db, *, contract_hash: str, **ownership):
    """Lock/check ownership in the SAME transaction that writes a cell result."""
    job = _owned_job(db, **ownership)
    attempt = (job.payload or {}).get("ai_batch_attempt")
    if (not isinstance(attempt, dict) or attempt.get("contract_hash") != contract_hash
            or attempt.get("state") != "accepted" or not attempt.get("vendor_batch_id")):
        raise ValueError("Batch result has no matching accepted attempt")
    return job


def checkpoint_batch_results(session_factory, *, contract_hash, results, **ownership):
    if not isinstance(results, dict) or not results or any(
        not isinstance(key, str) or not key or not isinstance(value, str) or not value
        for key, value in results.items()
    ):
        raise ValueError("Batch results must map request identities to nonempty text")
    with session_factory() as db:
        job = fence_batch_result(db, contract_hash=contract_hash, **ownership)
        payload = dict(job.payload)
        attempt = dict(payload["ai_batch_attempt"])
        previous = attempt.get("results", {})
        if any(key in previous and previous[key] != value for key, value in results.items()):
            raise ValueError("Batch result checkpoint conflicts with committed results")
        attempt["results"] = {**previous, **results}
        job.payload = {**payload, "ai_batch_attempt": attempt}
        db.commit()


def fence_workbook_run_state(db, workbook_id):
    """Fence queue-owned workbook state in its write transaction."""
    owner = batch_owner.get()
    if owner is None:
        return  # Legacy direct callers have no queue lease.
    if owner["workbook_id"] != workbook_id:
        raise ValueError("Workbook state does not match queue lease")
    _owned_job(db, **owner)


def check_workbook_run_owner(session_factory, workbook_id):
    """Preflight only; release the short lock before any external operation."""
    if batch_owner.get() is not None:
        with session_factory() as db:
            fence_workbook_run_state(db, workbook_id)


def acknowledge_batch_attempt(session_factory, *, contract_hash: str, vendor_batch_id: str, **ownership):
    if not isinstance(vendor_batch_id, str) or not vendor_batch_id.strip():
        raise ValueError("Vendor batch identity is required")
    with session_factory() as db:
        job = _owned_job(db, **ownership)
        payload = dict(job.payload or {})
        previous = payload.get("ai_batch_attempt")
        if not isinstance(previous, dict) or previous.get("contract_hash") != contract_hash:
            raise ValueError("Batch claim does not match acknowledgement")
        if previous.get("vendor_batch_id") not in (None, vendor_batch_id):
            raise ValueError("Batch already has a different vendor identity")
        attempt = {**previous, "state": "accepted", "vendor_batch_id": vendor_batch_id}
        job.payload = {**payload, "ai_batch_attempt": attempt}
        db.commit()
        return attempt
