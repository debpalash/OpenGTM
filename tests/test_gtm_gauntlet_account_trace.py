import asyncio
import copy
import json
from pathlib import Path
from types import SimpleNamespace

from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.routers import copilotkit as ck
from apps.api.services import chat_history
from apps.api.services.evaluation.account_trace import (
    build_account_discovery_artifact,
)
from apps.api.services.evaluation.gtm_gauntlet import load_artifact, score_gauntlet
from apps.api.services.workbook.models import Base, Workbook, WorkbookRow


PEOPLE_FIXTURE = (
    Path(__file__).parent
    / "fixtures"
    / "gtm_gauntlet"
    / "partnership_people_pass.json"
)
QUERY = "Find 20 B2B SaaS companies in India that use Stripe and are hiring partnership roles."


def _events(generator):
    async def collect():
        result = []
        async for line in generator:
            if line.startswith("data: ") and line.strip() != "data: [DONE]":
                result.append(json.loads(line[6:]))
        return result

    return asyncio.run(collect())


def _approved_action(trace, *, retry=False):
    args = {
        "icp_description": QUERY,
        "name": "20 B2B SaaS Accounts in India",
        "target_rows": 20,
        "auto_run": True,
        "auto_enrich": False,
        "account_discovery": True,
        "idempotency_key": "chat-accounts:recorded-g1",
    }
    tool_call = {
        "id": "g1-retry" if retry else "g1-create",
        "type": "function",
        "function": {
            "name": "create_source_workbook",
            "arguments": json.dumps(args),
        },
    }
    approval_id = chat_history.create_tool_approval(
        trace["workspace_id"], None, tool_call
    )
    events = _events(ck._resolve_approved_calls(
        [],
        [{"tool_call": {"id": approval_id}, "decision": "approve"}],
        store=object(),
        workspace_id=trace["workspace_id"],
        slug="main",
    ))
    result = next(
        event["tool_result"]["result"] for event in events
        if event.get("tool_result", {}).get("name") == "create_source_workbook"
    )
    return {
        "step_id": "g1-retry" if retry else "g1-create",
        "workflow_id": "G1",
        "tool_name": "create_source_workbook",
        "status": "succeeded" if result.get("ok") else "failed",
        "latency_ms": 80 if retry else 350,
        "args": args,
        "approval": {"approval_id": approval_id, "decision": "approved"},
        "result": result,
    }


def _passing_artifact(monkeypatch, tmp_path):
    engine = create_engine(
        "sqlite:///:memory:",
        connect_args={"check_same_thread": False},
        poolclass=StaticPool,
    )
    Base.metadata.create_all(engine, tables=[Workbook.__table__, WorkbookRow.__table__])
    factory = sessionmaker(bind=engine, autoflush=False, autocommit=False)
    monkeypatch.setattr("apps.api.database.SessionLocal", factory)
    monkeypatch.setattr(chat_history, "DB_PATH", str(tmp_path / "chat.db"))
    connection = chat_history._get_db()
    try:
        chat_history._init_tables(connection)
    finally:
        connection.close()
    monkeypatch.setattr(
        "apps.api.services.queue_service.QueueService.add_job",
        lambda self, *args, **kwargs: SimpleNamespace(id=77),
    )
    trace = {
        "schema_version": "1.0",
        "fixture_kind": "synthetic_recorded_native_shape",
        "run_id": "recorded_product_g1",
        "mode": "recorded",
        "build_sha": "fixture-build",
        "started_at": "2026-08-28T10:00:00Z",
        "finished_at": "2026-08-28T10:01:00Z",
        "workspace_id": "W1",
        "conversation_id": "conv-g1",
        "acknowledgement_ms": 160,
        "sourcing_latency_ms": 45_000,
        "prompts": [QUERY],
        "steps": [],
        "jobs": [],
        "external_writes": [],
        "unresolved_issues": [],
        "production_run_history": [],
    }
    trace["steps"].append(_approved_action(trace))
    workbook_id = trace["steps"][0]["result"]["workbook_id"]

    with factory() as db:
        workbook = db.query(Workbook).filter(Workbook.id == workbook_id).one()
        brief = workbook.source_config["account_discovery_brief"]
        workbook.total_rows = 20
        workbook.source_config = {
            **workbook.source_config,
            "last_source_run": {
                "status": "complete",
                "requested_count": 20,
                "delivered_count": 20,
                "shortfall": 0,
                "found_count": 25,
                "added_count": 20,
                "skipped_count": 5,
                "rejected_by_reason": {"technology_evidence_missing": 5},
                "exhausted_sources": [
                    {"source": "web", "status": "completed", "output_count": 25},
                ],
                "retry_options": [],
                "job_id": "lead-job-g1",
            },
        }
        for index in range(20):
            domain = f"account-{index}.example"
            criteria = {
                "company_type:B2B SaaS": {"matched": True},
                "geography:India": {"matched": True},
                "technology:Stripe": {"matched": True},
                "hiring:partnership": {"matched": True},
            }
            db.add(WorkbookRow(
                workbook_id=workbook_id,
                workspace_id="W1",
                position=index,
                canonical_entity_id=f"account_{index}",
                source_provider="recorded_search",
                source_record_id=domain,
                data={
                    "account_id": f"account_{index}",
                    "company": f"Fixture Account {index}",
                    "canonical_domain": domain,
                    "fit_reasons": [
                        "Company type evidence matches B2B SaaS",
                        "Geography evidence matches India",
                        "Technology evidence matches Stripe",
                        "Hiring evidence matches partnership",
                    ],
                    "criteria_evidence": criteria,
                    "evidence_urls": [f"https://evidence.example/accounts/{index}"],
                    "retrieved_at": "2026-08-28T10:00:30Z",
                    "field_confidence": 0.9,
                },
            ))
        db.commit()
        assert brief["brief_id"].startswith("accounts_")

    trace["steps"].append(_approved_action(trace, retry=True))
    with factory() as db:
        g1_artifact = build_account_discovery_artifact(trace, db)

    combined = copy.deepcopy(load_artifact(PEOPLE_FIXTURE))
    combined["run_id"] = "recorded_product_g1_g2_g4_g5"
    combined["scenarios"] = [
        g1_artifact["scenarios"][0],
        *combined["scenarios"],
    ]
    return combined


def test_native_account_action_and_persisted_rows_pass_g1(monkeypatch, tmp_path):
    combined = _passing_artifact(monkeypatch, tmp_path)
    report = score_gauntlet(combined)

    assert report["score"] == 100.0
    assert report["hard_failures"] == []
    assert report["run_passed"] is True
    assert report["workflows"]["G1"]["status"] == "passed"
    assert report["workflows"]["G1"]["failed_checks"] == []
    scenario = combined["scenarios"][0]
    assert scenario["account_action"]["persisted"] is True
    assert scenario["account_retry_action"]["reused"] is True
    assert scenario["source_run"]["delivered_count"] == 20
    assert len(scenario["accounts"]) == 20


def test_false_complete_account_run_is_a_hard_failure(monkeypatch, tmp_path):
    artifact = copy.deepcopy(load_artifact(PEOPLE_FIXTURE))
    scenario = {
        "id": "bad-g1",
        "status": "completed",
        "workflow_ids": ["G1"],
        "workspace_id": "W1",
        "brief": {
            "complete": True,
            "requested_count": 20,
            "company_types": ["B2B SaaS"],
            "geographies": ["India"],
            "technologies": ["Stripe"],
            "hiring_roles": ["partnership"],
            "evidence_requirements": [
                "canonical_company_domain", "criterion_evidence",
                "evidence_url", "retrieved_at", "field_confidence",
            ],
            "missing_fields": [],
            "brief_id": "accounts_bad",
        },
        "account_action": {
            "status": "succeeded", "persisted": True, "approved": True,
            "workbook_id": "wb_bad", "url": "/workbooks/wb_bad",
            "action_id": "bad", "idempotency_key": "bad", "row_count": 1,
            "source_job_id": 1, "workspace_id": "W1",
        },
        "account_retry_action": {
            "status": "succeeded", "persisted": True, "approved": True,
            "reused": True, "workbook_id": "wb_bad", "url": "/workbooks/wb_bad",
            "action_id": "bad", "idempotency_key": "bad", "row_count": 1,
            "source_job_id": 1, "workspace_id": "W1",
        },
        "source_run": {
            "status": "complete", "requested_count": 20,
            "delivered_count": 1, "shortfall": 0,
            "rejected_by_reason": {}, "exhausted_sources": [], "retry_options": [],
        },
        "accounts": [],
        "can_continue_enrichment": False,
        "timings_ms": {
            "acknowledgement": 100, "workbook_creation": 100,
            "account_sourcing": 1000,
        },
        "jobs": [],
        "external_writes": [],
    }
    artifact["scenarios"].insert(0, scenario)

    report = score_gauntlet(artifact)

    assert "false_complete_account_run" in {
        failure["code"] for failure in report["hard_failures"]
    }
    assert report["workflows"]["G1"]["status"] == "failed"


def test_honest_partial_account_run_reports_recovery_without_hard_failure():
    artifact = copy.deepcopy(load_artifact(PEOPLE_FIXTURE))
    scenario = {
        "id": "partial-g1",
        "status": "partial",
        "workflow_ids": ["G1"],
        "workspace_id": "W1",
        "brief": {
            "complete": True,
            "requested_count": 20,
            "company_types": ["B2B SaaS"],
            "geographies": ["India"],
            "technologies": ["Stripe"],
            "hiring_roles": ["partnership"],
            "evidence_requirements": [
                "canonical_company_domain", "criterion_evidence",
                "evidence_url", "retrieved_at", "field_confidence",
            ],
            "missing_fields": [],
            "brief_id": "accounts_partial",
        },
        "account_action": {
            "status": "succeeded", "persisted": True, "approved": True,
            "workbook_id": "wb_partial", "url": "/workbooks/wb_partial",
            "action_id": "partial", "idempotency_key": "partial", "row_count": 0,
            "source_job_id": 1, "workspace_id": "W1",
        },
        "account_retry_action": {
            "status": "succeeded", "persisted": True, "approved": True,
            "reused": True, "workbook_id": "wb_partial",
            "url": "/workbooks/wb_partial", "action_id": "partial",
            "idempotency_key": "partial", "row_count": 0,
            "source_job_id": 1, "workspace_id": "W1",
        },
        "source_run": {
            "status": "partial", "requested_count": 20,
            "delivered_count": 0, "shortfall": 20,
            "rejected_by_reason": {"technology_evidence_missing": 8},
            "exhausted_sources": [
                {"source": "web", "status": "completed", "output_count": 8},
            ],
            "retry_options": ["retry_same_brief", "relax_one_filter"],
        },
        "accounts": [],
        "can_continue_enrichment": False,
        "timings_ms": {
            "acknowledgement": 100, "workbook_creation": 100,
            "account_sourcing": 1000,
        },
        "jobs": [],
        "external_writes": [],
    }
    artifact["scenarios"].insert(0, scenario)

    report = score_gauntlet(artifact)

    assert report["hard_failures"] == []
    assert report["workflows"]["G1"]["status"] == "failed"
    assert "scenario_completed" in report["workflows"]["G1"]["failed_checks"]
