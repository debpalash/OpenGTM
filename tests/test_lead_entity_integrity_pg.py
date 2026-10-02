"""Lead merge and company merge on real PostgreSQL under FORCE RLS (PG-gated).

SQLite cannot show what only PostgreSQL enforces: the foreign key that blocked
merging a company with linked people, and the row lock that stops concurrent
requests from each filling the same empty contact slot.

Run:
    TEST_DATABASE_URL='postgresql+psycopg://postgres@127.0.0.1:5432/<throwaway>' \\
    uv run pytest tests/test_lead_entity_integrity_pg.py -q
"""

import os
import threading
import uuid

import pytest

TEST_DATABASE_URL = os.getenv("TEST_DATABASE_URL")

pytestmark = pytest.mark.skipif(
    not TEST_DATABASE_URL,
    reason="TEST_DATABASE_URL not set (Postgres integrity tests skipped)",
)


@pytest.fixture(scope="module")
def app_session():
    from tests.pg_rls_support import rls_app_session
    factory, dispose = rls_app_session(TEST_DATABASE_URL, pool_size=30)
    yield factory
    dispose()


@pytest.fixture
def lead_store(app_session, monkeypatch):
    import apps.api.services.leadgen.store as store_mod
    monkeypatch.setattr(store_mod, "SessionLocal", app_session)
    ws = f"ws_integrity_{uuid.uuid4().hex[:8]}"
    return ws, store_mod.PgLeadStore(ws)


def test_merging_a_company_with_people_works_on_postgres_and_undoes(app_session):
    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.entities.graph import merge_entities, resolve_company, split_entity
    from apps.api.services.entities.models import (
        CompanyEntity, EntityMergeLog, PersonEmployment, PersonEntity,
    )
    from apps.api.services.entities.people import resolve_person

    ws = f"ws_integrity_{uuid.uuid4().hex[:8]}"
    with workspace_scope(ws), app_session() as db:
        kept, _ = resolve_company(db, {"company": "Keep Original", "website": "keep-pg.example"},
                                  "source-a", workspace_id=ws)
        other, _ = resolve_company(db, {"company": "Separate Original", "website": "separate-pg.example"},
                                   "source-b", workspace_id=ws)
        person, _ = resolve_person(
            db, workspace_id=ws, name="Synthetic Person", company="Separate Original",
            company_domain="separate-pg.example", linkedin_url="https://linkedin.com/in/synthetic-pg-person",
            source="fixture")
        db.commit()
        kept_id, other_id, person_id = kept.id, other.id, person.id
        assert person.company_entity_id == other_id
        merge_entities(db, kept_id, other_id, workspace_id=ws)

    with workspace_scope(ws), app_session() as db:
        assert db.get(PersonEntity, person_id).company_entity_id == kept_id
        assert [j.company_entity_id for j in db.query(PersonEmployment).filter_by(person_id=person_id)] == [kept_id]
        assert db.get(CompanyEntity, other_id) is None
        log = db.query(EntityMergeLog).filter_by(workspace_id=ws, kept_id=kept_id, merged_id=other_id).one()
        result = split_entity(db, log.id, workspace_id=ws)
        assert result["rebound_people"] == 1

    with workspace_scope(ws), app_session() as db:
        assert db.get(PersonEntity, person_id).company_entity_id == other_id
        assert [j.company_entity_id for j in db.query(PersonEmployment).filter_by(person_id=person_id)] == [other_id]
        kept = db.get(CompanyEntity, kept_id)
        assert kept.sources == ["source-a"]
        assert (kept.observation_count, kept.corroboration_count) == (1, 1)
        assert [o["value"] for o in kept.fields["website"]] == ["keep-pg.example"]


def test_rediscovery_keeps_pipeline_state_on_postgres(lead_store):
    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.leadgen.models import Lead

    ws, store = lead_store
    with workspace_scope(ws):
        lead_id = store.upsert_lead(Lead(company="Acme", city="Austin", website="https://acme.example",
                                         email="alice@acme.example", contact_person="Alice Smith",
                                         status="contacted", score=82, score_tier="hot", notes="called twice"))
        assert store.upsert_lead(Lead(company="Acme", city="Austin", source="rerun")) == lead_id
        row = store.get_lead(lead_id)
    assert (row.website, row.email, row.status, row.score, row.notes) == (
        "https://acme.example", "alice@acme.example", "contacted", 82, "called twice")
    assert row.source == "rerun"


def test_concurrent_contacts_on_one_lead_never_overwrite_each_other(lead_store):
    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.leadgen.lead_merge import LeadContactConflict
    from apps.api.services.leadgen.models import Lead

    ws, store = lead_store
    with workspace_scope(ws):
        lead_id = store.upsert_lead(Lead(company="Acme", city="Austin", website="https://acme.example",
                                         status="contacted"))

    writers = 8
    barrier, outcomes = threading.Barrier(writers), []

    def worker(i):
        barrier.wait()
        try:
            with workspace_scope(ws):
                store.upsert_lead(
                    Lead(company="Acme", city="Austin", contact_person=f"Person {i}", email=f"p{i}@acme.example"),
                    on_contact_conflict="reject")
            outcomes.append(("ok", i))
        except LeadContactConflict:
            outcomes.append(("conflict", i))
        except Exception as exc:  # pragma: no cover - reported by the assertion below
            outcomes.append(("error", repr(exc)))

    threads = [threading.Thread(target=worker, args=(i,)) for i in range(writers)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()

    kinds = [kind for kind, _ in outcomes]
    # Every writer saw an empty contact slot at the start. The row lock lets one
    # fill it and makes the rest see the winner, so none silently replaces it.
    assert kinds.count("ok") == 1 and kinds.count("conflict") == writers - 1, outcomes
    winner = next(i for kind, i in outcomes if kind == "ok")
    with workspace_scope(ws):
        row = store.get_lead(lead_id)
    assert (row.contact_person, row.email) == (f"Person {winner}", f"p{winner}@acme.example")
    assert (row.website, row.status) == ("https://acme.example", "contacted")
