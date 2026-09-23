"""Canonical company identity on real PostgreSQL under FORCE RLS (PG-gated).

Proves on PostgreSQL what the SQLite suite cannot: concurrent imports of one
domain converge on a single entity without losing any observation, and
company_identifiers is tenant-isolated for the non-superuser app role.

Run:
    TEST_DATABASE_URL='postgresql+psycopg://postgres@127.0.0.1:5432/<throwaway>' \\
    uv run pytest tests/test_entity_identity_pg.py -q
"""

import os
import threading
import uuid

import pytest
from sqlalchemy.exc import DBAPIError

TEST_DATABASE_URL = os.getenv("TEST_DATABASE_URL")

pytestmark = pytest.mark.skipif(
    not TEST_DATABASE_URL,
    reason="TEST_DATABASE_URL not set (Postgres entity identity tests skipped)",
)


@pytest.fixture(scope="module")
def app_session():
    from tests.pg_rls_support import rls_app_session
    factory, dispose = rls_app_session(TEST_DATABASE_URL)
    yield factory
    dispose()


def _ws():
    return f"ws_ident_{uuid.uuid4().hex[:8]}"


def test_concurrent_imports_converge_without_losing_observations(app_session):
    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.entities.graph import resolve_company
    from apps.api.services.entities.models import CompanyEntity, CompanyIdentifier

    ws = _ws()
    writers = 12
    barrier = threading.Barrier(writers)
    errors = []

    def worker(i):
        try:
            with workspace_scope(ws):
                barrier.wait()
                with app_session() as db:
                    resolve_company(db, {"company": f"Acme {i}", "website": "https://acme-pg.com"},
                                    f"src{i}", workspace_id=ws)
                    db.commit()
        except Exception as exc:  # surfaced below with the failing writer
            errors.append((i, repr(exc)))

    threads = [threading.Thread(target=worker, args=(i,)) for i in range(writers)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    assert not errors, errors

    with workspace_scope(ws), app_session() as db:
        entities = db.query(CompanyEntity).filter_by(workspace_id=ws).all()
        assert len(entities) == 1
        assert sorted(entities[0].sources) == sorted(f"src{i}" for i in range(writers))
        assert entities[0].observation_count == writers
        owners = db.query(CompanyIdentifier).filter_by(workspace_id=ws).all()
        assert [(o.kind, o.value, o.entity_id) for o in owners] == [
            ("domain", "acme-pg.com", entities[0].id)]


def test_identifiers_are_tenant_isolated_under_force_rls(app_session):
    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.entities.graph import resolve_company
    from apps.api.services.entities.models import CompanyIdentifier

    w1, w2 = _ws(), _ws()
    with workspace_scope(w1), app_session() as db:
        e1, _ = resolve_company(db, {"company": "Beta", "website": "beta-pg.com"}, "csv", workspace_id=w1)
        db.commit()
        e1_id = e1.id
    with workspace_scope(w2), app_session() as db:
        assert db.query(CompanyIdentifier).filter_by(value="beta-pg.com").count() == 0
        # The same domain in another tenant is an independent entity.
        e2, created = resolve_company(db, {"company": "Beta", "website": "beta-pg.com"}, "csv",
                                      workspace_id=w2)
        db.commit()
        assert created and e2.id != e1_id
        # WITH CHECK: a W2 session cannot write an identifier into W1.
        db.add(CompanyIdentifier(workspace_id=w1, kind="domain", value="spoof.example", entity_id=e1_id))
        with pytest.raises(DBAPIError):
            db.flush()
        db.rollback()


def test_merge_repoints_row_and_watch_references_under_rls(app_session):
    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.entities.graph import merge_entities, resolve_company
    from apps.api.services.poller.models import WatchSubscription
    from apps.api.services.signals.tracking import _scope_key, workbook_account_ids
    from apps.api.services.workbook.models import Workbook, WorkbookRow

    ws = _ws()
    wb = f"wb_{uuid.uuid4().hex[:8]}"
    with workspace_scope(ws), app_session() as db:
        kept, _ = resolve_company(db, {"company": "Gamma", "website": "gamma-pg.com"}, "csv", workspace_id=ws)
        dup, _ = resolve_company(db, {"company": "Gamma Inc", "website": "gamma-inc-pg.io"}, "crm", workspace_id=ws)
        k, m = kept.id, dup.id
        db.add(Workbook(id=wb, workspace_id=ws, name="Accounts"))
        db.flush()
        db.add(WorkbookRow(workbook_id=wb, workspace_id=ws, position=0, canonical_entity_id=m,
                           data={"account_id": m, "company": "Gamma Inc"}))
        watch_id = str(uuid.uuid4())
        db.add(WatchSubscription(id=watch_id, workspace_id=ws, kind="account_group", target="Account set",
                                 signal_types=["jobs"],
                                 config={"accounts": [{"account_id": m}], "scope_key": _scope_key([m])},
                                 cursor={"account_group": {m: {"seen": ["x"]}}, "collector_health": {}}))
        db.commit()
        merge_entities(db, k, m, workspace_id=ws)
    with workspace_scope(ws), app_session() as db:
        assert workbook_account_ids(db, ws, wb) == [k]
        watch = db.get(WatchSubscription, watch_id)
        assert watch.config["accounts"] == [{"account_id": k}]
        assert watch.cursor["account_group"] == {k: {"seen": ["x"]}}


def test_concurrent_person_saves_converge_under_rls(app_session):
    from apps.api.core.tenancy import workspace_scope
    from apps.api.services.entities.models import PersonEmployment, PersonEntity, PersonIdentifier
    from apps.api.services.entities.people import resolve_person

    ws, writers = _ws(), 10
    barrier, errors = threading.Barrier(writers), []

    def worker(i):
        try:
            with workspace_scope(ws):
                barrier.wait()
                with app_session() as db:
                    resolve_person(db, workspace_id=ws, name="Jane Doe", company="PayPal",
                                   company_domain="paypal.com", linkedin_url="linkedin.com/in/jane-doe-pg",
                                   title="Partnerships", source=f"src{i}", legacy_ids=[f"person_pg_{i}"])
                    db.commit()
        except Exception as exc:
            errors.append((i, repr(exc)))

    threads = [threading.Thread(target=worker, args=(i,)) for i in range(writers)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    assert not errors, errors
    with workspace_scope(ws), app_session() as db:
        persons = db.query(PersonEntity).filter_by(workspace_id=ws).all()
        assert len(persons) == 1
        assert len(persons[0].fields["sources"]) == writers
        assert db.query(PersonEmployment).filter_by(workspace_id=ws).count() == 1
        assert db.query(PersonIdentifier).filter_by(workspace_id=ws, kind="legacy_id").count() == writers
    with workspace_scope(_ws()), app_session() as db:
        assert db.query(PersonIdentifier).filter_by(value="linkedin.com/in/jane-doe-pg").count() == 0
