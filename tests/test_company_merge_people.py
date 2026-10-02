"""Company corrections preserve current and historical person associations."""
from datetime import datetime, timedelta

import pytest
from sqlalchemy import create_engine, event
from sqlalchemy.orm import Session

from apps.api.database import Base
from apps.api.services.entities.graph import merge_entities, resolve_company, split_entity
from apps.api.services.entities.models import CompanyEntity, EntityMergeLog, PersonEmployment, PersonEntity
from apps.api.services.entities.people import resolve_person
from apps.api.services.poller.models import WatchSubscription  # register watch table


@pytest.fixture
def db():
    engine = create_engine("sqlite:///:memory:")
    @event.listens_for(engine, "connect")
    def foreign_keys(connection, _):
        connection.execute("PRAGMA foreign_keys=ON")
    Base.metadata.create_all(engine)
    with Session(engine, autoflush=False) as session:
        yield session
    engine.dispose()


def _companies(db):
    return [resolve_company(db, {"company": name, "website": domain}, "fixture", workspace_id="ws")[0]
            for name, domain in [("Keep Original", "keep.example"), ("Separate Original", "separate.example"),
                                 ("Third Original", "third.example")]]


def test_merge_and_undo_rebind_only_moved_people_and_employments(db):
    kept, other, third = _companies(db)
    now = datetime(2026, 1, 1)
    person, _ = resolve_person(db, workspace_id="ws", name="Synthetic Person", company="Separate Original",
                              company_domain="separate.example", linkedin_url="https://linkedin.com/in/fixture",
                              source="fixture", observed_at=now)
    historical, _ = resolve_person(db, workspace_id="ws", name="Historical Person", company="Separate Original",
                                  company_domain="separate.example", linkedin_url="https://linkedin.com/in/history",
                                  source="fixture", observed_at=now)
    resolve_person(db, workspace_id="ws", name="Historical Person", company="Third Original",
                   company_domain="third.example", linkedin_url="https://linkedin.com/in/history",
                   source="fixture", observed_at=now + timedelta(days=1))
    kept_person, _ = resolve_person(db, workspace_id="ws", name="Kept Person", company="Keep Original",
                                   company_domain="keep.example", linkedin_url="https://linkedin.com/in/kept",
                                   source="fixture", observed_at=now)
    db.commit()
    k, m, t = kept.id, other.id, third.id
    p, h, kp = person.id, historical.id, kept_person.id
    moved_jobs = [job.id for job in db.query(PersonEmployment).filter_by(company_entity_id=m)]
    assert len(moved_jobs) == 2
    assert merge_entities(db, k, m, workspace_id="ws")["kept_id"] == k
    db.expire_all()
    assert db.get(CompanyEntity, m) is None
    assert db.get(PersonEntity, p).company_entity_id == k
    assert db.get(PersonEntity, h).company_entity_id == t
    assert all(db.get(PersonEmployment, jid).company_entity_id == k for jid in moved_jobs)
    assert {db.get(PersonEmployment, jid).company_domain for jid in moved_jobs} == {"separate.example"}
    assert split_entity(db, db.query(EntityMergeLog).one().id, workspace_id="ws")["restored_id"] == m
    db.expire_all()
    assert db.get(PersonEntity, p).company_entity_id == m
    assert db.get(PersonEntity, h).company_entity_id == t
    assert db.get(PersonEntity, kp).company_entity_id == k
    assert all(db.get(PersonEmployment, jid).company_entity_id == m for jid in moved_jobs)


def test_undo_preserves_person_reassignment_after_merge(db):
    kept, other, third = _companies(db)
    person, _ = resolve_person(db, workspace_id="ws", name="Synthetic Person", company="Separate Original",
                              company_domain="separate.example", linkedin_url="https://linkedin.com/in/fixture",
                              source="fixture", observed_at=datetime(2026, 1, 1))
    db.commit()
    k, m, t, p = kept.id, other.id, third.id, person.id
    merge_entities(db, k, m, workspace_id="ws")
    person = db.get(PersonEntity, p)
    person.company_entity_id = t
    job = db.query(PersonEmployment).filter_by(person_id=p).one()
    job.company_entity_id = t
    db.commit()
    split_entity(db, db.query(EntityMergeLog).one().id, workspace_id="ws")
    db.expire_all()
    assert db.get(PersonEntity, p).company_entity_id == t
    assert db.get(PersonEmployment, job.id).company_entity_id == t
