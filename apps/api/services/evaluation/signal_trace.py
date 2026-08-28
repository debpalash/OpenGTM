"""Adapt native account-signal actions and saved watch state for G6 scoring."""

from __future__ import annotations

from typing import Any, Mapping

from apps.api.services.poller.models import WatchSubscription
from apps.api.services.signals.tracking import schedule_receipt


def _dict(value: Any) -> dict[str, Any]:
    return value if isinstance(value, dict) else {}


def _list(value: Any) -> list[Any]:
    return value if isinstance(value, list) else []


def _text(value: Any) -> str:
    return str(value).strip() if value is not None else ""


def _steps(trace: Mapping[str, Any]) -> list[dict[str, Any]]:
    return [
        step for step in _list(trace.get("steps"))
        if isinstance(step, dict) and _text(step.get("tool_name")) == "track_account_signals"
    ]


def _snapshot(db: Any, workspace_id: str, schedule_id: str) -> dict[str, Any]:
    watch = db.query(WatchSubscription).filter(
        WatchSubscription.id == schedule_id,
        WatchSubscription.workspace_id == workspace_id,
        WatchSubscription.kind == "account_group",
    ).one_or_none()
    if watch is None:
        return {"exists": False, "schedule_id": schedule_id}
    receipt = schedule_receipt(watch, readback_confirmed=True)
    count = 0
    for candidate in db.query(WatchSubscription).filter(
        WatchSubscription.workspace_id == workspace_id,
        WatchSubscription.kind == "account_group",
    ).all():
        config = candidate.config if isinstance(candidate.config, dict) else {}
        if config.get("scope_key") == receipt.get("scope_key"):
            count += 1
    return {"exists": True, "schedule_count_for_scope": count, **receipt}


def _action(step: Mapping[str, Any], snapshot: Mapping[str, Any]) -> dict[str, Any]:
    args = _dict(step.get("args"))
    result = _dict(step.get("result"))
    approval = _dict(step.get("approval"))
    schedule_id = _text(result.get("schedule_id"))
    return {
        "action_id": _text(result.get("action_id") or args.get("idempotency_key")),
        "idempotency_key": _text(result.get("action_id") or args.get("idempotency_key")),
        "approved": _text(approval.get("decision")) in {"approve", "approved"},
        "status": "succeeded" if result.get("ok") is True else "failed",
        "persisted": snapshot.get("exists") is True and snapshot.get("schedule_id") == schedule_id,
        "reused": result.get("reused") is True,
        "updated": result.get("updated") is True,
        "schedule_id": schedule_id,
        "readback_confirmed": result.get("readback_confirmed") is True,
    }


def build_signal_tracking_artifact(trace: Mapping[str, Any], db: Any) -> dict[str, Any]:
    """Build one G6 artifact from approved actions and actual saved schedule state."""
    steps = _steps(trace)
    first_step = steps[0] if steps else {}
    retry_step = steps[1] if len(steps) > 1 else {}
    workspace_id = _text(trace.get("workspace_id"))
    first_result = _dict(first_step.get("result"))
    retry_result = _dict(retry_step.get("result"))
    snapshot = _snapshot(db, workspace_id, _text(first_result.get("schedule_id")))
    retry_snapshot = _snapshot(db, workspace_id, _text(retry_result.get("schedule_id")))
    action = _action(first_step, snapshot)
    retry_action = _action(retry_step, retry_snapshot)
    first_args = _dict(first_step.get("args"))
    requested = {
        "workbook_id": _text(first_args.get("workbook_id")),
        "account_ids": [_text(value) for value in _list(first_args.get("account_ids"))],
        "cadence": _text(first_args.get("cadence")),
        "signal_types": [_text(value) for value in _list(first_args.get("signal_types"))],
    }
    completed = (
        action.get("persisted") is True
        and action.get("readback_confirmed") is True
        and retry_action.get("reused") is True
        and retry_action.get("schedule_id") == action.get("schedule_id")
        and snapshot.get("scope", {}).get("account_ids") == requested["account_ids"]
    )
    scenario = {
        "id": "exact_account_signal_tracking",
        "status": "completed" if completed else "partial",
        "workflow_ids": ["G6"],
        "workspace_id": workspace_id,
        "conversation_id": _text(trace.get("conversation_id")),
        "prompt_steps": list(_list(trace.get("prompts"))),
        "requested_tracking": requested,
        "selection": {"account_ids": list(requested["account_ids"])},
        "schedule_action": action,
        "schedule_retry_action": retry_action,
        "schedule_readback": dict(snapshot),
        "timings_ms": {
            "acknowledgement": trace.get("acknowledgement_ms"),
            "schedule_creation": first_step.get("latency_ms"),
        },
        "jobs": list(_list(trace.get("jobs"))),
        "external_writes": list(_list(trace.get("external_writes"))),
    }
    return {
        "schema_version": "1.0",
        "fixture_kind": _text(trace.get("fixture_kind")),
        "run_id": _text(trace.get("run_id")),
        "mode": _text(trace.get("mode")),
        "build_sha": _text(trace.get("build_sha")),
        "started_at": _text(trace.get("started_at")),
        "finished_at": _text(trace.get("finished_at")),
        "unresolved_issues": list(_list(trace.get("unresolved_issues"))),
        "production_run_history": list(_list(trace.get("production_run_history"))),
        "scenarios": [scenario],
    }
