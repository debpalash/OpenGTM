import asyncio
import json

from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.models import Job
from apps.api.routers import copilotkit as ck
from apps.api.services.poller.models import WatchSchedule, WatchSubscription
from apps.api.services.signals.tracking import ACCOUNT_SIGNAL_TYPES
from apps.api.services.workbook.models import Base, Workbook, WorkbookRow


PROMPT = (
    "Track these accounts weekly for partnership hiring, leadership changes, "
    "funding, and pricing-page changes."
)


def _factory():
    engine = create_engine(
        "sqlite:///:memory:",
        connect_args={"check_same_thread": False},
        poolclass=StaticPool,
    )
    Base.metadata.create_all(
        engine,
        tables=[
            Workbook.__table__, WorkbookRow.__table__, Job.__table__,
            WatchSubscription.__table__, WatchSchedule.__table__,
        ],
    )
    factory = sessionmaker(bind=engine, autoflush=False, autocommit=False)
    with factory() as db:
        db.add(Workbook(
            id="wb-g6", workspace_id="W1", name="Saved target accounts",
            source_type="account_discovery",
        ))
        for index in range(2):
            db.add(WorkbookRow(
                workbook_id="wb-g6", workspace_id="W1", position=index,
                canonical_entity_id=f"account_{index}",
                data={
                    "account_id": f"account_{index}",
                    "company": f"Account {index}",
                    "canonical_domain": f"account-{index}.example",
                },
            ))
        db.commit()
    return factory


def test_chat_signal_action_returns_saved_readback(monkeypatch):
    factory = _factory()
    monkeypatch.setattr(ck.settings, "INTENT_POLLER_ENABLED", True)
    monkeypatch.setattr(ck.settings, "PG_LEAD_STORE", True)
    monkeypatch.setattr("apps.api.database.SessionLocal", factory)
    args = {
        "workbook_id": "wb-g6",
        "account_ids": ["account_0", "account_1"],
        "cadence": "weekly",
        "signal_types": list(ACCOUNT_SIGNAL_TYPES),
        "idempotency_key": "chat-signals:conv-g6:selection",
    }
    first = json.loads(asyncio.run(ck._execute_tool(
        "track_account_signals", args, store=object(), workspace_id="W1", slug="main",
    )))
    retry = json.loads(asyncio.run(ck._execute_tool(
        "track_account_signals", args, store=object(), workspace_id="W1", slug="main",
    )))

    assert first["ok"] is True
    assert first["readback_confirmed"] is True
    assert first["scope"]["account_ids"] == args["account_ids"]
    assert first["manual_retry_action"]["url"].endswith("/poll")
    assert retry["reused"] is True
    assert retry["schedule_id"] == first["schedule_id"]


def test_explicit_g6_followup_binds_saved_rows_without_llm(monkeypatch):
    factory = _factory()
    monkeypatch.setattr(ck.settings, "INTENT_POLLER_ENABLED", True)
    monkeypatch.setattr(ck.settings, "PG_LEAD_STORE", True)

    class Request:
        async def json(self):
            return {
                "conversation_id": "conv-g6",
                "messages": [{"role": "user", "content": PROMPT}],
            }

    monkeypatch.setattr("apps.api.database.SessionLocal", factory)
    monkeypatch.setattr(ck, "_resolve_chat_workspace", lambda request: ("W1", None, "main"))
    monkeypatch.setattr(ck, "get_lead_store", lambda *args: object())
    monkeypatch.setattr(
        ck.chat_history, "get_conversation", lambda *args: {"id": "conv-g6"}
    )
    monkeypatch.setattr(ck.chat_history, "add_message", lambda *args, **kwargs: None)
    monkeypatch.setattr(ck.chat_history, "create_tool_approval", lambda *args: "approval-g6")
    monkeypatch.setattr(
        ck,
        "_latest_conversation_tool_result",
        lambda *args: {
            "name": "create_source_workbook",
            "result": {"workbook_id": "wb-g6"},
        },
    )
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
    assert '"name": "track_account_signals"' in stream
    assert '"confirmation_id": "approval-g6"' in stream
    assert '"account_ids": ["account_0", "account_1"]' in stream
    assert '"cadence": "weekly"' in stream
    assert '"pricing_page_change"' in stream
    assert "Repeating this request will update or return the same schedule" in stream


def test_explicit_g6_followup_refuses_missing_selection(monkeypatch):
    monkeypatch.setattr(ck.settings, "INTENT_POLLER_ENABLED", True)
    monkeypatch.setattr(ck.settings, "PG_LEAD_STORE", True)
    class Request:
        async def json(self):
            return {
                "conversation_id": "conv-g6-empty",
                "messages": [{"role": "user", "content": PROMPT}],
            }

    monkeypatch.setattr(ck, "_resolve_chat_workspace", lambda request: ("W1", None, "main"))
    monkeypatch.setattr(ck, "get_lead_store", lambda *args: object())
    monkeypatch.setattr(
        ck.chat_history, "get_conversation", lambda *args: {"id": "conv-g6-empty"}
    )
    monkeypatch.setattr(ck.chat_history, "add_message", lambda *args, **kwargs: None)
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
    assert "I have not created a schedule" in stream
    assert "confirmation_required" not in stream
