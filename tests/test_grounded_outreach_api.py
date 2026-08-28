import uuid

import pytest
from fastapi import HTTPException
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.core.tenancy import WorkspaceCtx
from apps.api.routers.outreach import get_draft, list_drafts, router
from apps.api.services.outreach.orm_models import OutreachDraft


def _factory():
    engine = create_engine(
        "sqlite:///:memory:",
        connect_args={"check_same_thread": False},
        poolclass=StaticPool,
    )
    OutreachDraft.__table__.create(engine)
    return sessionmaker(bind=engine, autoflush=False, autocommit=False)


def _ctx(workspace_id: str) -> WorkspaceCtx:
    return WorkspaceCtx(user=None, workspace_id=workspace_id, slug="main")  # type: ignore[arg-type]


def _draft(workspace_id: str, *, name: str) -> OutreachDraft:
    return OutreachDraft(
        id=str(uuid.uuid4()),
        workspace_id=workspace_id,
        action_idempotency_key=f"draft:{workspace_id}:{name}",
        conversation_id="conversation-1",
        source_action_id="contacts-1",
        person_id=f"person-{name}",
        person_name=name,
        company="Stripe",
        title="VP Partnerships",
        to_email=f"{name.lower()}@stripe.com",
        contact_status="verified",
        risky_approved=False,
        generic_inbox=False,
        is_role_address=False,
        subject="Partnership idea for Stripe",
        body_text=f"Hi {name},\n\nWould you be open to a conversation?",
        sentence_evidence=[{
            "sentence_id": "s1",
            "text": f"Hi {name},",
            "personalized": True,
            "claim_ids": ["claim-person"],
            "evidence": [{
                "source_url": "https://example.com/profile",
                "label": "Public profile",
                "observed_at": "2026-08-28T10:00:00Z",
                "confidence": 0.9,
            }],
        }],
        source_snapshot={"contract_hash": f"hash-{name}"},
        state="draft",
    )


def test_draft_read_api_is_tenant_scoped_and_includes_evidence_only_on_detail():
    factory = _factory()
    with factory() as db:
        own = _draft("W1", name="Jane")
        other = _draft("W2", name="Alex")
        db.add_all([own, other])
        db.commit()

        listed = list_drafts(limit=50, offset=0, db=db, ctx=_ctx("W1"))
        assert [item["id"] for item in listed["drafts"]] == [own.id]
        assert "sentence_evidence" not in listed["drafts"][0]
        assert listed["drafts"][0]["send_performed"] is False

        detail = get_draft(own.id, db=db, ctx=_ctx("W1"))
        assert detail["sentence_evidence"][0]["claim_ids"] == ["claim-person"]
        assert detail["source_snapshot"] == {"contract_hash": "hash-Jane"}

        with pytest.raises(HTTPException) as hidden:
            get_draft(other.id, db=db, ctx=_ctx("W1"))
        assert hidden.value.status_code == 404


def test_grounded_drafts_have_no_http_create_or_send_route():
    draft_routes = {
        (route.path, method)
        for route in router.routes
        if "/drafts" in route.path
        for method in (route.methods or set())
    }
    assert draft_routes == {
        ("/api/outreach/drafts", "GET"),
        ("/api/outreach/drafts/{draft_id}", "GET"),
    }
