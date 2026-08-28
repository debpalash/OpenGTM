from __future__ import annotations

import asyncio
import copy
import json
from pathlib import Path

from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.routers import copilotkit as ck
from apps.api.services import chat_history
from apps.api.services.evaluation.chat_trace import build_people_workflow_artifact
from apps.api.services.evaluation.gtm_gauntlet import load_artifact, score_gauntlet
from apps.api.services.workbook.models import Base, Workbook, WorkbookRow


TRACE_FIXTURE = (
    Path(__file__).parent
    / "fixtures"
    / "gtm_gauntlet"
    / "chat_people_native_trace.json"
)


def _collect_events(generator) -> list[dict]:
    async def collect() -> list[dict]:
        events = []
        async for line in generator:
            if line.startswith("data: ") and line.strip() != "data: [DONE]":
                events.append(json.loads(line[6:]))
        return events

    return asyncio.run(collect())


def _tool_result(events: list[dict], name: str) -> dict:
    return next(
        event["tool_result"]["result"]
        for event in events
        if event.get("tool_result", {}).get("name") == name
    )


def _database(monkeypatch):
    engine = create_engine(
        "sqlite:///:memory:",
        connect_args={"check_same_thread": False},
        poolclass=StaticPool,
    )
    Base.metadata.create_all(engine, tables=[Workbook.__table__, WorkbookRow.__table__])
    factory = sessionmaker(bind=engine, autoflush=False, autocommit=False)
    monkeypatch.setattr("apps.api.database.SessionLocal", factory)
    return factory


def _execute_approved_workbook(trace: dict, *, retry: bool = False) -> dict:
    verification = next(
        step["result"]
        for step in trace["steps"]
        if step["tool_name"] == "verify_people_at_company"
    )
    args = {
        "conversation_id": trace["conversation_id"],
        "name": "Stripe Partnerships People",
        "person_ids": trace["selection"]["person_ids"],
        "idempotency_key": "chat-people:recorded-stripe-partners",
    }
    tool_call = {
        "id": "proposal-retry" if retry else "proposal-create",
        "type": "function",
        "function": {
            "name": "create_people_workbook",
            "arguments": json.dumps(args),
        },
    }
    approval_id = chat_history.create_tool_approval(
        trace["workspace_id"], None, tool_call
    )
    events = _collect_events(
        ck._resolve_approved_calls(
            [],
            [{"tool_call": {"id": approval_id}, "decision": "approve"}],
            store=object(),
            workspace_id=trace["workspace_id"],
            slug="main",
        )
    )
    result = _tool_result(events, "create_people_workbook")
    return {
        "step_id": "g5-retry-workbook" if retry else "g5-create-workbook",
        "workflow_id": "G5",
        "tool_name": "create_people_workbook",
        "status": "succeeded" if result.get("ok") else "failed",
        "latency_ms": 90 if retry else 420,
        "args": args,
        "approval": {"approval_id": approval_id, "decision": "approved"},
        "result": result,
        "source_result_set_id": verification["result_set_id"],
    }


def _real_recorded_trace(monkeypatch, tmp_path):
    factory = _database(monkeypatch)
    monkeypatch.setattr(chat_history, "DB_PATH", str(tmp_path / "chat_history.db"))
    connection = chat_history._get_db()
    try:
        chat_history._init_tables(connection)
    finally:
        connection.close()
    trace = copy.deepcopy(load_artifact(TRACE_FIXTURE))
    conversation = chat_history.create_conversation(
        trace["workspace_id"], None, title="Stripe partnerships"
    )
    trace["conversation_id"] = conversation["id"]

    for step in trace["steps"]:
        chat_history.add_message(
            conversation["id"],
            "tool",
            f"{step['tool_name']} result",
            tool_data=json.dumps(
                {"name": step["tool_name"], "result": step["result"]}
            ),
        )

    trace["steps"].append(_execute_approved_workbook(trace))
    trace["steps"].append(_execute_approved_workbook(trace, retry=True))
    return trace, factory


def test_native_chat_trace_reads_persisted_state_and_scores_current_baseline(
    monkeypatch, tmp_path
):
    trace, factory = _real_recorded_trace(monkeypatch, tmp_path)

    with factory() as db:
        artifact = build_people_workflow_artifact(trace, db)
    report = score_gauntlet(artifact)

    assert report["score"] == 87.0
    assert report["hard_failures"] == []
    assert report["run_passed"] is False
    assert report["workflows"]["G2"]["status"] == "failed"
    assert report["workflows"]["G4"]["status"] == "failed"
    assert report["workflows"]["G5"]["status"] == "passed"
    assert report["workflows"]["G2"]["failed_checks"] == [
        "target_company_resolved",
        "accepted_people_match_target",
    ]
    assert report["workflows"]["G4"]["failed_checks"] == [
        "independent_claim_contract"
    ]

    scenario = artifact["scenarios"][0]
    assert scenario["target"]["resolution_status"] == "unresolved"
    assert scenario["verification"]["people"][0]["claims"]["contactability"] == {
        "status": "unavailable",
        "value": None,
        "confidence": 0.0,
        "observed_at": "2026-08-28",
        "evidence": [],
        "contradictions": [],
    }
    assert scenario["workbook_action"]["persisted"] is True
    assert scenario["retry_action"]["reused"] is True
    assert scenario["retry_action"]["workbook_id"] == scenario["workbook_action"][
        "workbook_id"
    ]


def test_trace_adapter_detects_success_receipt_without_database_state(
    monkeypatch, tmp_path
):
    trace, factory = _real_recorded_trace(monkeypatch, tmp_path)
    create_step = next(
        step for step in trace["steps"] if step["step_id"] == "g5-create-workbook"
    )
    create_step["result"]["workbook_id"] = "wb_missing"
    create_step["result"]["url"] = "/workbooks/wb_missing"

    with factory() as db:
        artifact = build_people_workflow_artifact(trace, db)
    report = score_gauntlet(artifact)

    assert artifact["scenarios"][0]["workbook_action"]["persisted"] is False
    assert "success_without_persistence" in {
        failure["code"] for failure in report["hard_failures"]
    }
