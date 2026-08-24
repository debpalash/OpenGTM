"""Offline regression tests for AmbitionBox chat search behavior."""

import asyncio
import json

import pytest

from apps.api.services.leadgen.ambitionbox import (
    AmbitionBoxClient,
    _industry_slug,
)
from apps.api.services.connectors.contracts import (
    ConnectorCollection,
    ConnectorPage,
    ConnectorRecord,
)


def run(coro):
    return asyncio.run(coro)


def test_user_facing_industry_aliases_match_ambitionbox_taxonomy():
    assert _industry_slug("Human Resources") == "recruitment"
    assert _industry_slug("HR Services") == "recruitment"
    assert _industry_slug("HR/Recruitment") == "recruitment"
    assert _industry_slug("Staffing") == "recruitment"
    assert _industry_slug("SaaS") == "software-product"
    assert _industry_slug("IT Services & Consulting") == "it-services-and-consulting"


def test_search_treats_null_cards_as_empty_result(monkeypatch):
    client = AmbitionBoxClient()

    async def fake_request(method, path, json_body=None):
        return {"cards": None}

    monkeypatch.setattr(client, "_request", fake_request)
    result = run(client.search_companies(industry=["unknown category"]))

    assert result == {
        "companies": [], "total": 0, "page": 1,
        "source_total": None, "total_pages": None, "has_more": False,
    }


def test_collect_fetches_five_pages_for_100_unique_companies(monkeypatch):
    client = AmbitionBoxClient()
    calls = []

    async def fake_fetch(**kwargs):
        calls.append(kwargs)
        page = kwargs["page"]
        return ConnectorPage.from_records(
            provider="ambitionbox", page=page, page_size=20,
            source_total=100, total_pages=5,
            records=[
                ConnectorRecord(
                    provider="ambitionbox", record_id=str(page * 100 + index),
                    rank=(page - 1) * 20 + index,
                    data={"company_id": page * 100 + index, "name": f"Company {page}-{index}"},
                )
                for index in range(20)
            ],
        )

    monkeypatch.setattr(client, "fetch_company_page", fake_fetch)
    companies = run(client.search_and_collect(
        pages=5,
        industry=["Human Resources"],
        sort_by="popular",
        limit=100,
    ))

    assert len(companies) == 100
    assert [call["page"] for call in calls] == [1, 2, 3, 4, 5]
    assert all(call["industry"] == ["Human Resources"] for call in calls)


def test_collect_does_not_disguise_a_failed_page_as_a_short_result(monkeypatch):
    client = AmbitionBoxClient()

    async def fake_fetch(**kwargs):
        if kwargs["page"] == 2:
            raise RuntimeError("temporary gateway failure")
        return ConnectorPage.from_records(
            provider="ambitionbox", page=kwargs["page"], page_size=20,
            total_pages=5,
            records=[
                ConnectorRecord(
                    provider="ambitionbox", record_id=str(index), rank=index,
                    data={"company_id": index, "name": f"Company {index}"},
                )
                for index in range(20)
            ],
        )

    monkeypatch.setattr(client, "fetch_company_page", fake_fetch)
    with pytest.raises(RuntimeError, match="failed on page 2"):
        run(client.search_and_collect(pages=5, limit=100))


def test_chat_tool_uses_multi_page_collection_for_top_100(monkeypatch):
    import apps.api.routers.copilotkit as copilotkit
    import apps.api.services.leadgen.ambitionbox as ambitionbox_module

    captured = {}

    class FakeAmbitionBox:
        async def collect_companies(self, **kwargs):
            captured.update(kwargs)
            records = tuple(
                ConnectorRecord(
                    provider="ambitionbox", record_id=str(i), rank=i,
                    data={"company_id": i, "name": f"HR Company {i}"},
                ) for i in range(100)
            )
            return ConnectorCollection(
                provider="ambitionbox", records=records, requested_count=100,
                pages_fetched=5, next_page=6, source_total=4365,
                target_met=True, exhausted=False,
            )

        async def search_companies(self, **kwargs):
            raise AssertionError("top-100 search must use multi-page collection")

    monkeypatch.setattr(ambitionbox_module, "ambitionbox", FakeAmbitionBox())
    result = json.loads(run(copilotkit._execute_tool(
        "ambitionbox_search",
        {"industry": "Human Resources", "limit": 100, "sort_by": "popular"},
        store=object(),
        workspace_id="workspace-1",
        slug="workspace-1",
    )))

    assert result["total"] == 100
    assert result["pages_fetched"] == 5
    assert result["target_met"] is True
    assert result["partial"] is False
    assert result["requested_limit"] == 100
    assert captured == {
        "requested_count": 100,
        "max_pages": 10,
        "industry": ["Human Resources"],
        "sort_by": "popular",
        "rating": None,
    }


def test_ambitionbox_import_tool_starts_durable_run(monkeypatch):
    import apps.api.routers.copilotkit as copilotkit
    import apps.api.services.workbook.ambitionbox_import as import_module

    captured = {}

    def fake_start(**kwargs):
        captured.update(kwargs)
        return {
            "workbook_id": "wb-ambitionbox",
            "name": kwargs["name"],
            "run_id": "run-1",
            "job_id": 42,
            "status": "pending",
            "requested_limit": kwargs["requested_limit"],
            "source": "ambitionbox",
            "url": "/workbooks/wb-ambitionbox",
        }

    monkeypatch.setattr(import_module, "start_ambitionbox_import", fake_start)

    result = json.loads(run(copilotkit._execute_tool(
        "import_ambitionbox_to_workbook",
        {
            "industry": "Human Resources",
            "limit": 100,
            "sort_by": "popular",
            "name": "AmbitionBox HR/Recruitment Companies",
        },
        store=object(),
        workspace_id="workspace-1",
        slug="workspace-1",
    )))

    assert result["workbook_id"] == "wb-ambitionbox"
    assert result["run_id"] == "run-1"
    assert result["status"] == "pending"
    assert captured == {
        "workspace_id": "workspace-1",
        "name": "AmbitionBox HR/Recruitment Companies",
        "industry": "Human Resources",
        "sort_by": "popular",
        "rating": None,
        "requested_limit": 100,
    }
    assert copilotkit._needs_confirmation("import_ambitionbox_to_workbook")


def test_ambitionbox_workbook_preserves_ranked_source_fields(monkeypatch):
    from sqlalchemy import create_engine
    from sqlalchemy.orm import sessionmaker
    from sqlalchemy.pool import StaticPool

    from apps.api.database import Base
    from apps.api.services.workbook.models import Workbook, WorkbookRow
    import apps.api.services.workbook.ambitionbox_import as import_module

    engine = create_engine(
        "sqlite://",
        connect_args={"check_same_thread": False},
        poolclass=StaticPool,
    )
    session_factory = sessionmaker(bind=engine)
    Base.metadata.create_all(
        engine, tables=[Workbook.__table__, WorkbookRow.__table__]
    )
    monkeypatch.setattr(import_module, "SessionLocal", session_factory)

    quess = {
        "company_id": 14207,
        "name": "Quess",
        "industry": "Recruitment",
        "rating": 3.8,
        "review_count": 10169,
        "jobs_count": 15,
        "salaries_count": 32094,
        "interviews_count": 557,
        "employee_count": "100001+",
        "top_location": "Bengaluru",
        "total_locations": 781,
        "is_verified": True,
        "company_type": "Public",
        "profile_url": "https://www.ambitionbox.com/overview/quess-overview",
    }
    randstad = {
        "company_id": 12181,
        "name": "Randstad",
        "industry": "Recruitment",
        "rating": 3.7,
        "review_count": 4909,
        "employee_count": "50001-100000",
        "top_location": "Hyderabad",
        "profile_url": "https://www.ambitionbox.com/overview/randstad-overview",
    }

    result = import_module.create_ambitionbox_workbook(
        [quess, randstad, dict(quess)],
        workspace_id="workspace-1",
        name="Top Recruitment Companies",
        industry="Human Resources",
        requested_limit=100,
    )

    with session_factory() as db:
        wb = db.query(Workbook).one()
        rows = db.query(WorkbookRow).order_by(WorkbookRow.position).all()

        assert result["rows_added"] == 2
        assert wb.id == result["workbook_id"]
        assert wb.workspace_id == "workspace-1"
        assert wb.source_type == "ambitionbox"
        assert wb.sync_to_leads is False
        assert wb.total_rows == 2
        assert [row.data["company"] for row in rows] == ["Quess", "Randstad"]
        assert rows[0].data["ambitionbox_rating"] == 3.8
        assert rows[0].data["review_count"] == 10169
        assert rows[0].data["company_size"] == "100001+"
        assert rows[0].data["ambitionbox_url"].endswith("quess-overview")
        assert rows[0].source_provider == "ambitionbox"
        assert rows[0].source_record_id == "14207"
        assert rows[0].data["_source"]["provider"] == "ambitionbox"
        assert all(row.workspace_id == "workspace-1" for row in rows)
        assert all(row.lead_id is None for row in rows)


def test_collection_fetches_extra_page_when_cross_page_duplicates_reduce_yield(monkeypatch):
    client = AmbitionBoxClient()
    calls = []

    async def fake_fetch(**kwargs):
        page = kwargs["page"]
        calls.append(page)
        start = {1: 0, 2: 10, 3: 30}[page]
        records = [
            ConnectorRecord(
                provider="ambitionbox", record_id=str(i), rank=(page - 1) * 20 + n,
                data={"company_id": i, "name": f"Company {i}"},
            )
            for n, i in enumerate(range(start, start + 20))
        ]
        return ConnectorPage.from_records(
            provider="ambitionbox", records=records, page=page, page_size=20,
            source_total=100, total_pages=5,
        )

    monkeypatch.setattr(client, "fetch_company_page", fake_fetch)
    result = run(client.collect_companies(requested_count=40, max_pages=3))

    assert calls == [1, 2, 3]
    assert len(result.records) == 40
    assert result.target_met is True
    assert result.partial is False


def test_collection_reports_partial_when_source_is_exhausted(monkeypatch):
    client = AmbitionBoxClient()

    async def fake_fetch(**kwargs):
        return ConnectorPage.from_records(
            provider="ambitionbox", page=1, page_size=20,
            source_total=2, total_pages=1,
            records=[
                ConnectorRecord(
                    provider="ambitionbox", record_id=str(i), rank=i,
                    data={"company_id": i, "name": f"Company {i}"},
                ) for i in range(2)
            ],
        )

    monkeypatch.setattr(client, "fetch_company_page", fake_fetch)
    result = run(client.collect_companies(requested_count=100, max_pages=10))

    assert result.status == "partial"
    assert result.target_met is False
    assert result.exhausted is True
    assert result.source_total == 2


def test_durable_import_resumes_from_committed_page_without_duplicate_rows(monkeypatch):
    from sqlalchemy import create_engine
    from sqlalchemy.orm import sessionmaker
    from sqlalchemy.pool import StaticPool

    from apps.api.database import Base
    from apps.api.services.workbook.models import ConnectorRun, Workbook, WorkbookRow
    import apps.api.services.leadgen.ambitionbox as ambitionbox_module
    import apps.api.services.workbook.ambitionbox_import as import_module
    import apps.api.services.workbook.enrichment as enrichment_module

    engine = create_engine(
        "sqlite://", connect_args={"check_same_thread": False}, poolclass=StaticPool
    )
    Session = sessionmaker(bind=engine)
    Base.metadata.create_all(
        engine, tables=[Workbook.__table__, WorkbookRow.__table__, ConnectorRun.__table__]
    )
    monkeypatch.setattr(import_module, "SessionLocal", Session)
    monkeypatch.setattr(enrichment_module, "_make_redis", lambda: None)

    calls = []
    fail_page_two = {"value": True}

    class FakeAmbitionBox:
        async def fetch_company_page(self, **kwargs):
            page = kwargs["page"]
            calls.append(page)
            if page == 2 and fail_page_two["value"]:
                raise RuntimeError("page two transient failure")
            start = (page - 1) * 2
            records = [
                ConnectorRecord(
                    provider="ambitionbox", record_id=str(i), rank=i,
                    data={
                        "company_id": i, "name": f"Company {i}",
                        "profile_url": f"https://example.test/{i}",
                    },
                ) for i in range(start, start + 2)
            ]
            return ConnectorPage.from_records(
                provider="ambitionbox", records=records, page=page, page_size=2,
                source_total=4, total_pages=2,
            )

    monkeypatch.setattr(ambitionbox_module, "ambitionbox", FakeAmbitionBox())

    with Session() as db:
        wb = Workbook(
            id="wb-1", workspace_id="ws-1", name="AB", source_type="ambitionbox",
            columns_config=[], source_config={}, status="running",
        )
        run_row = ConnectorRun(
            id="run-1", workspace_id="ws-1", workbook_id="wb-1",
            connector="ambitionbox", status="pending",
            query={"sort_by": "popular", "max_pages": 5},
            cursor={"next_page": 1, "reset_done": False}, requested_count=4,
        )
        db.add_all([wb, run_row])
        db.commit()

    payload = {"workspace_id": "ws-1", "workbook_id": "wb-1", "run_id": "run-1"}
    with pytest.raises(RuntimeError, match="page two transient failure"):
        run(import_module.handle_ambitionbox_import(1, payload))

    with Session() as db:
        checkpoint = db.query(ConnectorRun).one()
        assert checkpoint.cursor["next_page"] == 2
        assert checkpoint.fetched_count == 2
        assert checkpoint.status == "retrying"
        assert db.query(Workbook).one().status == "running"
        assert db.query(Workbook).one().source_config["run_status"] == "retrying"
        assert db.query(WorkbookRow).count() == 2

    import_module.reconcile_ambitionbox_job_failure(
        1, payload, "page two terminal failure", will_retry=False
    )
    with Session() as db:
        checkpoint = db.query(ConnectorRun).one()
        assert checkpoint.status == "failed"
        assert checkpoint.completed_at is not None
        assert db.query(Workbook).one().status == "draft"

    import_module.reconcile_ambitionbox_job_failure(
        1, payload, "page two transient failure", will_retry=True
    )
    with Session() as db:
        checkpoint = db.query(ConnectorRun).one()
        assert checkpoint.status == "retrying"
        assert db.query(Workbook).one().status == "running"

    fail_page_two["value"] = False
    result = run(import_module.handle_ambitionbox_import(1, payload))

    assert calls == [1, 2, 2]
    assert result["status"] == "complete"
    assert result["fetched_count"] == 4
    with Session() as db:
        rows = db.query(WorkbookRow).order_by(WorkbookRow.source_rank).all()
        assert len(rows) == 4
        assert [row.source_record_id for row in rows] == ["0", "1", "2", "3"]
        assert db.query(ConnectorRun).one().cursor["next_page"] == 3


def test_durable_import_rejects_overlapping_run_for_same_workbook(monkeypatch):
    from sqlalchemy import create_engine
    from sqlalchemy.orm import sessionmaker
    from sqlalchemy.pool import StaticPool

    from apps.api.database import Base
    from apps.api.models import Job
    from apps.api.services.workbook.models import ConnectorRun, Workbook
    import apps.api.services.workbook.ambitionbox_import as import_module

    engine = create_engine(
        "sqlite://", connect_args={"check_same_thread": False}, poolclass=StaticPool
    )
    Session = sessionmaker(bind=engine)
    Base.metadata.create_all(
        engine, tables=[Workbook.__table__, ConnectorRun.__table__, Job.__table__]
    )
    monkeypatch.setattr(import_module, "SessionLocal", Session)

    with Session() as db:
        db.add(Workbook(
            id="wb-single-flight", workspace_id="ws-1", name="AB",
            source_type="ambitionbox", source_config={}, columns_config=[],
            status="draft",
        ))
        db.commit()

    first = import_module.start_ambitionbox_import(
        workspace_id="ws-1", workbook_id="wb-single-flight",
        replace_existing=True, name="AB", requested_limit=100,
    )
    with pytest.raises(
        import_module.AmbitionBoxImportAlreadyRunning,
        match=first["run_id"],
    ):
        import_module.start_ambitionbox_import(
            workspace_id="ws-1", workbook_id="wb-single-flight",
            replace_existing=True, name="AB", requested_limit=100,
        )

    with Session() as db:
        assert db.query(ConnectorRun).count() == 1
        assert db.query(Job).count() == 1
        db.query(ConnectorRun).one().status = "complete"
        db.commit()

    second = import_module.start_ambitionbox_import(
        workspace_id="ws-1", workbook_id="wb-single-flight",
        replace_existing=True, name="AB refresh", requested_limit=20,
    )
    assert second["run_id"] != first["run_id"]
    with Session() as db:
        assert db.query(ConnectorRun).count() == 2
        assert db.query(Job).count() == 2
