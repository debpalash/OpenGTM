"""Adapt native account-discovery actions and workbook state for G1 scoring."""

from __future__ import annotations

from typing import Any, Mapping

from apps.api.services.workbook.models import Workbook, WorkbookRow


def _dict(value: Any) -> dict[str, Any]:
    return value if isinstance(value, dict) else {}


def _list(value: Any) -> list[Any]:
    return value if isinstance(value, list) else []


def _text(value: Any) -> str:
    return str(value).strip() if value is not None else ""


def _steps(trace: Mapping[str, Any], tool_name: str) -> list[dict[str, Any]]:
    return [
        step for step in _list(trace.get("steps"))
        if isinstance(step, dict) and _text(step.get("tool_name")) == tool_name
    ]


def _snapshot(db: Any, workspace_id: str, workbook_id: str) -> dict[str, Any]:
    workbook = db.query(Workbook).filter(
        Workbook.id == workbook_id,
        Workbook.workspace_id == workspace_id,
    ).one_or_none()
    if workbook is None:
        return {
            "exists": False,
            "workbook_id": workbook_id,
            "workspace_id": "",
            "row_count": 0,
            "brief": {},
            "source_run": {},
            "accounts": [],
        }
    rows = db.query(WorkbookRow).filter(
        WorkbookRow.workbook_id == workbook.id,
        WorkbookRow.workspace_id == workspace_id,
    ).order_by(WorkbookRow.position.asc(), WorkbookRow.id.asc()).all()
    accounts = []
    for row in rows:
        data = _dict(row.data)
        accounts.append({
            "account_id": _text(data.get("account_id") or row.canonical_entity_id),
            "company": _text(data.get("company")),
            "canonical_domain": _text(data.get("canonical_domain")),
            "fit_reasons": list(_list(data.get("fit_reasons"))),
            "criteria_evidence": dict(_dict(data.get("criteria_evidence"))),
            "evidence_urls": list(_list(data.get("evidence_urls"))),
            "retrieved_at": _text(data.get("retrieved_at")),
            "field_confidence": data.get("field_confidence"),
        })
    config = _dict(workbook.source_config)
    return {
        "exists": True,
        "workbook_id": _text(workbook.id),
        "workspace_id": _text(workbook.workspace_id),
        "row_count": len(rows),
        "brief": dict(_dict(config.get("account_discovery_brief"))),
        "source_run": dict(_dict(config.get("last_source_run"))),
        "accounts": accounts,
    }


def _action(step: Mapping[str, Any], snapshot: Mapping[str, Any]) -> dict[str, Any]:
    args = _dict(step.get("args"))
    result = _dict(step.get("result"))
    approval = _dict(step.get("approval"))
    workbook_id = _text(result.get("workbook_id"))
    persisted = snapshot.get("exists") is True and snapshot.get("workbook_id") == workbook_id
    return {
        "action_id": _text(result.get("action_id") or args.get("idempotency_key")),
        "idempotency_key": _text(result.get("action_id") or args.get("idempotency_key")),
        "workspace_id": _text(snapshot.get("workspace_id")),
        "approved": _text(approval.get("decision")) in {"approve", "approved"},
        "status": "succeeded" if result.get("ok") is True else "failed",
        "persisted": persisted,
        "reused": result.get("reused") is True,
        "workbook_id": workbook_id,
        "source_job_id": result.get("source_job_id"),
        "row_count": int(snapshot.get("row_count") or 0),
        "url": _text(result.get("url")),
    }


def build_account_discovery_artifact(
    trace: Mapping[str, Any],
    db: Any,
) -> dict[str, Any]:
    """Build a G1 artifact from approved actions and actual workbook rows."""
    steps = _steps(trace, "create_source_workbook")
    create_step = steps[0] if steps else {}
    retry_step = steps[1] if len(steps) > 1 else {}
    workspace_id = _text(trace.get("workspace_id"))
    create_result = _dict(create_step.get("result"))
    retry_result = _dict(retry_step.get("result"))
    snapshot = _snapshot(db, workspace_id, _text(create_result.get("workbook_id")))
    retry_snapshot = _snapshot(db, workspace_id, _text(retry_result.get("workbook_id")))
    action = _action(create_step, snapshot)
    retry_action = _action(retry_step, retry_snapshot)
    brief = _dict(snapshot.get("brief"))
    source_run = _dict(snapshot.get("source_run"))
    requested = int(brief.get("requested_count") or 0)
    delivered = int(source_run.get("delivered_count") or 0)
    completed = (
        action.get("persisted") is True
        and retry_action.get("reused") is True
        and _text(source_run.get("status")) == "complete"
        and requested > 0
        and delivered == requested
    )
    scenario = {
        "id": "structured_account_discovery",
        "status": "completed" if completed else "partial",
        "workflow_ids": ["G1"],
        "workspace_id": workspace_id,
        "conversation_id": _text(trace.get("conversation_id")),
        "prompt_steps": list(_list(trace.get("prompts"))),
        "brief": dict(brief),
        "account_action": action,
        "account_retry_action": retry_action,
        "source_run": dict(source_run),
        "accounts": list(snapshot.get("accounts") or []),
        "can_continue_enrichment": snapshot.get("exists") is True and bool(snapshot.get("accounts")),
        "timings_ms": {
            "acknowledgement": trace.get("acknowledgement_ms"),
            "workbook_creation": create_step.get("latency_ms"),
            "account_sourcing": trace.get("sourcing_latency_ms"),
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
