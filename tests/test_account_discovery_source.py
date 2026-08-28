import asyncio
from types import SimpleNamespace

from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.services.leadgen.account_discovery import build_account_discovery_brief
from apps.api.services.workbook import source_engine as se
from apps.api.services.workbook.models import Base, Workbook, WorkbookRow


QUERY = "Find 2 B2B SaaS companies in India that use Stripe and are hiring partnership roles."


def test_source_engine_keeps_only_evidence_complete_accounts_and_reports_shortfall(
    monkeypatch,
):
    engine = create_engine(
        "sqlite:///:memory:",
        connect_args={"check_same_thread": False},
        poolclass=StaticPool,
    )
    Base.metadata.create_all(engine, tables=[Workbook.__table__, WorkbookRow.__table__])
    factory = sessionmaker(bind=engine, autoflush=False, autocommit=False)
    monkeypatch.setattr(se, "SessionLocal", factory)
    brief = build_account_discovery_brief(QUERY)
    source_column = {
        "id": "src_g1",
        "name": "Source",
        "type": "source",
        "icp": {"description": QUERY},
        "target_rows": 2,
        "account_discovery_brief": brief,
    }
    with factory() as db:
        db.add(Workbook(
            id="wb_g1",
            name="G1 Accounts",
            workspace_id="W1",
            source_type="account_discovery",
            source_config={"account_discovery_brief": brief},
            columns_config=[source_column],
        ))
        db.commit()

    good = {
        "id": 1,
        "company": "Fixture SaaS",
        "website": "https://fixture.example",
        "source_url": "https://evidence.example/fixture-saas",
        "specialization": "B2B SaaS",
        "address": "Bengaluru, Karnataka, India",
        "technologies": "Stripe",
        "hiring_signals": '{"roles":["Partnerships Director"]}',
        "score": 91,
        "source": "recorded_search",
        "workspace_id": "W1",
        "updated_at": "2026-08-28T09:00:00+00:00",
        "collection_job_id": "lead-job-g1",
    }
    padded = {
        **good,
        "id": 2,
        "company": "Padded Result",
        "website": "https://padded.example",
        "source_url": "https://directory.example/padded",
        "technologies": "",
        "hiring_signals": "",
    }

    class FakeRunnerDB:
        def get_job_stages(self, job_id):
            assert job_id == "lead-job-g1"
            return [
                {"stage": "web", "status": "completed", "output_count": 2},
                {"stage": "job_boards", "status": "completed", "output_count": 1},
            ]

        def close(self):
            return None

    class FakeRunner:
        def __init__(self, db):
            self.db = FakeRunnerDB()

        async def submit(self, query, workspace_id):
            assert query == QUERY
            assert workspace_id == "W1"
            return "lead-job-g1"

    class FakeStore:
        def get_leads(self, **kwargs):
            assert kwargs["collection_job_id"] == "lead-job-g1"
            return [good, padded]

        def close(self):
            return None

    entity_ids = {"Fixture SaaS": "account_fixture", "Padded Result": "account_padded"}
    monkeypatch.setattr(
        "apps.api.services.leadgen.job_runner.JobRunner",
        FakeRunner,
    )
    monkeypatch.setattr(
        "apps.api.services.leadgen.db.LeadDB",
        lambda path: object(),
    )
    monkeypatch.setattr(
        "apps.api.services.workspace.manager.workspace_slug",
        lambda workspace_id: "main",
    )
    monkeypatch.setattr(
        "apps.api.services.workspace.manager.workspace_leads_db_path",
        lambda slug: "/tmp/recorded-g1.db",
    )
    monkeypatch.setattr(
        "apps.api.services.leadgen.store.get_lead_store",
        lambda workspace_id, slug: FakeStore(),
    )
    monkeypatch.setattr(
        se,
        "resolve_company",
        lambda db, data, **kwargs: (
            SimpleNamespace(
                id=entity_ids[data["company"]],
                corroboration_count=1,
            ),
            True,
        ),
    )
    monkeypatch.setattr(se, "_make_redis", lambda: None)
    monkeypatch.setattr(
        "apps.api.services.automations.events.emit_row_added",
        lambda *args, **kwargs: None,
    )

    result = asyncio.run(se.materialize_source("wb_g1", "src_g1", "W1"))

    assert result["status"] == "partial"
    assert result["requested_count"] == 2
    assert result["delivered_count"] == 1
    assert result["shortfall"] == 1
    assert result["rejected_by_reason"] == {
        "technology_evidence_missing": 1,
        "hiring_evidence_missing": 1,
    }
    assert [source["source"] for source in result["exhausted_sources"]] == [
        "web", "job_boards",
    ]
    assert result["retry_options"] == [
        "retry_same_brief", "relax_one_filter", "add_sources",
    ]

    with factory() as db:
        workbook = db.query(Workbook).one()
        rows = db.query(WorkbookRow).all()
        assert workbook.total_rows == 1
        assert workbook.source_config["last_source_run"]["status"] == "partial"
        assert workbook.source_config["last_source_run"]["shortfall"] == 1
        assert len(rows) == 1
        assert rows[0].source_record_id == "fixture.example"
        assert rows[0].data["account_id"] == "account_fixture"
        assert rows[0].data["canonical_domain"] == "fixture.example"
        assert rows[0].data["field_confidence"] == 0.91
        assert len(rows[0].data["fit_reasons"]) == 4
        assert rows[0].data["evidence_urls"][0] == (
            "https://evidence.example/fixture-saas"
        )
