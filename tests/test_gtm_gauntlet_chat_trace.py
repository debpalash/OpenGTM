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
from apps.api.services.leadgen import people_contacts as pc
from apps.api.services.leadgen.enrichment.email_deliverability import (
    DeliverabilityResult,
)
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


def _execute_approved_contacts(trace: dict, *, retry: bool = False) -> dict:
    args = {
        "conversation_id": trace["conversation_id"],
        "person_ids": trace["selection"]["person_ids"],
        "idempotency_key": "chat-contacts:recorded-stripe-partners",
    }
    tool_call = {
        "id": "proposal-contact-retry" if retry else "proposal-contacts",
        "type": "function",
        "function": {
            "name": "enrich_people_contacts",
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
    result = _tool_result(events, "enrich_people_contacts")
    chat_history.add_message(
        trace["conversation_id"],
        "tool",
        "enrich_people_contacts result",
        tool_data=json.dumps({
            "name": "enrich_people_contacts",
            "result": result,
        }),
    )
    return {
        "step_id": "g3-retry-contacts" if retry else "g3-enrich-contacts",
        "workflow_id": "G3",
        "tool_name": "enrich_people_contacts",
        "status": "succeeded" if result.get("ok") else "failed",
        "latency_ms": 80 if retry else 1400,
        "args": args,
        "approval": {"approval_id": approval_id, "decision": "approved"},
        "result": result,
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

    provider_calls = []

    async def recorded_provider(name, lead, *, timeout):
        provider_calls.append({
            "provider": name,
            "person": lead.contact_person,
            "website": lead.website,
            "timeout": timeout,
        })
        if name != "prospeo":
            raise AssertionError("recorded first provider should satisfy the exact lookup")
        return {
            "provider": "prospeo",
            "success": True,
            "fields": {
                "email": "fixture.partner@stripe.com",
                "email_match_method": "linkedin",
            },
            "confidence": 0.9,
            "duration_ms": 120,
            "license": "proprietary-api",
        }

    async def recorded_verifier(email, *, workspace_id):
        assert email == "fixture.partner@stripe.com"
        assert workspace_id == trace["workspace_id"]
        return DeliverabilityResult(
            email=email,
            confidence="verified",
            status="valid",
            source="reacher",
            verification_confidence=0.95,
            verification_attempts=[{
                "provider": "reacher",
                "status": "valid",
                "detail": "recorded_fixture",
            }],
        )

    monkeypatch.setattr(pc, "_run_discovery_provider", recorded_provider)
    monkeypatch.setattr(pc, "_verify_discovered_email", recorded_verifier)
    trace["steps"].append(_execute_approved_contacts(trace))
    trace["steps"].append(_execute_approved_contacts(trace, retry=True))
    trace["recorded_contact_provider_calls"] = provider_calls
    trace["steps"].append(_execute_approved_workbook(trace))
    trace["steps"].append(_execute_approved_workbook(trace, retry=True))
    return trace, factory


def test_native_chat_trace_scores_g2_through_g5_with_persisted_state(
    monkeypatch, tmp_path
):
    trace, factory = _real_recorded_trace(monkeypatch, tmp_path)

    with factory() as db:
        artifact = build_people_workflow_artifact(trace, db)
    report = score_gauntlet(artifact)

    assert report["score"] == 100.0
    assert report["hard_failures"] == []
    assert report["run_passed"] is True
    assert report["workflows"]["G2"]["status"] == "passed"
    assert report["workflows"]["G3"]["status"] == "passed"
    assert report["workflows"]["G4"]["status"] == "passed"
    assert report["workflows"]["G5"]["status"] == "passed"
    assert report["workflows"]["G2"]["failed_checks"] == []
    assert report["workflows"]["G3"]["failed_checks"] == []
    assert report["workflows"]["G4"]["failed_checks"] == []
    assert len(trace["recorded_contact_provider_calls"]) == 1

    scenario = artifact["scenarios"][0]
    assert scenario["target"]["resolution_status"] == "resolved"
    assert scenario["target"]["canonical_domain"] == "stripe.com"
    assert scenario["verification"]["people"][0]["claims"]["company_identity"][
        "evidence"
    ] == [
        {
            "url": "https://www.wikidata.org/wiki/Q170120",
            "source": "wikidata",
        }
    ]
    contact_claim = scenario["verification"]["people"][0]["claims"]["contactability"]
    assert contact_claim["status"] == "verified"
    assert contact_claim["value"] == {
        "email": "fixture.partner@stripe.com",
        "contact_status": "verified",
    }
    assert [evidence["status"] for evidence in contact_claim["evidence"]] == [
        "found", "valid",
    ]
    assert scenario["contact_action"]["selected_person_ids"] == [
        "person_fixture_stripe_partner"
    ]
    assert scenario["contact_retry_action"]["reused"] is True
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
