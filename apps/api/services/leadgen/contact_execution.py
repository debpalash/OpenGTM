"""Durable spend boundary for Chat contact enrichment.

A lost process may have billed a provider without recording its response. Such
claims expire to an explicit uncertain outcome, never back to runnable. A new
approved action key is required after reviewing provider usage.
"""

import asyncio
import hashlib
import json
import time
import uuid

from sqlalchemy import Column, Float, JSON, String, UniqueConstraint
from sqlalchemy.exc import IntegrityError

from apps.api.database import Base


class ContactExecution(Base):
    __tablename__ = "contact_executions"
    __table_args__ = (
        UniqueConstraint("workspace_id", "action_id", name="uq_contact_execution_action"),
    )

    id = Column(String(36), primary_key=True)
    workspace_id = Column(String(255), nullable=False)
    action_id = Column(String(255), nullable=False)
    contract_hash = Column(String(64), nullable=False)
    status = Column(String(20), nullable=False)
    started_at = Column(Float, nullable=False)
    expires_at = Column(Float, nullable=False)
    result = Column(JSON, nullable=True)


def _blocked(action_id, status, *, retry_after=0):
    return {
        "ok": False,
        "action_id": action_id,
        "execution_status": status,
        "error": "contact_action_in_progress" if status == "running" else "contact_action_outcome_uncertain",
        "reused": True,
        "automatic_retry_allowed": False,
        "retry_after_seconds": retry_after,
        "recovery": (
            "Check this action again after the retry interval; no additional provider calls were started."
            if status == "running" else
            "Provider usage may have been billed. Review usage and saved results before approving a new action key."
        ),
    }


async def execute_contact_once(*, workspace_id, action_id, contract, operation,
                               timeout_seconds=900, session_factory=None):
    """Atomically claim before network I/O and replay results across processes."""
    if not workspace_id or not action_id or len(action_id) > 255:
        raise ValueError("A workspace and bounded action key are required")
    if session_factory is None:
        from apps.api.database import SessionLocal
        session_factory = SessionLocal
    fingerprint = hashlib.sha256(json.dumps(
        contract, sort_keys=True, separators=(",", ":"), allow_nan=False,
    ).encode()).hexdigest()
    now = time.time()
    claim_id = str(uuid.uuid4())
    with session_factory() as db:
        db.add(ContactExecution(
            id=claim_id, workspace_id=workspace_id, action_id=action_id,
            contract_hash=fingerprint, status="running", started_at=now,
            expires_at=now + timeout_seconds + 30,
        ))
        try:
            db.commit()
        except IntegrityError:
            db.rollback()
            previous = db.query(ContactExecution).filter_by(
                workspace_id=workspace_id, action_id=action_id,
            ).one()
            if previous.contract_hash != fingerprint:
                return {"ok": False, "error": "Idempotency key conflicts with a different contact contract",
                        "action_id": action_id, "execution_status": "conflict"}
            if previous.status in {"completed", "failed"}:
                return {**previous.result, "reused": True, "execution_status": previous.status}
            if previous.status == "running" and previous.expires_at > now:
                return _blocked(action_id, "running", retry_after=max(1, int(previous.expires_at - now)))
            # Conditional update cannot overwrite a concurrently completed result.
            db.query(ContactExecution).filter_by(
                id=previous.id, workspace_id=workspace_id, status="running",
            ).filter(ContactExecution.expires_at <= now).update({"status": "uncertain"})
            db.commit()
            db.refresh(previous)
            if previous.status in {"completed", "failed"}:
                return {**previous.result, "reused": True, "execution_status": previous.status}
            return _blocked(action_id, "uncertain")

    def finish(status, result=None):
        with session_factory() as db:
            db.query(ContactExecution).filter_by(
                id=claim_id, workspace_id=workspace_id,
            ).update({"status": status, "result": result})
            db.commit()

    try:
        result = await asyncio.wait_for(operation(), timeout=timeout_seconds)
        if not isinstance(result, dict):
            raise ValueError("Invalid contact result")
        status = "completed" if result.get("ok") else "failed"
        finish(status, result)
        return {**result, "execution_status": status}
    except asyncio.CancelledError:
        finish("uncertain")
        raise
    except Exception:
        finish("uncertain")
        return _blocked(action_id, "uncertain")
