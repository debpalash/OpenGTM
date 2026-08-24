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


def test_workbook_query_contract_filters_and_pages(db):
    db.upsert_lead(Lead(
        workspace_id="w1", company="Alpha", city="Pune", state="MH",
        score=90, source="job:one", email="a@example.com",
        specialization="IT Staffing",
    ))
    db.upsert_lead(Lead(
        workspace_id="w1", company="Beta", city="Pune", state="MH",
        score=40, source="job:two", specialization="Accounting",
    ))

    rows, total = db.query_leads_page(
        {
            "job_ids": ["one"],
            "city": "Pune",
            "specialization": "Staffing",
            "has_email": True,
            "min_score": 80,
        },
        page=1,
        page_size=10,
    )
    assert total == 1
    assert [row["company"] for row in rows] == ["Alpha"]
    facets = db.get_filter_options()
    assert facets["cities"] == ["Pune"]
    assert facets["sources"] == ["job:one", "job:two"]
    assert facets["total_leads"] == 2


def test_update_fields_cannot_change_identity_or_inject_column_names(db):
    lead_id = db.upsert_lead(Lead(
        workspace_id="w1", company="Alpha", city="Pune", score=10,
    ))
    db.update_lead_fields(lead_id, {
        "score": 99,
        "id": 999,
        "workspace_id": "w2",
        "score = 0": "ignored",
    })
    row = db.get_lead(lead_id)
    assert row is not None
    assert row.id == lead_id and row.workspace_id == "w1" and row.score == 99
