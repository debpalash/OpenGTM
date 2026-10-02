"""Create collisions and company merge/undo on PostgreSQL with FORCE RLS.

Set TEST_DATABASE_URL to a disposable PostgreSQL database owned by a role that
can migrate and create the restricted test login (see pg_rls_support).
"""

import os
import threading
from concurrent.futures import ThreadPoolExecutor
from uuid import uuid4

import pytest
from sqlalchemy import event

TEST_DATABASE_URL = os.getenv("TEST_DATABASE_URL")
pytestmark = [
    pytest.mark.postgres,
    pytest.mark.skipif(not TEST_DATABASE_URL, reason="TEST_DATABASE_URL not set"),
]


@pytest.fixture(scope="module")
def app_session():
    from tests.pg_rls_support import rls_app_session

    factory, dispose = rls_app_session(TEST_DATABASE_URL, pool_size=20)
    try:
        yield factory
    finally:
        dispose()


def test_concurrent_creates_have_one_winner_without_contact_replacement(app_session, monkeypatch):
    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.leadgen import store as store_module
    from apps.api.services.leadgen.models import Lead, LeadAlreadyExistsError

    monkeypatch.setattr(store_module, "SessionLocal", app_session)
    ws = f"create_{uuid4().hex}"
    store = store_module.PgLeadStore(ws)
    writers = 8
    barrier = threading.Barrier(writers)
    local = threading.local()
    engine = app_session.kw["bind"]

    # Hold every writer just before its INSERT: all have observed the absent
    # lead, so this exercises the savepoint/unique-constraint recovery path.
    def before_insert(conn, cursor, statement, parameters, context, executemany):
        if statement.lstrip().upper().startswith("INSERT INTO LEADS ") and not getattr(local, "waited", False):
            local.waited = True
            barrier.wait(timeout=20)

    event.listen(engine, "before_cursor_execute", before_insert)
    def create(i):
        with workspace_scope(ws):
            try:
                lead_id = store.upsert_lead(Lead(company="Acme", city="Austin",
                    contact_person=f"Person {i}", email=f"person{i}@acme.example"), create_only=True)
                return "created", i, lead_id
            except LeadAlreadyExistsError as exc:
                return "collision", i, exc.lead_id

    try:
        with ThreadPoolExecutor(max_workers=writers) as pool:
            outcomes = list(pool.map(create, range(writers)))
    finally:
        event.remove(engine, "before_cursor_execute", before_insert)
    winners = [result for result in outcomes if result[0] == "created"]
    assert len(winners) == 1, outcomes
    _, winner, lead_id = winners[0]
    assert sum(result[0] == "collision" for result in outcomes) == writers - 1
    assert {result[2] for result in outcomes} == {lead_id}
    with workspace_scope(ws):
        saved = store.get_lead(lead_id)
    assert (saved.contact_person, saved.email) == (f"Person {winner}", f"person{winner}@acme.example")
    # A matching company/city in a different tenant is still independent.
    other = store_module.PgLeadStore(f"other_{uuid4().hex}")
    assert other.upsert_lead(Lead(company="Acme", city="Austin"), create_only=True) != lead_id
    assert other.get_lead(lead_id) is None


@pytest.mark.parametrize("observed_side", ["kept", "merged"])
def test_merge_refreshes_committed_evidence_and_undo_restores_people(app_session, observed_side):
    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.entities.graph import merge_entities, resolve_company, split_entity
    from apps.api.services.entities.models import CompanyEntity, EntityMergeLog, PersonEmployment, PersonEntity
    from apps.api.services.entities.people import resolve_person

    ws = f"merge_{uuid4().hex}"
    with workspace_scope(ws), app_session() as db:
        kept, _ = resolve_company(db, {"company": "Keep Original", "website": "keep.example"}, "a", workspace_id=ws)
        other, _ = resolve_company(db, {"company": "Separate Original", "website": "separate.example"}, "b", workspace_id=ws)
        person, _ = resolve_person(db, workspace_id=ws, name="Synthetic Person", company="Separate Original",
            company_domain="separate.example", linkedin_url="linkedin.com/in/fixture", source="fixture")
        db.commit()
        k, m, p = kept.id, other.id, person.id

    with workspace_scope(ws), app_session() as stale:
        # Keep both objects alive in the identity map before the separate commit.
        cached = [stale.get(CompanyEntity, entity_id) for entity_id in (k, m)]
        assert [entity.observation_count for entity in cached] == [1, 1]
        with app_session() as writer:
            domain = "keep.example" if observed_side == "kept" else "separate.example"
            resolve_company(writer, {"company": "Updated observation", "website": domain,
                "phone": "5550001234"}, "c", workspace_id=ws)
            writer.commit()
        merge_entities(stale, k, m, workspace_id=ws)

    with workspace_scope(ws), app_session() as db:
        survivor = db.get(CompanyEntity, k)
        assert survivor.observation_count == 3
        assert set(survivor.sources) == {"a", "b", "c"}
        assert survivor.primary_phone == "5550001234"
        assert db.get(CompanyEntity, m) is None
        assert db.get(PersonEntity, p).company_entity_id == k
        assert db.query(PersonEmployment).filter_by(person_id=p).one().company_entity_id == k
        # Preserve an independent observation accepted after merging, too.
        resolve_company(db, {"company": "Keep Original", "website": "keep.example",
            "email": "later@keep.example"}, "later", workspace_id=ws)
        db.commit()
        log = db.query(EntityMergeLog).filter_by(workspace_id=ws, kept_id=k, merged_id=m).one()
        assert split_entity(db, log.id, workspace_id=ws)["restored_id"] == m

    with workspace_scope(ws), app_session() as db:
        kept, other = db.get(CompanyEntity, k), db.get(CompanyEntity, m)
        assert kept.observation_count == (3 if observed_side == "kept" else 2)
        assert other.observation_count == (2 if observed_side == "merged" else 1)
        assert kept.primary_email == "later@keep.example"
        assert set(kept.sources) == ({"a", "c", "later"} if observed_side == "kept" else {"a", "later"})
        assert set(other.sources) == ({"b", "c"} if observed_side == "merged" else {"b"})
        assert db.get(PersonEntity, p).company_entity_id == m
        assert db.query(PersonEmployment).filter_by(person_id=p).one().company_entity_id == m
