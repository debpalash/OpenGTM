"""people_search workbook source — behavioural tests (SQLite, search layer mocked).

Covers:
  * config validation (companies|from_column required; caps clamped);
  * person rows materialized with the right field vocabulary (lead fields the
    email waterfall keys on + explicit person fields), workspace-stamped;
  * per-company cap (max_per_company, hard cap 25);
  * total-search cap (max_searches, hard cap 100) — DDG never over-queried;
  * dedup on re-run via the canonical_entity_id identity mechanism
    (linkedin_url, fallback name+company);
  * workspace isolation (each workbook's rows stamped with its own tenant);
  * PEOPLE_SEARCH_SOURCE_ENABLED=False → graceful no-op, ZERO searches.

The DDG layer is mocked at ``crosslinked._ddg_linkedin_search`` (the single
seam both find_people and find_people_by_titles go through).
"""

import asyncio

import pytest
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.database import Base
from apps.api.core.config import settings
from apps.api.services.workbook.models import Workbook, WorkbookRow

import apps.api.services.workbook.source_engine as se
import apps.api.services.workbook.people_search as ps
import apps.api.services.leadgen.enrichment.providers.crosslinked as cl

W1 = "ws_one"
W2 = "ws_two"


# ── fixtures ──────────────────────────────────────────────────────────────

@pytest.fixture()
def session_factory():
    engine = create_engine(
        "sqlite:///:memory:",
        connect_args={"check_same_thread": False},
        poolclass=StaticPool,
    )
    Base.metadata.create_all(engine, tables=[Workbook.__table__, WorkbookRow.__table__])
    return sessionmaker(bind=engine, autoflush=False, autocommit=False)


@pytest.fixture()
def env(session_factory, monkeypatch):
    """Bind SessionLocal everywhere the source path touches it, enable the
    flag, silence redis broadcasts, and zero the politeness delay."""
    monkeypatch.setattr(se, "SessionLocal", session_factory, raising=True)
    monkeypatch.setattr(ps, "SessionLocal", session_factory, raising=True)
    monkeypatch.setattr(ps, "_make_redis", lambda: None, raising=True)
    monkeypatch.setattr(settings, "PEOPLE_SEARCH_SOURCE_ENABLED", True, raising=False)
    return session_factory


def _mk_workbook(factory, workspace_id, col, extra_cols=None, name="People"):
    with factory() as db:
        wb = Workbook(
            name=name,
            workspace_id=workspace_id,
            columns_config=(extra_cols or []) + [col],
        )
        db.add(wb)
        db.commit()
        return wb.id


def _ps_col(**overrides):
    col = {"id": "src_ps1", "name": "Find people", "type": "source", "kind": "people_search"}
    col.update(overrides)
    return col


def _ddg_result(name, title, company, slug):
    return {
        "title": f"{name} - {title} at {company} | LinkedIn",
        "body": f"{name}. {title} at {company}.",
        "href": f"https://www.linkedin.com/in/{slug}",
    }


class SearchSpy:
    """Fake for crosslinked._ddg_linkedin_search — records queries."""

    def __init__(self, results_by_company=None, default=None):
        self.results_by_company = results_by_company or {}
        self.default = default if default is not None else []
        self.queries = []

    async def __call__(self, query, max_results=10):
        self.queries.append(query)
        for company, results in self.results_by_company.items():
            if f'"{company}"' in query:
                return results
        return self.default


def _run(workbook_id, column_id, workspace_id):
    return asyncio.run(se.materialize_source(workbook_id, column_id, workspace_id))


def _rows(factory, workbook_id):
    with factory() as db:
        return (
            db.query(WorkbookRow)
            .filter(WorkbookRow.workbook_id == workbook_id)
            .order_by(WorkbookRow.position.asc())
            .all()
        )


# ── config validation ─────────────────────────────────────────────────────

def test_config_requires_companies_or_from_column():
    with pytest.raises(ValueError):
        ps.validate_people_search_config({"kind": "people_search", "titles": ["CTO"]})
    # either one is enough
    assert ps.validate_people_search_config({"companies": ["Acme"]})["companies"] == ["Acme"]
    assert ps.validate_people_search_config({"from_column": "company"})["from_column"] == "company"


def test_config_caps_and_defaults():
    cfg = ps.validate_people_search_config({"companies": ["Acme"]})
    assert cfg["max_per_company"] == 10  # default
    assert cfg["max_searches"] == 100    # default

    cfg = ps.validate_people_search_config(
        {"companies": ["Acme"], "max_per_company": 999, "max_searches": 9999}
    )
    assert cfg["max_per_company"] == 25   # hard cap
    assert cfg["max_searches"] == 100     # hard cap

    cfg = ps.validate_people_search_config(
        {"companies": ["Acme"], "max_per_company": 0, "max_searches": -3}
    )
    assert cfg["max_per_company"] == 1
    assert cfg["max_searches"] == 1

    with pytest.raises(ValueError):
        ps.validate_people_search_config({"companies": ["Acme"], "max_per_company": "lots"})


def test_config_normalizes_lists():
    cfg = ps.validate_people_search_config(
        {"companies": [" Acme ", "", None], "titles": [" CTO ", ""]}
    )
    assert cfg["companies"] == ["Acme"]
    assert cfg["titles"] == ["CTO"]


# ── row materialization: field vocabulary ─────────────────────────────────

def test_rows_materialized_with_person_fields(env, monkeypatch):
    spy = SearchSpy({"Acme Corp": [
        _ddg_result("Jane Smith", "CTO", "Acme Corp", "jane-smith"),
        _ddg_result("John Doe", "Founder", "Acme Corp", "john-doe"),
    ]})
    monkeypatch.setattr(cl, "_ddg_linkedin_search", spy, raising=True)

    wb_id = _mk_workbook(env, W1, _ps_col(companies=["Acme Corp"], titles=["CTO"]))
    result = _run(wb_id, "src_ps1", W1)

    assert result["added"] == 2
    assert result["searches_used"] == 1
    rows = _rows(env, wb_id)
    assert len(rows) == 2

    d = rows[0].data
    assert d["contact_person"] == "Jane Smith"
    assert d["full_name"] == "Jane Smith"
    assert d["first_name"] == "Jane"
    assert d["last_name"] == "Smith"
    assert d["title"] == "CTO"
    assert d["contact_title"] == "CTO"  # lead-field vocab (email finder input)
    assert d["company"] == "Acme Corp"
    assert d["linkedin_url"] == "https://www.linkedin.com/in/jane-smith"
    assert d["source"] == "people_search"
    assert 0 < d["confidence"] <= 1

    # identity + tenancy stamped like every source row
    assert rows[0].canonical_entity_id.startswith("person:")
    assert all(r.workspace_id == W1 for r in rows)

    with env() as db:
        wb = db.query(Workbook).filter(Workbook.id == wb_id).first()
        assert wb.total_rows == 2
        assert wb.status == "draft"


def test_domain_company_entry_sets_website(env, monkeypatch):
    """Domain entries yield a website so the email waterfall
    (contact_person + website domain) can run without an extra hop."""
    spy = SearchSpy({"acme.com": [_ddg_result("Jane Smith", "CTO", "Acme", "jane-smith")]})
    monkeypatch.setattr(cl, "_ddg_linkedin_search", spy, raising=True)

    wb_id = _mk_workbook(env, W1, _ps_col(companies=["acme.com"], titles=["CTO"]))
    _run(wb_id, "src_ps1", W1)

    (row,) = _rows(env, wb_id)
    assert row.data["website"] == "https://acme.com"
    assert row.data["contact_person"] == "Jane Smith"


def test_title_query_shape(env, monkeypatch):
    """One crosslinked-style query per title: site:linkedin.com/in "<company>" "<title>"."""
    spy = SearchSpy()
    monkeypatch.setattr(cl, "_ddg_linkedin_search", spy, raising=True)
    monkeypatch.setattr(cl.asyncio, "sleep", _no_sleep, raising=True)

    wb_id = _mk_workbook(env, W1, _ps_col(
        companies=["Acme Corp"], titles=["CTO", "Founder"], geo="Berlin", seniority="senior",
    ))
    _run(wb_id, "src_ps1", W1)

    assert spy.queries == [
        'site:linkedin.com/in "Acme Corp" "CTO" "senior" "Berlin"',
        'site:linkedin.com/in "Acme Corp" "Founder" "senior" "Berlin"',
    ]


async def _no_sleep(_secs):
    return None


# ── caps ──────────────────────────────────────────────────────────────────

def test_per_company_cap(env, monkeypatch):
    # Alphabetic names (the crosslinked snippet parser rejects digits in names);
    # dedup is by unique linkedin slug, so repeated names are fine.
    many = [
        _ddg_result(f"Adam {chr(65 + i % 26)}son", "CTO", "Acme Corp", f"p-{i}")
        for i in range(30)
    ]
    spy = SearchSpy({"Acme Corp": many})
    monkeypatch.setattr(cl, "_ddg_linkedin_search", spy, raising=True)

    wb_id = _mk_workbook(env, W1, _ps_col(
        companies=["Acme Corp"], titles=["CTO"], max_per_company=2,
    ))
    result = _run(wb_id, "src_ps1", W1)

    assert result["added"] == 2
    assert len(_rows(env, wb_id)) == 2


def test_total_search_cap(env, monkeypatch):
    spy = SearchSpy()  # every query returns nothing → 1 query per (company,title)
    monkeypatch.setattr(cl, "_ddg_linkedin_search", spy, raising=True)

    companies = [f"Company Num{i}" for i in range(10)]
    wb_id = _mk_workbook(env, W1, _ps_col(
        companies=companies, titles=["CTO"], max_searches=3,
    ))
    result = _run(wb_id, "src_ps1", W1)

    assert len(spy.queries) == 3
    assert result["searches_used"] == 3
    assert result["added"] == 0


def test_search_cap_spans_titles_within_company(env, monkeypatch):
    spy = SearchSpy()
    monkeypatch.setattr(cl, "_ddg_linkedin_search", spy, raising=True)
    monkeypatch.setattr(cl.asyncio, "sleep", _no_sleep, raising=True)

    wb_id = _mk_workbook(env, W1, _ps_col(
        companies=["A Corp", "B Corp"], titles=["CTO", "Founder", "VP"], max_searches=4,
    ))
    _run(wb_id, "src_ps1", W1)

    # 3 title queries for A Corp, then only 1 left for B Corp
    assert len(spy.queries) == 4


# ── dedup on re-run (identity mechanism) ──────────────────────────────────

def test_dedup_on_rerun(env, monkeypatch):
    spy = SearchSpy({"Acme Corp": [
        _ddg_result("Jane Smith", "CTO", "Acme Corp", "jane-smith"),
        _ddg_result("John Doe", "Founder", "Acme Corp", "john-doe"),
    ]})
    monkeypatch.setattr(cl, "_ddg_linkedin_search", spy, raising=True)

    wb_id = _mk_workbook(env, W1, _ps_col(companies=["Acme Corp"], titles=["CTO"]))

    first = _run(wb_id, "src_ps1", W1)
    assert first["added"] == 2

    second = _run(wb_id, "src_ps1", W1)
    assert second["added"] == 0
    assert second["skipped"] == 2
    assert len(_rows(env, wb_id)) == 2


def test_person_identity_prefers_linkedin_then_name_company():
    a = ps.person_identity("Jane Smith", "Acme", "https://linkedin.com/in/jane")
    b = ps.person_identity("Jane R. Smith", "Acme Inc", "https://linkedin.com/in/jane/")
    assert a == b  # same profile URL (trailing slash / name variance irrelevant)

    c = ps.person_identity("Jane Smith", "Acme", "")
    d = ps.person_identity("jane smith", "ACME", "")
    e = ps.person_identity("Jane Smith", "Other Co", "")
    assert c == d
    assert c != e
    assert c.startswith("person:")


# ── workspace isolation ───────────────────────────────────────────────────

def test_workspace_isolation(env, monkeypatch):
    spy = SearchSpy({"Acme Corp": [_ddg_result("Jane Smith", "CTO", "Acme Corp", "jane-smith")]})
    monkeypatch.setattr(cl, "_ddg_linkedin_search", spy, raising=True)

    wb1 = _mk_workbook(env, W1, _ps_col(companies=["Acme Corp"], titles=["CTO"]))
    wb2 = _mk_workbook(env, W2, _ps_col(companies=["Acme Corp"], titles=["CTO"]))

    r1 = _run(wb1, "src_ps1", W1)
    r2 = _run(wb2, "src_ps1", W2)
    assert r1["added"] == 1 and r2["added"] == 1  # dedup never crosses workbooks

    rows1, rows2 = _rows(env, wb1), _rows(env, wb2)
    assert {r.workspace_id for r in rows1} == {W1}
    assert {r.workspace_id for r in rows2} == {W2}


# ── flag gate ─────────────────────────────────────────────────────────────

def test_flag_off_makes_zero_searches(env, monkeypatch):
    monkeypatch.setattr(settings, "PEOPLE_SEARCH_SOURCE_ENABLED", False, raising=False)
    spy = SearchSpy({"Acme Corp": [_ddg_result("Jane Smith", "CTO", "Acme Corp", "j")]})
    monkeypatch.setattr(cl, "_ddg_linkedin_search", spy, raising=True)

    wb_id = _mk_workbook(env, W1, _ps_col(companies=["Acme Corp"], titles=["CTO"]))
    result = _run(wb_id, "src_ps1", W1)

    assert result["error"] == "people_search_disabled"
    assert result["added"] == 0
    assert spy.queries == []
    assert _rows(env, wb_id) == []
    with env() as db:
        assert db.query(Workbook).filter(Workbook.id == wb_id).first().status == "draft"


def test_invalid_config_at_runtime_is_graceful(env, monkeypatch):
    spy = SearchSpy()
    monkeypatch.setattr(cl, "_ddg_linkedin_search", spy, raising=True)

    wb_id = _mk_workbook(env, W1, _ps_col())  # no companies, no from_column
    result = _run(wb_id, "src_ps1", W1)

    assert "companies" in result["error"]
    assert result["added"] == 0
    assert spy.queries == []


# ── from_column: companies read off existing rows ─────────────────────────

def test_from_column_reads_companies_off_rows(env, monkeypatch):
    spy = SearchSpy({
        "Acme Corp": [_ddg_result("Jane Smith", "CTO", "Acme Corp", "jane-smith")],
        "Beta LLC": [_ddg_result("John Doe", "CTO", "Beta LLC", "john-doe")],
    })
    monkeypatch.setattr(cl, "_ddg_linkedin_search", spy, raising=True)

    company_col = {"id": "company", "name": "Company", "type": "lead_field", "lead_field": "company"}
    wb_id = _mk_workbook(
        env, W1,
        _ps_col(from_column="company", titles=["CTO"]),
        extra_cols=[company_col],
    )
    with env() as db:
        for i, name in enumerate(["Acme Corp", "Beta LLC", "Acme Corp"]):  # dup on purpose
            db.add(WorkbookRow(
                workbook_id=wb_id, workspace_id=W1, position=i + 1,
                data={"company": name}, enrichments={},
            ))
        db.commit()

    result = _run(wb_id, "src_ps1", W1)

    assert result["companies"] == 2  # deduped company values
    assert result["added"] == 2
    people_rows = [r for r in _rows(env, wb_id) if (r.data or {}).get("source") == "people_search"]
    assert {r.data["contact_person"] for r in people_rows} == {"Jane Smith", "John Doe"}
