import asyncio
import copy
import json
from pathlib import Path

from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.routers import copilotkit as ck
from apps.api.services import chat_history
from apps.api.services.evaluation.gtm_gauntlet import load_artifact, score_gauntlet
from apps.api.services.evaluation.outreach_trace import build_grounded_outreach_artifact
from apps.api.services.outreach.orm_models import OutreachDraft, OutreachSend


PEOPLE_FIXTURE = (
    Path(__file__).parent / "fixtures" / "gtm_gauntlet" / "partnership_people_pass.json"
)


def _factory():
    engine = create_engine(
        "sqlite:///:memory:",
        connect_args={"check_same_thread": False},
        poolclass=StaticPool,
    )
    OutreachDraft.__table__.create(engine)
    OutreachSend.__table__.create(engine)
    return sessionmaker(bind=engine, autoflush=False, autocommit=False)


def _contact_result():
    return {
        "ok": True,
        "action_id": "contacts-g7",
        "workspace_id": "W1",
        "company": "Stripe",
        "function": "partnership",
        "people": [{
            "person_id": "person-g7",
            "name": "Jane Partner",
            "title": "Vice President, Strategic Partnerships",
            "function": "partnership",
            "evidence_url": "https://evidence.example/stripe/jane",
            "linkedin_url": "https://linkedin.example/in/jane",
            "retrieved_at": "2026-08-28T10:00:00Z",
            "confidence": 0.9,
            "contactability": {
                "email": "jane.partner@stripe.com",
                "status": "verified",
                "is_role": False,
                "observed_at": "2026-08-28T10:02:00Z",
                "verification_confidence": 0.95,
                "attempts": [
                    {"stage": "discovery", "provider": "prospeo", "status": "found"},
                    {"stage": "verification", "provider": "reacher", "status": "valid"},
                ],
            },
        }],
    }


def _action(args, *, retry=False):
    result = json.loads(asyncio.run(ck._execute_tool(
        "draft_grounded_outreach", args,
        store=object(), workspace_id="W1", slug="main",
    )))
    return {
        "step_id": "g7-retry" if retry else "g7-create",
        "workflow_id": "G7",
        "tool_name": "draft_grounded_outreach",
        "status": "succeeded" if result.get("ok") else "failed",
        "latency_ms": 55 if retry else 180,
        "args": args,
        "approval": {"decision": "approved"},
        "result": result,
    }


def _passing_artifact(monkeypatch, tmp_path):
    factory = _factory()
    monkeypatch.setattr("apps.api.database.SessionLocal", factory)
    monkeypatch.setattr(chat_history, "DB_PATH", str(tmp_path / "chat.db"))
    connection = chat_history._get_db()
    try:
        chat_history._init_tables(connection)
    finally:
        connection.close()
    conversation = chat_history.create_conversation("W1", None, title="G7")
    contacts = _contact_result()
    chat_history.add_message(
        conversation["id"],
        "tool",
        "enrich_people_contacts result",
        tool_data=json.dumps({"name": "enrich_people_contacts", "result": contacts}),
    )
    args = {
        "conversation_id": conversation["id"],
        "source_action_id": contacts["action_id"],
        "person_id": "person-g7",
        "allow_risky": False,
        "idempotency_key": "chat-draft:recorded-g7",
    }
    trace = {
        "fixture_kind": "synthetic_recorded_native_shape",
        "run_id": "recorded_product_g7",
        "mode": "recorded",
        "build_sha": "fixture-build",
        "workspace_id": "W1",
        "conversation_id": conversation["id"],
        "acknowledgement_ms": 100,
        "prompts": [
            "Draft a short partnership email to the best verified contact. Do not send it."
        ],
        "steps": [_action(args), _action(args, retry=True)],
        "source_contact": {
            "person_id": "person-g7",
            "email": "jane.partner@stripe.com",
            "contact_status": "verified",
            "valid_verifier_attempt": True,
            "risky_approved": False,
        },
        "jobs": [],
        "external_writes": [],
        "unresolved_issues": [],
        "production_run_history": [],
    }
    with factory() as db:
        g7 = build_grounded_outreach_artifact(trace, db)
    combined = copy.deepcopy(load_artifact(PEOPLE_FIXTURE))
    combined["run_id"] = "recorded_product_g2_g4_g5_g7"
    combined["scenarios"].extend(g7["scenarios"])
    return combined


def test_native_grounded_draft_and_saved_readback_pass_g7(monkeypatch, tmp_path):
    artifact = _passing_artifact(monkeypatch, tmp_path)
    report = score_gauntlet(artifact)

    assert report["score"] == 100.0
    assert report["hard_failures"] == []
    assert report["run_passed"] is True
    assert report["workflows"]["G7"] == {"status": "passed", "failed_checks": []}


def test_ungrounded_personalization_is_a_hard_failure(monkeypatch, tmp_path):
    artifact = _passing_artifact(monkeypatch, tmp_path)
    scenario = artifact["scenarios"][-1]
    personalized = next(
        item for item in scenario["draft_readback"]["sentence_evidence"]
        if item["personalized"]
    )
    personalized["evidence"] = []

    report = score_gauntlet(artifact)
    assert "ungrounded_personalized_sentence" in {
        item["code"] for item in report["hard_failures"]
    }
    assert "g7_sentence_grounding" in report["workflows"]["G7"]["failed_checks"]


def test_any_send_on_draft_only_request_is_a_hard_failure(monkeypatch, tmp_path):
    artifact = _passing_artifact(monkeypatch, tmp_path)
    scenario = artifact["scenarios"][-1]
    scenario["draft_readback"]["send_performed"] = True
    scenario["draft_readback"]["sent_at"] = "2026-08-28T10:03:00Z"
    scenario["external_writes"] = [{"type": "email", "status": "sent", "approved": True}]

    report = score_gauntlet(artifact)
    assert "draft_request_sent_message" in {
        item["code"] for item in report["hard_failures"]
    }
    assert "g7_draft_only" in report["workflows"]["G7"]["failed_checks"]


def test_generic_or_role_recipient_is_a_hard_failure(monkeypatch, tmp_path):
    artifact = _passing_artifact(monkeypatch, tmp_path)
    saved = artifact["scenarios"][-1]["draft_readback"]
    saved["generic_inbox"] = True
    saved["to_email"] = "partnerships@stripe.com"
    artifact["scenarios"][-1]["source_contact"]["email"] = "partnerships@stripe.com"

    report = score_gauntlet(artifact)
    assert "generic_outreach_recipient" in {
        item["code"] for item in report["hard_failures"]
    }
    assert "g7_non_generic_contact" in report["workflows"]["G7"]["failed_checks"]
