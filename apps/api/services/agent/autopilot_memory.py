"""
Autopilot memory — short, per-workspace record of recent autopilot runs.

Feeds the LLM planner a few of the workspace's own recent (goal → plan → outcome)
records so it can avoid re-proposing failed shapes. Kept INTENTIONALLY EPHEMERAL
(in-process, per-workspace, bounded) per the task's "keep ... in existing
session/memory infra; avoid DB schema changes". Every access is explicitly
filtered by workspace_id (no cross-tenant bleed even without a DB/RLS), and the
goal text is treated as UNTRUSTED on read-back (the planner wraps it).

`record` is idempotent on (workspace_id, workbook_id): a retried memory write
upserts rather than double-inserts. A process restart loses memory — acceptable
(memory is best-effort; the planner falls back to no-memory cleanly).
"""

from __future__ import annotations

import threading
import time
from typing import Dict, List, Optional

_LOCK = threading.Lock()
# workspace_id -> list[ {goal, plan, workbook_id, outcome, created_at} ] (bounded)
_MEM: Dict[str, List[dict]] = {}
_MAX_PER_WS = 50


def recent(workspace_id: Optional[str], limit: int = 3) -> List[dict]:
    """Most-recent autopilot records for a workspace (newest first).

    Returns [] for an unknown/None workspace. Explicitly workspace-filtered.
    """
    if not workspace_id:
        return []
    with _LOCK:
        rows = list(_MEM.get(workspace_id, []))
    rows.sort(key=lambda r: r["created_at"], reverse=True)
    return rows[: max(0, int(limit or 0))]


def record(
    workspace_id: Optional[str],
    goal: str,
    plan: dict,
    workbook_id: Optional[str],
    outcome: str,
) -> None:
    """Best-effort upsert of one run record (idempotent on (ws, workbook_id))."""
    if not workspace_id:
        return
    now = time.time()
    row = {
        "goal": goal or "",
        "plan": plan or {},
        "workbook_id": workbook_id,
        "outcome": outcome or "ok",
        "created_at": now,
    }
    with _LOCK:
        bucket = _MEM.setdefault(workspace_id, [])
        # Upsert on (workspace_id, workbook_id) when workbook_id is set.
        if workbook_id:
            for i, existing in enumerate(bucket):
                if existing.get("workbook_id") == workbook_id:
                    bucket[i] = row
                    break
            else:
                bucket.append(row)
        else:
            bucket.append(row)
        # Bound memory size (drop oldest).
        if len(bucket) > _MAX_PER_WS:
            bucket.sort(key=lambda r: r["created_at"])
            del bucket[: len(bucket) - _MAX_PER_WS]


def _reset_for_tests() -> None:
    with _LOCK:
        _MEM.clear()
