"""A shared or reused email must not glue different people together.

Regression coverage for the person resolution report: resolving a new name and
LinkedIn profile with an email another person already owned attached the new
profile and name to that person, so unrelated people became one.
"""

import pytest
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.database import Base
from apps.api.services.entities import people
from apps.api.services.entities.models import PersonEntity
from apps.api.services.entities.people import person_profile, resolve_person
from tests.entity_tables import PERSON_TABLES

WS = "ws"
SHARED = "shared@example.invalid"
ALICE_LI = "https://linkedin.com/in/synthetic-alice-repro"
BOB_LI = "https://linkedin.com/in/synthetic-bob-repro"


@pytest.fixture
def Session():
    engine = create_engine("sqlite:///:memory:", connect_args={"check_same_thread": False},
                           poolclass=StaticPool)
    Base.metadata.create_all(engine, tables=PERSON_TABLES)
    return sessionmaker(bind=engine, autoflush=False, autocommit=False)


def _alice(db, **kw):
    args = dict(workspace_id=WS, name="Synthetic Alice", company="Fixture", linkedin_url=ALICE_LI,
                email=SHARED, source="fixture")
    args.update(kw)
    return resolve_person(db, **args)


def _bob(db, **kw):
    args = dict(workspace_id=WS, name="Synthetic Bob", company="Fixture", linkedin_url=BOB_LI,
                email=SHARED, source="fixture")
    args.update(kw)
    return resolve_person(db, **args)


def _ids(db, person_id):
    return {(i["kind"], i["value"]) for i in person_profile(db, person_id, WS)["identifiers"]}


def test_a_different_name_and_profile_on_a_shared_email_is_a_different_person(Session):
    with Session() as db:
        alice, _ = _alice(db)
        bob, created = _bob(db)
        db.commit()

        assert created and bob.id != alice.id
        assert (alice.full_name, bob.full_name) == ("Synthetic Alice", "Synthetic Bob")
        # Nothing of Bob's is attached to Alice, and the mailbox stays with its owner.
        assert alice.identity_keys["linkedin"] == ["linkedin.com/in/synthetic-alice-repro"]
        assert alice.identity_keys["name_variants"] == ["Synthetic Alice"]
        assert _ids(db, alice.id) == {("linkedin", "linkedin.com/in/synthetic-alice-repro"), ("email", SHARED)}
        assert _ids(db, bob.id) == {("linkedin", "linkedin.com/in/synthetic-bob-repro")}
        assert "emails" not in bob.identity_keys


def test_the_conflict_is_recorded_on_both_people(Session):
    with Session() as db:
        alice, _ = _alice(db)
        bob, _ = _bob(db)
        db.commit()

        a_notes = person_profile(db, alice.id, WS)["identity_conflicts"]
        b_notes = person_profile(db, bob.id, WS)["identity_conflicts"]
        assert [(n["kind"], n["email"], n["other_person_id"]) for n in a_notes] == [("shared_email", SHARED, bob.id)]
        assert [(n["kind"], n["email"], n["other_person_id"]) for n in b_notes] == [("shared_email", SHARED, alice.id)]


def test_both_keep_resolving_to_themselves_afterwards(Session):
    with Session() as db:
        alice, _ = _alice(db)
        bob, _ = _bob(db)
        db.commit()

        bob_again, created = _bob(db)
        alice_by_profile, _ = _alice(db)
        alice_by_email, _ = resolve_person(db, workspace_id=WS, name="Synthetic Alice", company="Fixture",
                                           email=SHARED, source="fixture")
        db.commit()

        assert not created and bob_again.id == bob.id
        assert alice_by_profile.id == alice.id and alice_by_email.id == alice.id
        assert db.query(PersonEntity).filter_by(workspace_id=WS).count() == 2
        # Repeated observations neither duplicate the notes nor leak the email onto Bob.
        assert len(person_profile(db, alice.id, WS)["identity_conflicts"]) == 1
        assert len(person_profile(db, bob.id, WS)["identity_conflicts"]) == 1
        assert "emails" not in db.get(PersonEntity, bob.id).identity_keys


def test_a_changed_profile_slug_for_the_same_person_is_not_a_conflict(Session):
    with Session() as db:
        old, _ = _alice(db, name="Alice Smith")
        renamed, created = _alice(db, name="Alice Smith", linkedin_url="https://linkedin.com/in/alice-smith")
        db.commit()

        assert not created and renamed.id == old.id
        assert _ids(db, old.id) == {("linkedin", "linkedin.com/in/synthetic-alice-repro"),
                                    ("linkedin", "linkedin.com/in/alice-smith"), ("email", SHARED)}
        assert person_profile(db, old.id, WS)["identity_conflicts"] == []


def test_a_fuller_form_of_the_name_still_counts_as_the_same_person(Session):
    with Session() as db:
        first, _ = _alice(db, name="Alice Smith")
        again, created = _bob(db, name="A. Smith")   # initial, new profile, same mailbox
        assert not created and again.id == first.id


def test_the_people_search_flow_with_only_a_legacy_id_is_protected_too(Session):
    with Session() as db:
        alice, _ = resolve_person(db, workspace_id=WS, name="Synthetic Alice", company="Fixture",
                                  email=SHARED, source="people_search", legacy_ids=["person:alice"])
        bob, created = resolve_person(db, workspace_id=WS, name="Synthetic Bob", company="Fixture",
                                      email=SHARED, source="people_search", legacy_ids=["person:bob"])
        db.commit()

        assert created and bob.id != alice.id
        assert people._owner(db, WS, "legacy_id", "person:bob") == bob.id
        assert people._owner(db, WS, "legacy_id", "person:alice") == alice.id
        assert people._owner(db, WS, "email", SHARED) == alice.id


def test_an_email_alone_still_finds_its_owner(Session):
    """With no profile or id to separate them, the email is all there is to go on."""
    with Session() as db:
        alice, _ = _alice(db)
        other, created = resolve_person(db, workspace_id=WS, name="Synthetic Bob", company="Fixture",
                                        email=SHARED, source="fixture")
        assert not created and other.id == alice.id


def test_a_profile_resolved_with_an_email_someone_else_owns_does_not_take_it(Session):
    with Session() as db:
        alice, _ = _alice(db)
        bob, _ = resolve_person(db, workspace_id=WS, name="Synthetic Bob", company="Fixture",
                                linkedin_url=BOB_LI, source="fixture")
        # Bob's profile is later seen with Alice's mailbox: it stays Alice's.
        again, created = _bob(db)
        db.commit()

        assert not created and again.id == bob.id
        assert people._owner(db, WS, "email", SHARED) == alice.id
        assert "emails" not in db.get(PersonEntity, bob.id).identity_keys
        assert [n["other_person_id"] for n in person_profile(db, bob.id, WS)["identity_conflicts"]] == [alice.id]


def test_each_person_keeps_their_own_employment_history(Session):
    with Session() as db:
        alice, _ = _alice(db, company="Acme", company_domain="acme.com")
        bob, _ = _bob(db, company="Beta", company_domain="beta.com")
        db.commit()

        assert [j["company_name"] for j in person_profile(db, alice.id, WS)["employments"]] == ["Acme"]
        assert [j["company_name"] for j in person_profile(db, bob.id, WS)["employments"]] == ["Beta"]


def test_workspaces_do_not_share_mailboxes(Session):
    with Session() as db:
        _alice(db)
        other_ws, created = _bob(db, workspace_id="ws-2")
        assert created
        assert person_profile(db, other_ws.id, "ws-2")["identity_conflicts"] == []
