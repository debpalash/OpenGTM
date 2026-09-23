"""Persisted person identity: LinkedIn-keyed people, legacy aliases, employment history."""

import os
import threading
from datetime import datetime

import pytest
from sqlalchemy import create_engine
from sqlalchemy.exc import OperationalError
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.database import Base
from apps.api.services.entities import people
from apps.api.services.entities.graph import resolve_company
from apps.api.services.entities.models import (
    CompanyEntity, CompanyIdentifier, EntityBlockingKey, EntityReviewPair, PersonEmployment,
    PersonEntity, PersonIdentifier,
)
from apps.api.services.entities.people import linkedin_key, person_profile, resolve_person

_TABLES = [CompanyEntity.__table__, CompanyIdentifier.__table__, EntityBlockingKey.__table__,
           EntityReviewPair.__table__, PersonEntity.__table__, PersonIdentifier.__table__,
           PersonEmployment.__table__]
T1, T2, T3 = datetime(2026, 1, 1), datetime(2026, 5, 1), datetime(2026, 9, 1)


@pytest.fixture
def Session():
    engine = create_engine("sqlite:///:memory:", connect_args={"check_same_thread": False},
                           poolclass=StaticPool)
    Base.metadata.create_all(engine, tables=_TABLES)
    return sessionmaker(bind=engine, autoflush=False, autocommit=False)


@pytest.mark.parametrize("url, key", [
    ("https://www.linkedin.com/in/Jane-Doe/", "linkedin.com/in/jane-doe"),
    ("in.linkedin.com/in/jane-doe?trk=abc", "linkedin.com/in/jane-doe"),
    ("https://linkedin.com/in/j%C3%A9r%C3%B4me", "linkedin.com/in/jérôme"),
    ("https://www.linkedin.com/company/paypal", ""),
    ("https://example.com/in/jane", ""),
    ("", ""),
])
def test_linkedin_key_normalization(url, key):
    assert linkedin_key(url) == key


def test_job_change_keeps_the_person_and_records_history(Session):
    with Session() as db:
        a, created = resolve_person(db, workspace_id="ws", name="Jane Doe", company="PayPal",
                                    company_domain="paypal.com", title="Partnerships Lead",
                                    linkedin_url="https://linkedin.com/in/jane-doe", source="chat",
                                    legacy_ids=["person_aaa"], observed_at=T1)
        b, created_b = resolve_person(db, workspace_id="ws", name="Jane Doe", company="Stripe",
                                      company_domain="stripe.com", title="Head of Partnerships",
                                      linkedin_url="https://www.linkedin.com/in/jane-doe/", source="people_search",
                                      legacy_ids=["person_bbb"], observed_at=T2)
        db.commit()
        assert created and not created_b and a.id == b.id
        profile = person_profile(db, a.id, "ws")
        jobs = profile["employments"]
        assert [(j["company_name"], j["is_current"]) for j in jobs] == [("Stripe", True), ("PayPal", False)]
        assert {i["value"] for i in profile["identifiers"] if i["kind"] == "legacy_id"} == {"person_aaa", "person_bbb"}
        assert sorted(profile["sources"]) == ["chat", "people_search"]


def test_legacy_ids_keep_resolving_and_later_gain_a_linkedin_key(Session):
    with Session() as db:
        first, _ = resolve_person(db, workspace_id="ws", name="Sam Lee", company="Acme",
                                  source="chat", legacy_ids=["person_legacy1"], observed_at=T1)
        again, created = resolve_person(db, workspace_id="ws", name="Sam Lee", company="Acme",
                                        linkedin_url="linkedin.com/in/sam-lee", source="chat",
                                        legacy_ids=["person_legacy1"], observed_at=T2)
        assert not created and again.id == first.id
        assert people._owner(db, "ws", "linkedin", "linkedin.com/in/sam-lee") == first.id


def test_name_only_people_at_different_companies_stay_distinct(Session):
    """Without a profile or email, only the company-scoped legacy id identifies someone."""
    with Session() as db:
        a, _ = resolve_person(db, workspace_id="ws", name="Alex Kim", company="Acme", source="s",
                              legacy_ids=["person:acme-alex"])
        b, created = resolve_person(db, workspace_id="ws", name="Alex Kim", company="Beta", source="s",
                                    legacy_ids=["person:beta-alex"])
        assert created and a.id != b.id


def test_title_change_at_same_company_is_history_not_a_new_job(Session):
    with Session() as db:
        p, _ = resolve_person(db, workspace_id="ws", name="Ann Wu", company="Acme", company_domain="acme.com",
                              title="Manager", linkedin_url="linkedin.com/in/ann-wu", source="s", observed_at=T1)
        resolve_person(db, workspace_id="ws", name="Ann Wu", company="Acme Inc", company_domain="www.acme.com",
                       title="Director", linkedin_url="linkedin.com/in/ann-wu", source="s", observed_at=T2)
        jobs = person_profile(db, p.id, "ws")["employments"]
        assert len(jobs) == 1 and jobs[0]["title"] == "Director"
        assert [t["title"] for t in jobs[0]["titles"]] == ["Manager", "Director"]
        assert jobs[0]["first_observed_at"].startswith("2026-01") and jobs[0]["last_observed_at"].startswith("2026-05")


def test_company_gaining_an_account_later_does_not_split_history(Session):
    with Session() as db:
        p, _ = resolve_person(db, workspace_id="ws", name="Ann Wu", company="Acme", company_domain="acme.com",
                              linkedin_url="linkedin.com/in/ann-wu", source="s", observed_at=T1)
        company, _ = resolve_company(db, {"company": "Acme", "website": "acme.com"}, "csv", workspace_id="ws")
        resolve_person(db, workspace_id="ws", name="Ann Wu", company="Acme", company_domain="acme.com",
                       linkedin_url="linkedin.com/in/ann-wu", source="s", observed_at=T2)
        jobs = person_profile(db, p.id, "ws")["employments"]
        assert len(jobs) == 1 and jobs[0]["company_entity_id"] == company.id
        assert db.get(PersonEntity, p.id).company_entity_id == company.id


def test_out_of_order_observation_does_not_become_current(Session):
    with Session() as db:
        p, _ = resolve_person(db, workspace_id="ws", name="Kai Ro", company="New Co", company_domain="newco.com",
                              linkedin_url="linkedin.com/in/kai-ro", source="s", observed_at=T3)
        resolve_person(db, workspace_id="ws", name="Kai Ro", company="Old Co", company_domain="oldco.com",
                       linkedin_url="linkedin.com/in/kai-ro", source="archive", observed_at=T1)
        jobs = person_profile(db, p.id, "ws")["employments"]
        assert [(j["company_name"], j["is_current"]) for j in jobs] == [("New Co", True), ("Old Co", False)]


def test_tenants_never_share_people(Session):
    with Session() as db:
        a, _ = resolve_person(db, workspace_id="ws-1", name="Jo Ng", company="Acme",
                              linkedin_url="linkedin.com/in/jo-ng", source="s")
        b, created = resolve_person(db, workspace_id="ws-2", name="Jo Ng", company="Acme",
                                    linkedin_url="linkedin.com/in/jo-ng", source="s")
        assert created and a.id != b.id
        assert person_profile(db, a.id, "ws-2") is None


def test_unidentifiable_person_is_rejected(Session):
    with Session() as db, pytest.raises(ValueError):
        resolve_person(db, workspace_id="ws", name="Nobody", company="Acme", source="s")


def test_concurrent_saves_of_one_profile_create_one_person(tmp_path):
    engine = create_engine(f"sqlite:///{tmp_path / 'people.db'}",
                           connect_args={"check_same_thread": False, "timeout": 30})
    Base.metadata.create_all(engine, tables=_TABLES)
    factory = sessionmaker(bind=engine, autoflush=False, autocommit=False)
    barrier, errors = threading.Barrier(8), []

    def worker(i):
        barrier.wait()
        for _ in range(20):
            try:
                with factory() as db:
                    resolve_person(db, workspace_id="ws", name="Jane Doe", company="PayPal",
                                   company_domain="paypal.com", linkedin_url="linkedin.com/in/jane-doe",
                                   source=f"src{i}", legacy_ids=[f"person_{i}"])
                    db.commit()
                return
            except OperationalError:
                continue
        errors.append(i)

    threads = [threading.Thread(target=worker, args=(i,)) for i in range(8)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    assert not errors
    with factory() as db:
        persons = db.query(PersonEntity).filter_by(workspace_id="ws").all()
        assert len(persons) == 1
        assert len((persons[0].fields or {})["sources"]) == 8
        assert db.query(PersonEmployment).count() == 1
        assert db.query(PersonIdentifier).filter_by(kind="legacy_id").count() == 8


def test_migration_round_trip(tmp_path, monkeypatch):
    from alembic import command
    from alembic.config import Config

    monkeypatch.setenv("DATABASE_URL", f"sqlite:///{tmp_path / 'mig.db'}")
    cfg = Config(os.path.join(os.path.dirname(__file__), "..", "alembic.ini"))
    command.upgrade(cfg, "3f4051627384")
    command.downgrade(cfg, "2e3f40516273")
    command.upgrade(cfg, "head")
