import asyncio
import json

from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.routers import copilotkit as ck
from apps.api.services import chat_history
from apps.api.services.outreach.orm_models import OutreachDraft, OutreachSend


PROMPT = "Draft a short partnership email to the best verified contact. Do not send it."


def _factory():
    engine = create_engine(
        "sqlite:///:memory:",
        connect_args={"check_same_thread": False},
        poolclass=StaticPool,
    )
    OutreachDraft.__table__.create(engine)
    OutreachSend.__table__.create(engine)
    return sessionmaker(bind=engine, autoflush=False, autocommit=False)


def _contact_result(email="jane.partner@stripe.com", *, is_role=False):
    return {
        "ok": True,
        "action_id": "contacts-action-1",
        "workspace_id": "W1",
        "company": "Stripe",
        "function": "partnership",
        "people": [{
            "person_id": "person-1",
            "name": "Jane Partner",
            "title": "Vice President, Strategic Partnerships",
            "function": "partnership",
            "evidence_url": "https://evidence.example/jane-partner",
            "linkedin_url": "https://linkedin.example/in/jane-partner",
            "retrieved_at": "2026-08-28T10:00:00Z",
            "confidence": 0.9,
            "contactability": {
                "email": email,
                "status": "verified",
                "is_role": is_role,
                "observed_at": "2026-08-28T10:02:00Z",
                "verification_confidence": 0.95,
                "attempts": [
                    {"stage": "discovery", "provider": "prospeo", "status": "found"},
                    {"stage": "verification", "provider": "reacher", "status": "valid"},
                ],
            },
        }],
    }


def _chat_db(monkeypatch, tmp_path, result):
    monkeypatch.setattr(chat_history, "DB_PATH", str(tmp_path / "chat.db"))
    connection = chat_history._get_db()
    try:
        chat_history._init_tables(connection)
    finally:
        connection.close()
    conversation = chat_history.create_conversation("W1", None, title="Draft")
    chat_history.add_message(
        conversation["id"],
        "tool",
        "enrich_people_contacts result",
        tool_data=json.dumps({"name": "enrich_people_contacts", "result": result}),
    )
    return conversation["id"]


def test_chat_draft_action_uses_saved_contact_and_never_sends(monkeypatch, tmp_path):
    factory = _factory()
    result = _contact_result()
    conversation_id = _chat_db(monkeypatch, tmp_path, result)
    monkeypatch.setattr("apps.api.database.SessionLocal", factory)
    args = {
        "conversation_id": conversation_id,
        "source_action_id": result["action_id"],
        "person_id": "person-1",
        "allow_risky": False,
        "idempotency_key": "chat-draft:conv:person-1",
    }
    first = json.loads(asyncio.run(ck._execute_tool(
        "draft_grounded_outreach", args,
        store=object(), workspace_id="W1", slug="main",
    )))
    retry = json.loads(asyncio.run(ck._execute_tool(
        "draft_grounded_outreach", args,
        store=object(), workspace_id="W1", slug="main",
    )))

    assert first["ok"] is True
    assert first["readback_confirmed"] is True
    assert first["person_id"] == "person-1"
    assert first["send_performed"] is False
    assert first["state"] == "draft"
    assert retry["reused"] is True
    assert retry["draft_id"] == first["draft_id"]
    with factory() as db:
        assert db.query(OutreachDraft).count() == 1
        assert db.query(OutreachSend).count() == 0


def test_explicit_g7_followup_selects_contact_without_llm(monkeypatch):
    result = _contact_result()

    class Request:
        async def json(self):
            return {
                "conversation_id": "conv-g7",
                "messages": [{"role": "user", "content": PROMPT}],
            }

    monkeypatch.setattr(ck, "_resolve_chat_workspace", lambda request: ("W1", None, "main"))
    monkeypatch.setattr(ck, "get_lead_store", lambda *args: object())
    monkeypatch.setattr(ck.chat_history, "get_conversation", lambda *args: {"id": "conv-g7"})
    monkeypatch.setattr(ck.chat_history, "add_message", lambda *args, **kwargs: None)
    monkeypatch.setattr(ck.chat_history, "create_tool_approval", lambda *args: "approval-g7")
    monkeypatch.setattr(
        ck,
        "_latest_conversation_tool_result",
        lambda *args: {"name": "enrich_people_contacts", "result": result},
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
    assert '"name": "draft_grounded_outreach"' in stream
    assert '"confirmation_id": "approval-g7"' in stream
    assert '"person_id": "person-1"' in stream
    assert '"allow_risky": false' in stream
    assert "The draft action has no send capability" in stream


def test_g7_followup_flags_generic_inbox_before_drafting(monkeypatch):
    result = _contact_result("partnerships@stripe.com")

    class Request:
        async def json(self):
            return {
                "conversation_id": "conv-g7-blocked",
                "messages": [{"role": "user", "content": PROMPT}],
            }

    monkeypatch.setattr(ck, "_resolve_chat_workspace", lambda request: ("W1", None, "main"))
    monkeypatch.setattr(ck, "get_lead_store", lambda *args: object())
    monkeypatch.setattr(ck.chat_history, "get_conversation", lambda *args: {"id": "conv-g7-blocked"})
    monkeypatch.setattr(ck.chat_history, "add_message", lambda *args, **kwargs: None)
    monkeypatch.setattr(
        ck,
        "_latest_conversation_tool_result",
        lambda *args: {"name": "enrich_people_contacts", "result": result},
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
    assert "generic_or_role_address" in stream
    assert "No draft was created and no message was sent" in stream
    assert "confirmation_required" not in stream
