"""Merging and undoing a company merge keeps people, employment and evidence intact.

Regression coverage for two reports. Merging a company that had linked people
failed on a foreign key instead of moving them, and undoing a merge restored the
merged company but left its evidence on the kept one, so counts and sources stayed
combined and every observation existed on both records.
"""

from datetime import datetime

import pytest
from sqlalchemy import create_engine, event
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.database import Base
from apps.api.services.entities.graph import merge_entities, resolve_company, split_entity
from apps.api.services.entities.models import (
    CompanyEntity, EntityBlockingKey, EntityMergeLog, EntityReviewPair, PersonEmployment, PersonEntity,
)
from apps.api.services.entities.people import resolve_person
from apps.api.services.poller.models import WatchSubscription
from apps.api.services.workbook.models import Workbook, WorkbookRow
from tests.entity_tables import PERSON_TABLES

WS = "ws-1"
T1, T2, T3 = datetime(2026, 1, 1), datetime(2026, 5, 1), datetime(2026, 9, 1)
_TABLES = [*PERSON_TABLES, EntityBlockingKey.__table__, EntityMergeLog.__table__,
           EntityReviewPair.__table__, Workbook.__table__, WorkbookRow.__table__,
           WatchSubscription.__table__]


@pytest.fixture
def Session():
    engine = create_engine("sqlite:///:memory:", connect_args={"check_same_thread": False},
                           poolclass=StaticPool)

    @event.listens_for(engine, "connect")
    def _enforce_foreign_keys(connection, _):  # as PostgreSQL does
        connection.execute("PRAGMA foreign_keys=ON")

    Base.metadata.create_all(engine, tables=_TABLES)
    return sessionmaker(bind=engine, autoflush=False, autocommit=False)


def _company(db, name, site, source, ws=WS, **extra):
    return resolve_company(db, {"company": name, "website": site, **extra}, source, workspace_id=ws)[0]


def _person(db, slug, company, domain, ws=WS, observed=T1):
    person, _ = resolve_person(
        db, workspace_id=ws, name=f"Person {slug}", company=company, company_domain=domain,
        linkedin_url=f"https://linkedin.com/in/{slug}", source="fixture", observed_at=observed)
    return person


def _merge(db, kept, other):
    merge_entities(db, kept.id, other.id, workspace_id=WS)
    return db.query(EntityMergeLog).filter_by(kept_id=kept.id, merged_id=other.id).one()


def _state(entity):
    """What a company holds as evidence and as winners. Agreement scores are left
    out: a new company has none until its next observation, a recomputed one does."""
    return {
        "name": entity.canonical_name, "domain": entity.primary_domain, "phone": entity.primary_phone,
        "sources": sorted(entity.sources), "observations": entity.observation_count,
        "corroboration": entity.corroboration_count, "fields": entity.fields,
    }


def _employment(db, person):
    return db.query(PersonEmployment).filter_by(person_id=person.id).all()


# ── people follow a merged company ───────────────────────────────────────────

def test_merging_a_company_with_linked_people_moves_them_to_the_kept_company(Session):
    with Session() as db:
        kept = _company(db, "Keep Original", "keep.example", "fixture")
        other = _company(db, "Separate Original", "separate.example", "fixture")
        person = _person(db, "synthetic", "Separate Original", "separate.example")
        db.commit()
        assert person.company_entity_id == other.id

        merge_entities(db, kept.id, other.id, workspace_id=WS)
        db.expire_all()

        assert db.get(PersonEntity, person.id).company_entity_id == kept.id
        assert [j.company_entity_id for j in _employment(db, person)] == [kept.id]
        assert db.get(CompanyEntity, other.id) is None


def test_historical_employment_follows_the_merge_and_the_current_job_is_untouched(Session):
    with Session() as db:
        kept = _company(db, "Keep Original", "keep.example", "fixture")
        other = _company(db, "Separate Original", "separate.example", "fixture")
        third = _company(db, "Third Co", "third.example", "fixture")
        _person(db, "mover", "Separate Original", "separate.example", observed=T1)
        mover = _person(db, "mover", "Third Co", "third.example", observed=T2)
        db.commit()
        assert mover.company_entity_id == third.id

        merge_entities(db, kept.id, other.id, workspace_id=WS)
        db.expire_all()

        by_company = {j.company_name: j for j in _employment(db, mover)}
        assert by_company["Separate Original"].company_entity_id == kept.id and not by_company["Separate Original"].is_current
        assert by_company["Third Co"].company_entity_id == third.id and by_company["Third Co"].is_current
        assert db.get(PersonEntity, mover.id).company_entity_id == third.id


def test_undo_moves_back_only_what_the_merge_moved(Session):
    with Session() as db:
        kept = _company(db, "Keep Original", "keep.example", "fixture")
        other = _company(db, "Separate Original", "separate.example", "fixture")
        third = _company(db, "Third Co", "third.example", "fixture")
        stays = _person(db, "stays", "Separate Original", "separate.example")
        leaves = _person(db, "leaves", "Separate Original", "separate.example", observed=T1)
        db.commit()
        log = _merge(db, kept, other)

        # After the merge: one person changes company, another joins the kept company.
        _person(db, "leaves", "Third Co", "third.example", observed=T3)
        newcomer = _person(db, "newcomer", "Keep Original", "keep.example", observed=T3)
        db.commit()

        result = split_entity(db, log.id, workspace_id=WS)
        db.expire_all()

        assert result["rebound_people"] == 1
        assert db.get(PersonEntity, stays.id).company_entity_id == other.id
        assert [j.company_entity_id for j in _employment(db, stays)] == [other.id]
        # Left the company since: their current job stays, their history goes back.
        assert db.get(PersonEntity, leaves.id).company_entity_id == third.id
        history = {j.company_name: j.company_entity_id for j in _employment(db, leaves)}
        assert history == {"Separate Original": other.id, "Third Co": third.id}
        # Joined the kept company after the merge: stays with it.
        assert db.get(PersonEntity, newcomer.id).company_entity_id == kept.id
        assert [j.company_entity_id for j in _employment(db, newcomer)] == [kept.id]


def test_merge_and_undo_leave_other_workspaces_alone(Session):
    with Session() as db:
        kept = _company(db, "Keep Original", "keep.example", "fixture")
        other = _company(db, "Separate Original", "separate.example", "fixture")
        mine = _person(db, "mine", "Separate Original", "separate.example")
        foreign_company = _company(db, "Separate Original", "separate.example", "fixture", ws="ws-2")
        theirs = _person(db, "theirs", "Separate Original", "separate.example", ws="ws-2")
        db.commit()
        log = _merge(db, kept, other)
        split_entity(db, log.id, workspace_id=WS)
        db.expire_all()

        assert db.get(PersonEntity, mine.id).company_entity_id == other.id
        assert db.get(PersonEntity, theirs.id).company_entity_id == foreign_company.id
        assert [j.company_entity_id for j in _employment(db, theirs)] == [foreign_company.id]
        assert db.query(CompanyEntity).filter_by(workspace_id="ws-2").count() == 1


# ── undo takes back exactly what the merge added ─────────────────────────────

def test_undoing_a_merge_leaves_both_companies_as_they_were(Session):
    with Session() as db:
        kept = _company(db, "Keep Original", "keep.example", "source-a")
        other = _company(db, "Separate Original", "separate.example", "source-b", phone="+1 555 0100")
        db.commit()
        kept_id, other_id = kept.id, other.id
        kept_before, other_before = _state(kept), _state(other)

        log = _merge(db, kept, other)
        assert sorted(db.get(CompanyEntity, kept_id).sources) == ["source-a", "source-b"]   # the merge combined them
        split_entity(db, log.id, workspace_id=WS)
        db.expire_all()

        assert _state(db.get(CompanyEntity, kept_id)) == kept_before
        assert _state(db.get(CompanyEntity, other_id)) == other_before
        assert [o["value"] for o in db.get(CompanyEntity, kept_id).fields["website"]] == ["keep.example"]


def test_undo_keeps_evidence_that_arrived_after_the_merge(Session):
    with Session() as db:
        kept = _company(db, "Keep Original", "keep.example", "source-a")
        other = _company(db, "Separate Original", "separate.example", "source-b")
        db.commit()
        log = _merge(db, kept, other)

        resolve_company(db, {"company": "Keep Original", "website": "keep.example"}, "source-c", workspace_id=WS)
        db.commit()
        split_entity(db, log.id, workspace_id=WS)
        db.expire_all()

        after = db.get(CompanyEntity, kept.id)
        assert sorted(after.sources) == ["source-a", "source-c"]
        assert (after.observation_count, after.corroboration_count) == (2, 2)
        assert [o["value"] for o in after.fields["website"]] == ["keep.example", "keep.example"]
        assert not any("separate" in o["value"] for obs in after.fields.values() for o in obs)


def test_a_source_the_merge_added_stays_if_the_kept_company_has_since_heard_from_it(Session):
    with Session() as db:
        kept = _company(db, "Keep Original", "keep.example", "source-a")
        other = _company(db, "Separate Original", "separate.example", "source-b")
        db.commit()
        log = _merge(db, kept, other)
        resolve_company(db, {"company": "Keep Original", "website": "keep.example"}, "source-b", workspace_id=WS)
        db.commit()
        split_entity(db, log.id, workspace_id=WS)
        db.expire_all()

        after = db.get(CompanyEntity, kept.id)
        assert sorted(after.sources) == ["source-a", "source-b"] and after.corroboration_count == 2


def test_a_source_both_companies_shared_is_not_taken_from_the_kept_one(Session):
    with Session() as db:
        kept = _company(db, "Keep Original", "keep.example", "csv")
        other = _company(db, "Separate Original", "separate.example", "csv")
        db.commit()
        log = _merge(db, kept, other)
        assert (db.get(CompanyEntity, kept.id).sources, db.get(CompanyEntity, kept.id).observation_count) == (["csv"], 2)
        split_entity(db, log.id, workspace_id=WS)
        db.expire_all()

        assert db.get(CompanyEntity, kept.id).sources == ["csv"]
        assert db.get(CompanyEntity, kept.id).observation_count == 1
        assert db.get(CompanyEntity, other.id).sources == ["csv"]


def test_undo_clears_values_that_only_the_merged_company_supplied(Session):
    with Session() as db:
        kept = _company(db, "Keep Original", "keep.example", "source-a")
        other = _company(db, "Separate Original", "separate.example", "source-b", phone="+1 555 0100")
        db.commit()
        log = _merge(db, kept, other)
        assert db.get(CompanyEntity, kept.id).primary_phone == "+1 555 0100"

        split_entity(db, log.id, workspace_id=WS)
        db.expire_all()

        after = db.get(CompanyEntity, kept.id)
        assert after.primary_phone == "" and "phone" not in after.fields
        assert after.canonical_name == "Keep Original" and after.primary_domain == "keep.example"


def test_a_merge_recorded_before_contributions_were_kept_still_undoes_cleanly(Session):
    with Session() as db:
        kept = _company(db, "Keep Original", "keep.example", "source-a")
        other = _company(db, "Separate Original", "separate.example", "source-b")
        db.commit()
        kept_before = _state(kept)
        log = _merge(db, kept, other)
        log.snapshot = {k: v for k, v in log.snapshot.items() if k not in ("evidence", "people", "employments")}
        db.commit()

        split_entity(db, log.id, workspace_id=WS)
        db.expire_all()
        assert _state(db.get(CompanyEntity, kept.id)) == kept_before


def test_a_company_with_no_people_still_merges_and_splits(Session):
    with Session() as db:
        kept = _company(db, "Keep Original", "keep.example", "fixture")
        other = _company(db, "Separate Original", "separate.example", "fixture")
        db.commit()
        log = _merge(db, kept, other)
        result = split_entity(db, log.id, workspace_id=WS)
        assert result["restored_id"] == other.id and result["rebound_people"] == 0
        assert db.get(CompanyEntity, other.id) is not None
