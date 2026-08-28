import asyncio
import json
from types import SimpleNamespace

from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.routers import copilotkit as ck
from apps.api.services.workbook.models import Base, Workbook


QUERY = "Find 20 B2B SaaS companies in India that use Stripe and are hiring partnership roles."


def test_create_account_discovery_workbook_is_structured_and_idempotent(monkeypatch):
    engine = create_engine(
        "sqlite:///:memory:",
        connect_args={"check_same_thread": False},
        poolclass=StaticPool,
    )
    Base.metadata.create_all(engine, tables=[Workbook.__table__])
    factory = sessionmaker(bind=engine, autoflush=False, autocommit=False)
    monkeypatch.setattr("apps.api.database.SessionLocal", factory)
    queued = []

    def fake_add_job(self, db, job_type, payload, priority=1, fire_key=None):
        queued.append({
            "job_type": job_type,
            "payload": payload,
            "fire_key": fire_key,
        })
        return SimpleNamespace(id=42)

    monkeypatch.setattr(
        "apps.api.services.queue_service.QueueService.add_job",
        fake_add_job,
    )
    args = {
        "icp_description": QUERY,
        "name": "Stripe Hiring Accounts",
        "target_rows": 999,
        "auto_run": True,
        "account_discovery": True,
        "idempotency_key": "account-action-stripe-india",
    }

    first = json.loads(asyncio.run(ck._execute_tool(
        "create_source_workbook",
        args,
        store=object(),
        workspace_id="W1",
        slug="main",
    )))
    second = json.loads(asyncio.run(ck._execute_tool(
        "create_source_workbook",
        args,
        store=object(),
        workspace_id="W1",
        slug="main",
    )))

    assert first["ok"] is True
    assert first["persisted"] is True
    assert first["reused"] is False
    assert first["source_job_id"] == 42
    assert first["brief"]["requested_count"] == 20
    assert second["workbook_id"] == first["workbook_id"]
    assert second["reused"] is True
    assert len(queued) == 1
    assert queued[0]["job_type"] == "source_workbook"
    assert queued[0]["payload"]["workspace_id"] == "W1"
    assert queued[0]["fire_key"].endswith(":account-action-stripe-india")

    with factory() as db:
        workbook = db.query(Workbook).one()
        source = next(
            column for column in workbook.columns_config
            if column.get("type") == "source"
        )
        assert workbook.source_type == "account_discovery"
        assert workbook.action_idempotency_key == "account-action-stripe-india"
        assert workbook.source_config["source_job_id"] == 42
        assert source["target_rows"] == 20
        assert source["account_discovery_brief"]["technologies"] == ["Stripe"]
        assert source["account_discovery_brief"]["hiring_roles"] == ["partnership"]

    conflict = json.loads(asyncio.run(ck._execute_tool(
        "create_source_workbook",
        {
            **args,
            "icp_description": "Find 10 Fintech companies in India.",
        },
        store=object(),
        workspace_id="W1",
        slug="main",
    )))
    assert conflict == {
        "error": "Idempotency key conflicts with a different sourcing brief",
        "action_id": "account-action-stripe-india",
    }
    assert len(queued) == 1


def test_source_queue_failure_is_truthful_and_retryable(monkeypatch):
    engine = create_engine(
        "sqlite:///:memory:",
        connect_args={"check_same_thread": False},
        poolclass=StaticPool,
    )
    Base.metadata.create_all(engine, tables=[Workbook.__table__])
    factory = sessionmaker(bind=engine, autoflush=False, autocommit=False)
    monkeypatch.setattr("apps.api.database.SessionLocal", factory)
    attempts = {"count": 0}

    def flaky_add_job(self, *args, **kwargs):
        attempts["count"] += 1
        if attempts["count"] == 1:
            raise RuntimeError("recorded queue outage")
        return SimpleNamespace(id=88)

    monkeypatch.setattr(
        "apps.api.services.queue_service.QueueService.add_job",
        flaky_add_job,
    )
    args = {
        "icp_description": QUERY,
        "auto_run": True,
        "account_discovery": True,
        "idempotency_key": "account-action-recover",
    }

    first = json.loads(asyncio.run(ck._execute_tool(
        "create_source_workbook", args, store=object(), workspace_id="W1", slug="main",
    )))
    retry = json.loads(asyncio.run(ck._execute_tool(
        "create_source_workbook", args, store=object(), workspace_id="W1", slug="main",
    )))

    assert first["ok"] is False
    assert first["persisted"] is True
    assert first["sourcing"] is False
    assert first["error"] == "Source queue unavailable"
    assert retry["ok"] is True
    assert retry["reused"] is True
    assert retry["source_job_id"] == 88
    assert attempts["count"] == 2
    with factory() as db:
        workbook = db.query(Workbook).one()
        assert workbook.source_config["source_job_id"] == 88
        assert workbook.source_config["source_queue_status"] == "queued"
        assert "source_queue_error_class" not in workbook.source_config


def test_account_discovery_requires_complete_server_parsed_brief(monkeypatch):
    result = json.loads(asyncio.run(ck._execute_tool(
        "create_source_workbook",
        {
            "icp_description": "Find B2B SaaS companies in India",
            "account_discovery": True,
        },
        store=object(),
        workspace_id="W1",
        slug="main",
    )))

    assert result["error"] == "Account discovery brief is incomplete"
    assert result["missing_fields"] == ["requested_count"]


def test_explicit_g1_prompt_emits_confirmation_without_llm(monkeypatch):
    class Request:
        async def json(self):
            return {
                "conversation_id": "conv-accounts",
                "messages": [{"role": "user", "content": QUERY}],
            }

    monkeypatch.setattr(ck, "_resolve_chat_workspace", lambda request: ("W1", None, "main"))
    monkeypatch.setattr(ck, "get_lead_store", lambda *args: object())
    monkeypatch.setattr(
        ck.chat_history, "get_conversation", lambda *args: {"id": "conv-accounts"}
    )
    monkeypatch.setattr(ck.chat_history, "add_message", lambda *args, **kwargs: None)
    monkeypatch.setattr(ck.chat_history, "create_tool_approval", lambda *args: "approval-accounts")
    monkeypatch.setattr(ck, "_latest_conversation_tool_result", lambda *args: None)
    monkeypatch.setattr(
        ck,
        "_get_provider_chain",
        lambda: (_ for _ in ()).throw(AssertionError("LLM provider was consulted")),
    )

    async def run():
        response = await ck.copilot_chat(Request())
        chunks = []
        async for chunk in response.body_iterator:
            chunks.append(chunk.decode() if isinstance(chunk, bytes) else chunk)
        return "".join(chunks)

    stream = asyncio.run(run())
    assert '"name": "create_source_workbook"' in stream
    assert '"confirmation_id": "approval-accounts"' in stream
    assert '"target_rows": 20' in stream
    assert '"account_discovery": true' in stream
    assert '"idempotency_key": "chat-accounts:conv-accounts:accounts_' in stream
    assert "Rows must prove every requested filter" in stream
