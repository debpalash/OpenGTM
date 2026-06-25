"""SQLite LeadDB store: composite (workspace_id, company, city) dedup + search.

Backend-agnostic behaviour the PG store mirrors (the PG side is covered by the
TEST_DATABASE_URL-gated RLS suite). Runs on the default SQLite path so it stays
in the normal suite.
"""

import os
import tempfile

import pytest

from apps.api.services.leadgen.db import LeadDB
from apps.api.services.leadgen.models import Lead


@pytest.fixture
def db():
    fd, path = tempfile.mkstemp(suffix=".db")
    os.close(fd)
    d = LeadDB(path)
    yield d
    d.close()
    os.unlink(path)


def test_composite_dedup_two_tenants_same_company_city(db):
    # Two workspaces can each own (Acme, NYC) — they are distinct rows.
    id1 = db.upsert_lead(Lead(workspace_id="w1", company="Acme", city="NYC", score=10))
    id2 = db.upsert_lead(Lead(workspace_id="w2", company="Acme", city="NYC", score=20))
    assert id1 != id2
    assert db.count_leads(workspace_id="w1") == 1
    assert db.count_leads(workspace_id="w2") == 1


def test_same_tenant_duplicate_updates_in_place(db):
    id1 = db.upsert_lead(Lead(workspace_id="w1", company="Acme", city="NYC", score=10))
    id2 = db.upsert_lead(Lead(workspace_id="w1", company="Acme", city="NYC", score=99))
    assert id1 == id2  # same (ws, company, city) → update, not a new row
    rows = db.get_leads(workspace_id="w1")
    assert len(rows) == 1 and rows[0].score == 99


def test_workspace_scoped_query(db):
    db.upsert_lead(Lead(workspace_id="w1", company="Alpha", city="SF"))
    db.upsert_lead(Lead(workspace_id="w2", company="Beta", city="LA"))
    w1 = db.get_leads(workspace_id="w1")
    assert {l.company for l in w1} == {"Alpha"}


def test_fts_search_still_works(db):
    db.upsert_lead(Lead(workspace_id="w1", company="DataDog", city="NYC",
                        specialization="Observability"))
    hits = db.get_leads(search="Observability")
    assert any(l.company == "DataDog" for l in hits)
