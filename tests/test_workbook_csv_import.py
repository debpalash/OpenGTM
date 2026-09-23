"""Regression coverage for schema-aware workbook CSV imports."""

from fastapi import FastAPI
from fastapi.testclient import TestClient
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker
from sqlalchemy.pool import StaticPool

from apps.api.core.tenancy import WorkspaceCtx
from apps.api.database import Base, get_db
from apps.api.routers.workbooks import router, require_editor
from apps.api.services.workbook.csv_import import analyze_csv_import, prepare_csv_import
from apps.api.services.workbook.models import Workbook, WorkbookRow


WORKSPACE = "ws-csv-import"
WORKBOOK = "wb-csv-import"


class _User:
    id = "csv-user"


def _ctx():
    return WorkspaceCtx(user=_User(), workspace_id=WORKSPACE, slug="csv-test")


def _harness():
    engine = create_engine(
        "sqlite:///:memory:",
        connect_args={"check_same_thread": False},
        poolclass=StaticPool,
    )
    Base.metadata.create_all(engine)
    Session = sessionmaker(bind=engine)
    with Session() as session:
        session.add(Workbook(
            id=WORKBOOK,
            name="Clay migration",
            workspace_id=WORKSPACE,
            source_type="empty",
            columns_config=[{
                "id": "company",
                "name": "Company",
                "type": "lead_field",
                "lead_field": "company",
                "width": 220,
            }],
        ))
        session.commit()

    app = FastAPI()
    app.include_router(router)

    def override_db():
        session = Session()
        try:
            yield session
        finally:
            session.close()

    app.dependency_overrides[get_db] = override_db
    app.dependency_overrides[require_editor] = _ctx
    return TestClient(app), Session


def test_prepare_import_maps_known_headers_and_preserves_custom_columns():
    rows, columns, added, mapping = prepare_csv_import(
        [{"Company Name": "Acme", "Company Domain": "acme.test", "Rating": "4.8"}],
        [],
    )
    assert rows == [{"company": "Acme", "website": "acme.test", "rating": "4.8"}]
    assert mapping == {
        "Company Name": "company",
        "Company Domain": "website",
        "Rating": "rating",
    }
    assert [column["lead_field"] for column in columns] == ["company", "website", "rating"]
    assert [column["name"] for column in added] == ["Company Name", "Company Domain", "Rating"]


def test_imported_field_does_not_get_swallowed_by_same_named_ai_column():
    _, columns, added, mapping = prepare_csv_import(
        [{"Rating": "4.8"}],
        [{"id": "rating", "name": "Rating", "type": "ai_formula", "prompt": "Score it"}],
    )
    assert mapping["Rating"] == "rating"
    assert len(columns) == 2
    assert added == [{
        "id": "rating_2", "name": "Rating", "type": "lead_field",
        "lead_field": "rating", "width": 200,
    }]


def test_clay_preview_reports_preservation_and_mapping_collisions():
    report = analyze_csv_import(
        [{"Company Name": "Acme", "Account": "Acme Inc", "Claygent": "Uses Kafka"}],
        [], mapping={"Company Name": "company", "Account": "company"},
        file_name="accounts.csv",
    )
    assert report["source_system"] == "clay"
    assert report["detection_reason"] == "headers"
    assert report["custom_columns"] == ["Claygent"]
    assert report["collisions"] == [{"target": "company", "headers": ["Company Name", "Account"]}]


def test_preview_endpoint_is_non_mutating():
    client, Session = _harness()
    response = client.post(f"/api/workbooks/{WORKBOOK}/import/preview", json={
        "file_name": "clay-export.csv",
        "rows": [{"Company Name": "Acme", "Research Result": "SOC 2"}],
    })
    assert response.status_code == 200, response.text
    assert response.json()["source_system"] == "clay"
    with Session() as session:
        assert session.query(WorkbookRow).count() == 0
        assert len(session.get(Workbook, WORKBOOK).columns_config) == 1


def test_csv_endpoint_inserts_workbook_rows_and_extends_schema():
    client, Session = _harness()
    response = client.post(f"/api/workbooks/{WORKBOOK}/import", json={
        "file_name": "clay-export.csv",
        "rows": [
            {"Company Name": "Acme", "Company Domain": "acme.test", "Rating": "4.8", "Reviews": "120"},
            {"Company Name": "Globex", "Company Domain": "globex.test", "Rating": "4.5", "Reviews": "80"},
        ],
    })
    assert response.status_code == 200, response.text
    result = response.json()
    assert result["added"] == result["created"] == 2
    assert result["total_rows"] == 2
    assert result["skipped_duplicates"] == 0
    assert result["columns_added"] == ["Company Domain", "Rating", "Reviews"]
    assert result["analysis"]["source_system"] == "clay"

    with Session() as session:
        workbook = session.get(Workbook, WORKBOOK)
        rows = session.query(WorkbookRow).order_by(WorkbookRow.position).all()
        assert workbook.source_type == "csv"
        assert workbook.source_config["last_csv_import"]["file_name"] == "clay-export.csv"
        assert workbook.source_config["last_csv_import"]["source_system"] == "clay"
        assert [row.position for row in rows] == [0, 1]
        assert rows[0].data == {
            "company": "Acme", "website": "acme.test", "rating": "4.8", "reviews": "120",
        }
        fields = {column.get("lead_field") for column in workbook.columns_config}
        assert {"company", "website", "rating", "reviews"}.issubset(fields)


def test_csv_endpoint_honors_mapping_skip_and_dedupes():
    client, Session = _harness()
    payload = {
        "rows": [{"Account": "Acme", "URL": "acme.test", "Ignore me": "secret"}],
        "mapping": {"Account": "company", "URL": "website", "Ignore me": None},
    }
    first = client.post(f"/api/workbooks/{WORKBOOK}/import", json=payload)
    second = client.post(f"/api/workbooks/{WORKBOOK}/import", json=payload)
    assert first.status_code == second.status_code == 200
    assert first.json()["added"] == 1
    assert second.json()["added"] == 0
    assert second.json()["skipped_duplicates"] == 1

    with Session() as session:
        rows = session.query(WorkbookRow).all()
        assert len(rows) == 1
        assert rows[0].data == {"company": "Acme", "website": "acme.test"}


def test_import_preserves_execution_configuration_and_existing_evidence():
    client, Session = _harness()
    agent = {"id": "agent", "name": "Research", "type": "agent", "width": 317,
             "tools": [], "policy": {"max_steps": 0, "max_cost_usd": 0},
             "future_setting": {"keep": True}}
    evidence = {"agent": {"value": "Verified", "status": "complete", "research": {"sources": ["https://example.test"]}}}
    with Session() as session:
        workbook = session.get(Workbook, WORKBOOK)
        workbook.columns_config = [*workbook.columns_config, agent]
        workbook.source_config = {"refresh_policy": {"enabled": False}}
        session.add(WorkbookRow(workbook_id=WORKBOOK, workspace_id=WORKSPACE,
                                position=7, data={"company": "Existing"}, enrichments=evidence))
        session.commit()
    response = client.post(f"/api/workbooks/{WORKBOOK}/import", json={
        "rows": [{"Company": "New", "New field": "Preserved"}],
    })
    assert response.status_code == 200, response.text
    with Session() as session:
        workbook = session.get(Workbook, WORKBOOK)
        assert workbook.columns_config[1] == agent
        assert workbook.source_config["refresh_policy"] == {"enabled": False}
        rows = session.query(WorkbookRow).order_by(WorkbookRow.position).all()
        assert [row.position for row in rows] == [7, 8]
        assert rows[0].enrichments == evidence
        assert rows[1].data == {"company": "New", "new_field": "Preserved"}


def test_import_foreign_workbook_returns_not_found_without_mutation():
    client, Session = _harness()
    with Session() as session:
        session.get(Workbook, WORKBOOK).workspace_id = "another-workspace"
        session.commit()
    response = client.post(f"/api/workbooks/{WORKBOOK}/import", json={"rows": [{"Company": "Forbidden"}]})
    assert response.status_code == 404, response.text
    with Session() as session:
        assert session.query(WorkbookRow).count() == 0
        assert len(session.get(Workbook, WORKBOOK).columns_config) == 1
