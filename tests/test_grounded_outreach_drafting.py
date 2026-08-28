import copy

import pytest
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.services.outreach.drafting import (
    DraftingError,
    create_grounded_draft,
    select_best_contact,
)
from apps.api.services.outreach.orm_models import OutreachDraft


def _factory():
    engine = create_engine(
        "sqlite:///:memory:",
        connect_args={"check_same_thread": False},
        poolclass=StaticPool,
    )
    OutreachDraft.__table__.create(engine)
    return sessionmaker(bind=engine, autoflush=False, autocommit=False)


def _person(
    person_id="person-1",
    name="Jane Partner",
    title="Vice President, Strategic Partnerships",
    email="jane.partner@stripe.com",
    status="verified",
    *,
    is_role=False,
):
    attempts = [
        {"stage": "discovery", "provider": "prospeo", "status": "found"},
        {"stage": "verification", "provider": "reacher", "status": "valid"},
    ]
    if status != "verified":
        attempts[-1]["status"] = "unknown"
    return {
        "person_id": person_id,
        "name": name,
        "title": title,
        "function": "partnership",
        "linkedin_url": f"https://linkedin.example/in/{person_id}",
        "evidence_url": f"https://evidence.example/people/{person_id}",
        "retrieved_at": "2026-08-28T10:00:00Z",
        "confidence": 0.9,
        "contactability": {
            "email": email,
            "status": status,
            "is_role": is_role,
            "observed_at": "2026-08-28T10:02:00Z",
            "verification_confidence": 0.95,
            "attempts": attempts,
        },
    }


def _source(*people):
    return {
        "action_id": "contact-action-1",
        "company": "Stripe",
        "function": "partnership",
        "people": list(people),
    }


def _create(db, source, **overrides):
    args = {
        "workspace_id": "W1",
        "conversation_id": "conv-1",
        "source_result": source,
        "source_action_id": source["action_id"],
        "requested_person_id": "",
        "allow_risky": False,
        "idempotency_key": "draft-action-1",
    }
    args.update(overrides)
    return create_grounded_draft(db, **args)


def test_best_verified_contact_is_persisted_with_sentence_evidence():
    factory = _factory()
    junior = _person("person-junior", "Jamie Partner", "Partnership Manager", "jamie@stripe.com")
    senior = _person("person-senior", "Jane Partner", "Vice President, Strategic Partnerships", "jane@stripe.com")
    with factory() as db:
        first = _create(db, _source(junior, senior))
        retry = _create(db, _source(junior, senior))

        assert first["persisted"] is True
        assert first["readback_confirmed"] is True
        assert first["person_id"] == "person-senior"
        assert first["contact_status"] == "verified"
        assert first["generic_inbox"] is False
        assert first["is_role_address"] is False
        assert first["state"] == "draft"
        assert first["send_performed"] is False
        assert first["sent_at"] is None
        assert first["url"].endswith(first["draft_id"])
        personalized = [
            item for item in first["sentence_evidence"] if item["personalized"]
        ]
        assert personalized
        assert all(item["claim_ids"] and item["evidence"] for item in personalized)
        assert all(
            evidence["source_url"].startswith("https://")
            for item in personalized
            for evidence in item["evidence"]
        )
        assert retry["reused"] is True
        assert retry["draft_id"] == first["draft_id"]
        assert db.query(OutreachDraft).count() == 1


def test_risky_address_requires_separate_approval():
    factory = _factory()
    risky = _person(status="risky")
    with factory() as db:
        with pytest.raises(DraftingError) as blocked:
            _create(db, _source(risky))
        assert blocked.value.flags[0]["reason"] == "risky_address_requires_approval"

        created = _create(
            db,
            _source(risky),
            allow_risky=True,
            idempotency_key="draft-risky-approved",
        )
        assert created["contact_status"] == "risky"
        assert created["risky_approved"] is True


@pytest.mark.parametrize(
    ("email", "is_role"),
    [("partnerships@stripe.com", False), ("jane.partner@stripe.com", True)],
)
def test_generic_and_role_accounts_are_flagged_before_drafting(email, is_role):
    factory = _factory()
    person = _person(email=email, is_role=is_role)
    with factory() as db:
        with pytest.raises(DraftingError) as blocked:
            _create(db, _source(person))
        flag = blocked.value.flags[0]
        assert flag["reason"] == "generic_or_role_address"
        assert flag["generic_inbox"] is (email.startswith("partnerships@"))
        assert flag["is_role_address"] is is_role
        assert db.query(OutreachDraft).count() == 0


def test_fabricated_verified_status_and_missing_public_evidence_fail_closed():
    source = _source(_person())
    source["people"][0]["contactability"]["attempts"][-1]["status"] = "unknown"
    with pytest.raises(DraftingError) as fabricated:
        select_best_contact(source)
    assert fabricated.value.flags[0]["reason"] == "verified_without_valid_verifier"

    missing = _person()
    missing["evidence_url"] = ""
    missing["linkedin_url"] = ""
    with pytest.raises(DraftingError) as ungrounded:
        select_best_contact(_source(missing))
    assert ungrounded.value.flags[0]["reason"] == "personalization_evidence_missing"

    undated = _person()
    undated.pop("retrieved_at")
    undated["contactability"].pop("observed_at")
    with pytest.raises(DraftingError) as stale:
        select_best_contact(_source(undated))
    assert stale.value.flags[0]["reason"] == "personalization_evidence_missing"


def test_idempotency_key_conflict_is_rejected():
    factory = _factory()
    with factory() as db:
        _create(db, _source(_person()))
        changed = copy.deepcopy(_source(_person(email="jane.changed@stripe.com")))
        with pytest.raises(DraftingError, match="different draft request"):
            _create(db, changed)
