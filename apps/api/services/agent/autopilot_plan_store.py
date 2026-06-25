"""
Autopilot plan store — server-side drafted-plan + single-use nonce (ephemeral).

Closes the confused-deputy gap in the human approval gate: the gate must execute
EXACTLY the plan it displayed. We persist the drafted plan server-side at draft
time keyed by (workspace_id, plan_id) with a single-use `nonce`, render and
execute from the STORED plan, and consume the nonce ATOMICALLY so a resubmitted /
double-clicked approval runs `execute_plan` only once.

Kept INTENTIONALLY EPHEMERAL (in-process, TTL-swept) per the task's "keep the
plan store ephemeral / in existing session/memory infra; avoid DB schema
changes". A process restart drops pending (un-approved) plans — acceptable: an
unknown plan_id at consume() simply rejects execution (fail closed), it never
executes an unbound plan. Workspace + user are part of the key so a different
ws/user cannot consume another's plan.
"""

from __future__ import annotations

import os
import threading
import time
import uuid
from typing import Dict, Optional, Tuple


def _ttl_sec() -> int:
    try:
        from apps.api.services.leadgen.llm import _read_setting
        return int(str(_read_setting("AUTOPILOT_PLAN_TTL_SEC", "3600")).strip())
    except Exception:
        try:
            return int(os.environ.get("AUTOPILOT_PLAN_TTL_SEC", "3600"))
        except Exception:
            return 3600


_LOCK = threading.Lock()
# plan_id -> {workspace_id, user_id, plan, nonce, consumed: bool, created_at}
_STORE: Dict[str, dict] = {}


def _sweep_locked(now: float) -> None:
    ttl = _ttl_sec()
    stale = [pid for pid, row in _STORE.items() if now - row["created_at"] > ttl]
    for pid in stale:
        _STORE.pop(pid, None)


def put(workspace_id: Optional[str], user_id: Optional[str], plan: dict) -> Tuple[str, str]:
    """Persist a drafted plan; return (plan_id, nonce). The client echoes both."""
    plan_id = uuid.uuid4().hex
    nonce = uuid.uuid4().hex
    now = time.time()
    with _LOCK:
        _sweep_locked(now)
        _STORE[plan_id] = {
            "workspace_id": workspace_id,
            "user_id": user_id,
            "plan": plan,
            "nonce": nonce,
            "consumed": False,
            "created_at": now,
        }
    return plan_id, nonce


def peek(workspace_id: Optional[str], plan_id: str) -> Optional[dict]:
    """Return the STORED plan for rendering the confirmation gate (no consume).

    Scoped by workspace so the gate renders the caller's own stored plan, never
    a client-supplied body.
    """
    if not plan_id:
        return None
    now = time.time()
    with _LOCK:
        _sweep_locked(now)
        row = _STORE.get(plan_id)
        if not row:
            return None
        if row["workspace_id"] != workspace_id:
            return None
        return row["plan"]


def consume(
    workspace_id: Optional[str],
    user_id: Optional[str],
    plan_id: str,
    nonce: str,
) -> Optional[dict]:
    """Atomically consume the single-use nonce; return the stored plan or None.

    Returns None (→ execution rejected) when: plan_id unknown / expired, nonce
    mismatch, wrong workspace, wrong user, or already consumed (double-submit).
    The nonce is marked consumed under the lock so a concurrent second submit
    gets None.
    """
    if not plan_id or not nonce:
        return None
    now = time.time()
    with _LOCK:
        _sweep_locked(now)
        row = _STORE.get(plan_id)
        if not row:
            return None
        if row["consumed"]:
            return None
        if row["nonce"] != nonce:
            return None
        if row["workspace_id"] != workspace_id:
            return None
        if row["user_id"] != user_id:
            return None
        row["consumed"] = True
        return row["plan"]


def _reset_for_tests() -> None:
    with _LOCK:
        _STORE.clear()
