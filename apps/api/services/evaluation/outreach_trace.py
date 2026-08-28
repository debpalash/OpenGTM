"""Adapt native grounded-draft actions and saved draft state for G7 scoring."""

from __future__ import annotations

from typing import Any, Mapping

from apps.api.services.outreach.drafting import draft_receipt
from apps.api.services.outreach.orm_models import OutreachDraft, OutreachSend


def _dict(value: Any) -> dict[str, Any]:
    return value if isinstance(value, dict) else {}


def _list(value: Any) -> list[Any]:
    return value if isinstance(value, list) else []


def _text(value: Any) -> str:
    return str(value).strip() if value is not None else ""


def _steps(trace: Mapping[str, Any]) -> list[dict[str, Any]]:
    return [
        step for step in _list(trace.get("steps"))
        if isinstance(step, dict) and _text(step.get("tool_name")) == "draft_grounded_outreach"
    ]


def _snapshot(db: Any, workspace_id: str, draft_id: str) -> dict[str, Any]:
    draft = db.query(OutreachDraft).filter(
        OutreachDraft.id == draft_id,
        OutreachDraft.workspace_id == workspace_id,
    ).one_or_none()
    if draft is None:
        return {"exists": False, "draft_id": draft_id}
    receipt = draft_receipt(draft, readback_confirmed=True, reused=False)
    count = db.query(OutreachDraft).filter(
        OutreachDraft.workspace_id == workspace_id,
        OutreachDraft.action_idempotency_key == draft.action_idempotency_key,
    ).count()
    sends_after = db.query(OutreachSend).filter(
        OutreachSend.workspace_id == workspace_id,
        OutreachSend.to_email == draft.to_email,
        OutreachSend.created_at >= draft.created_at,
    ).count()
    return {
        "exists": True,
        "draft_count_for_action": count,
        "send_rows_after_draft": sends_after,
        **receipt,
    }


def _action(step: Mapping[str, Any], snapshot: Mapping[str, Any]) -> dict[str, Any]:
    args = _dict(step.get("args"))
    result = _dict(step.get("result"))
    approval = _dict(step.get("approval"))
    return {
        "action_id": _text(result.get("action_id") or args.get("idempotency_key")),
        "idempotency_key": _text(result.get("action_id") or args.get("idempotency_key")),
        "approved": _text(approval.get("decision")) in {"approve", "approved"},
        "status": "succeeded" if result.get("ok") is True else "failed",
        "persisted": snapshot.get("exists") is True and _text(snapshot.get("draft_id")) == _text(result.get("draft_id")),
        "reused": result.get("reused") is True,
        "readback_confirmed": result.get("readback_confirmed") is True,
        "draft_id": _text(result.get("draft_id")),
    }


def build_grounded_outreach_artifact(trace: Mapping[str, Any], db: Any) -> dict[str, Any]:
    """Build one G7 artifact from approved actions and actual persisted draft state."""
    steps = _steps(trace)
    first_step = steps[0] if steps else {}
    retry_step = steps[1] if len(steps) > 1 else {}
    workspace_id = _text(trace.get("workspace_id"))
    first_result = _dict(first_step.get("result"))
    retry_result = _dict(retry_step.get("result"))
    snapshot = _snapshot(db, workspace_id, _text(first_result.get("draft_id")))
    retry_snapshot = _snapshot(db, workspace_id, _text(retry_result.get("draft_id")))
    action = _action(first_step, snapshot)
    retry_action = _action(retry_step, retry_snapshot)
    source_contact = dict(_dict(trace.get("source_contact")))
    completed = (
        action.get("persisted") is True
        and action.get("readback_confirmed") is True
        and retry_action.get("reused") is True
        and retry_action.get("draft_id") == action.get("draft_id")
        and snapshot.get("send_performed") is False
        and snapshot.get("send_rows_after_draft") == 0
    )
    scenario = {
        "id": "grounded_outreach_draft",
        "status": "completed" if completed else "partial",
        "workflow_ids": ["G7"],
        "workspace_id": workspace_id,
        "conversation_id": _text(trace.get("conversation_id")),
        "prompt_steps": list(_list(trace.get("prompts"))),
        "source_contact": source_contact,
        "selection": {"person_id": _text(_dict(first_step.get("args")).get("person_id"))},
        "draft_action": action,
        "draft_retry_action": retry_action,
        "draft_readback": dict(snapshot),
        "timings_ms": {
            "acknowledgement": trace.get("acknowledgement_ms"),
            "draft_creation": first_step.get("latency_ms"),
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
