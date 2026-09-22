"""Deterministic company identity: exact domains, shared hosts, concurrency, merge/split."""

import os
import sqlite3
import threading

import pytest
from sqlalchemy import create_engine
from sqlalchemy.exc import OperationalError
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.database import Base
from apps.api.services.entities import graph
from apps.api.services.entities.graph import identity_domain, merge_entities, resolve_company, split_entity
from apps.api.services.entities.models import (
    CompanyEntity, CompanyIdentifier, EntityBlockingKey, EntityMergeLog, EntityReviewPair,
)
from apps.api.services.workbook.models import Workbook, WorkbookRow

_TABLES = [CompanyEntity.__table__, CompanyIdentifier.__table__, EntityBlockingKey.__table__,
           EntityMergeLog.__table__, EntityReviewPair.__table__, Workbook.__table__,
           WorkbookRow.__table__]


@pytest.fixture
def Session():
    engine = create_engine("sqlite:///:memory:", connect_args={"check_same_thread": False},
                           poolclass=StaticPool)
    Base.metadata.create_all(engine, tables=_TABLES)
    return sessionmaker(bind=engine, autoflush=False, autocommit=False)


def _identifiers(db, ws="ws-1"):
    return {(i.kind, i.value): i.entity_id for i in db.query(CompanyIdentifier).filter_by(workspace_id=ws)}


@pytest.mark.parametrize("url, expected", [
    ("https://www.Acme.com/about", "acme.com"),
    ("acme.co.uk", "acme.co.uk"),
    ("https://facebook.com/acme-bakery", ""),
    ("https://m.facebook.com/acme", ""),
    ("linktr.ee/acme", ""),
    ("https://www.google.com/maps/place/acme", ""),
    ("", ""),
    ("localhost", ""),
])
def test_identity_domain_excludes_shared_hosts(url, expected):
    assert identity_domain(url) == expected


def test_unrelated_companies_on_shared_hosts_never_match(Session):
    with Session() as db:
        a, _ = resolve_company(db, {"company": "Rosa Bakery", "website": "facebook.com/rosabakery",
                                    "city": "Austin"}, "maps", workspace_id="ws-1")
        b, created = resolve_company(db, {"company": "Rosa Bistro", "website": "facebook.com/rosabistro",
                                          "city": "Austin"}, "maps", workspace_id="ws-1")
        assert created and a.id != b.id
        assert a.primary_domain == "" and _identifiers(db) == {}


def test_same_domain_resolves_exactly_despite_name_variants(Session):
    with Session() as db:
        a, created_a = resolve_company(db, {"company": "Acme Corporation", "website": "https://acme.com"},
                                       "csv", workspace_id="ws-1")
        b, created_b = resolve_company(db, {"company": "ACME Inc (EMEA)", "website": "www.acme.com/de"},
                                       "crm", workspace_id="ws-1")
        assert created_a and not created_b and a.id == b.id
        assert sorted(b.sources) == ["crm", "csv"]
        assert _identifiers(db) == {("domain", "acme.com"): a.id}
        # Tenant isolation: the same domain in another workspace is another entity.
        c, created_c = resolve_company(db, {"company": "Acme", "website": "acme.com"}, "csv",
                                       workspace_id="ws-2")
        assert created_c and c.id != a.id


def test_lost_race_records_observation_on_winner(Session, monkeypatch):
    """A concurrent import that read before the winner committed must converge."""
    with Session() as db:
        winner, _ = resolve_company(db, {"company": "Acme", "website": "acme.com"}, "csv",
                                    workspace_id="ws-1")
        db.commit()
        winner_id = winner.id
    real_owner = graph._identifier_owner
    calls = {"n": 0}

    def stale_first_read(db, ws, kind, value):
        calls["n"] += 1
        return None if calls["n"] == 1 else real_owner(db, ws, kind, value)

    monkeypatch.setattr(graph, "_identifier_owner", stale_first_read)
    monkeypatch.setattr(graph, "_candidate_ids", lambda *a, **k: set())  # fuzzy also missed
    with Session() as db:
        entity, created = resolve_company(db, {"company": "Acme Ltd", "website": "https://acme.com"},
                                          "crm", workspace_id="ws-1")
        db.commit()
        assert not created and entity.id == winner_id
        assert db.query(CompanyEntity).filter_by(workspace_id="ws-1").count() == 1
        assert sorted(db.get(CompanyEntity, winner_id).sources) == ["crm", "csv"]


def test_concurrent_imports_of_one_domain_create_one_entity(tmp_path):
    engine = create_engine(f"sqlite:///{tmp_path / 'race.db'}",
                           connect_args={"check_same_thread": False, "timeout": 30})
    Base.metadata.create_all(engine, tables=_TABLES)
    factory = sessionmaker(bind=engine, autoflush=False, autocommit=False)
    barrier = threading.Barrier(8)
    errors = []

    def worker(i):
        barrier.wait()
        for _ in range(20):  # SQLite reports snapshot conflicts as BUSY; retry like a job would
            try:
                with factory() as db:
                    resolve_company(db, {"company": f"Acme {i}", "website": "acme.com"}, f"src{i}",
                                    workspace_id="ws-1")
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
        entities = db.query(CompanyEntity).filter_by(workspace_id="ws-1").all()
        assert len(entities) == 1
        assert len(set(entities[0].sources)) == 8


def test_merge_moves_identifiers_and_split_restores_them(Session):
    with Session() as db:
        kept, _ = resolve_company(db, {"company": "Acme", "website": "acme.com"}, "csv", workspace_id="ws-1")
        merged, _ = resolve_company(db, {"company": "Zeta Holdings", "website": "zeta-holdings.io"}, "crm",
                                    workspace_id="ws-1")
        db.commit()
        kept_id, merged_id = kept.id, merged.id
        assert merge_entities(db, kept_id, merged_id, workspace_id="ws-1")["kept_id"] == kept_id
        assert _identifiers(db) == {("domain", "acme.com"): kept_id, ("domain", "zeta-holdings.io"): kept_id}
        # A later import of the merged domain resolves to the kept entity.
        again, created = resolve_company(db, {"company": "Zeta", "website": "zeta-holdings.io"}, "web",
                                         workspace_id="ws-1")
        assert not created and again.id == kept_id
        db.commit()
        log = db.query(EntityMergeLog).one()
        assert split_entity(db, log.id, workspace_id="ws-1")["restored_id"] == merged_id
        assert _identifiers(db)[("domain", "zeta-holdings.io")] == merged_id
        assert _identifiers(db)[("domain", "acme.com")] == kept_id


def test_migration_backfills_oldest_owner_and_skips_shared_hosts(tmp_path, monkeypatch):
    from alembic import command
    from alembic.config import Config

    db_path = str(tmp_path / "mig.db")
    monkeypatch.setenv("DATABASE_URL", f"sqlite:///{db_path}")
    cfg = Config(os.path.join(os.path.dirname(__file__), "..", "alembic.ini"))
    command.upgrade(cfg, "1d2e3f405162")
    con = sqlite3.connect(db_path)
    rows = [
        ("e1", "ws-1", "acme.com", '{"domains": ["acme.com", "acme.io"]}', "2026-01-01"),
        ("e2", "ws-1", "acme.com", '{"domains": ["acme.com"]}', "2026-02-01"),  # legacy duplicate
        ("e3", "ws-1", "facebook.com", '{"domains": ["facebook.com"]}', "2026-01-01"),
        ("e4", "ws-2", "acme.com", '{}', "2026-03-01"),
    ]
    for eid, ws, domain, keys, seen in rows:
        con.execute("INSERT INTO company_entities (id, workspace_id, canonical_name, primary_domain, "
                    "identity_keys, first_seen) VALUES (?, ?, 'x', ?, ?, ?)", (eid, ws, domain, keys, seen))
    con.commit()
    command.upgrade(cfg, "2e3f40516273")
    got = set(con.execute("SELECT workspace_id, value, entity_id FROM company_identifiers"))
    assert got == {("ws-1", "acme.com", "e1"), ("ws-1", "acme.io", "e1"), ("ws-2", "acme.com", "e4")}
    assert con.execute("SELECT count(*) FROM company_entities").fetchone()[0] == 4  # nothing merged
    con.close()
