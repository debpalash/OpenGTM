"""Mapped output columns consume freshly computed values before dispatch."""
import asyncio

import pytest
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker

from apps.api.database import Base
from apps.api.services.workbook import enrichment
from apps.api.services.workbook.column_deps import (
    _refs_in, cycle_blocked_columns, downstream_columns, independent_columns,
    referencing_columns, topo_sort_columns,
)
from apps.api.services.workbook.models import Workbook, WorkbookRow


@pytest.fixture
def workbook_runner(tmp_path, monkeypatch):
    engine = create_engine(f"sqlite:///{tmp_path / 'outputs.db'}")
    Base.metadata.create_all(engine)
    sessions = sessionmaker(bind=engine, autoflush=False)
    monkeypatch.setattr(enrichment, "SessionLocal", sessions)
    monkeypatch.setattr(enrichment, "_make_redis", lambda: None)
    monkeypatch.setattr(enrichment, "flush_row_change_emits", lambda *args: None)
    monkeypatch.setattr(enrichment, "BATCH_ENABLED", False)
    sent = []

    async def append_row(spreadsheet, values, range_name, workspace_id):
        sent.append(values)
        return {"success": True, "range": range_name}

    async def push_record(fields, base, table, *, workspace_id):
        sent.append(fields)
        return {"success": True, "record_id": "record-fixture"}

    monkeypatch.setattr("apps.api.services.integrations.sheets.append_row", append_row)
    monkeypatch.setattr("apps.api.services.integrations.airtable.push_record", push_record)
    yield sessions, sent
    engine.dispose()


def create_workbook(sessions, destination, *, old_value=None, formula='upper({input})'):
    config = ({"columns": ["summary"]} if destination == "sheets"
              else {"field_map": {"summary": "Summary"}})
    columns = [
        {"id": "send", "type": "output", "destination": destination,
         "destination_config": config},
        {"id": "summary", "name": "Summary", "type": "formula", "formula": formula},
        {"id": "input", "type": "input"},
    ]
    with sessions() as db:
        wb = Workbook(name="Mapped output", workspace_id="output-deps", columns_config=columns)
        db.add(wb)
        db.flush()
        row = WorkbookRow(workbook_id=wb.id, workspace_id="output-deps", position=0,
                          data={"input": "new"},
                          enrichments={} if old_value is None else {
                              "summary": {"value": old_value, "status": "complete"}})
        db.add(row)
        db.commit()
        return wb.id


@pytest.mark.parametrize("destination", ["sheets", "airtable"])
@pytest.mark.parametrize("old_value", [None, "STALE"])
def test_runner_dispatches_new_producer_value(workbook_runner, destination, old_value):
    sessions, sent = workbook_runner
    wid = create_workbook(sessions, destination, old_value=old_value)
    result = asyncio.run(enrichment.run_workbook_enrichment(
        wid, force=True, retry_passes=0, workspace_id="output-deps"))
    assert result["completed"] == 2 and result["errors"] == 0
    expected = ["NEW"] if destination == "sheets" else {"Summary": "NEW"}
    assert sent == [expected]


@pytest.mark.parametrize("destination", ["sheets", "airtable"])
def test_partial_run_without_producer_value_does_not_send(workbook_runner, destination):
    sessions, sent = workbook_runner
    wid = create_workbook(sessions, destination)
    result = asyncio.run(enrichment.run_workbook_enrichment(
        wid, column_ids=["send"], force=True, retry_passes=0, workspace_id="output-deps"))
    assert sent == []
    assert result["completed"] == 0 and result["errors"] == 1


@pytest.mark.parametrize("destination", ["sheets", "airtable"])
def test_failed_producer_does_not_send_old_value(workbook_runner, destination):
    sessions, sent = workbook_runner
    wid = create_workbook(sessions, destination, old_value="STALE", formula="1 / 0")
    result = asyncio.run(enrichment.run_workbook_enrichment(
        wid, force=True, retry_passes=0, workspace_id="output-deps"))
    assert sent == []
    assert result["completed"] == 0 and result["errors"] == 2


def test_mapping_references_join_existing_dependency_and_cycle_guards():
    producer = {"id": "summary", "name": "Summary", "type": "formula", "formula": '"ready"'}
    send = {"id": "send", "type": "output", "destination": "instantly",
            "destination_config": {"field_map": {"Summary": "email", "{first} {last}": "name"}}}
    columns = [send, producer]
    assert _refs_in(send) == {"Summary", "first", "last"}
    assert [col["id"] for col in topo_sort_columns(columns)] == ["summary", "send"]
    assert independent_columns(columns) == []
    assert referencing_columns(columns, producer) == [send]
    assert downstream_columns(columns, {"summary"}) == [send]
    assert cycle_blocked_columns([send, {**producer, "formula": "{send}"}]) == {"summary", "send"}


def test_destination_labels_and_unrelated_configs_are_not_source_references():
    send = {"id": "send", "type": "output", "destination": "airtable",
            "destination_config": {"field_map": {"summary": "email"}, "table": "input"}}
    producer = {"id": "email", "type": "formula", "formula": '"independent"'}
    assert _refs_in(send) == {"summary"}
    assert topo_sort_columns([send, producer]) == [send, producer]
    assert _refs_in({"type": "http", "destination_config": {"field_map": {"summary": "email"}}}) == set()


@pytest.mark.parametrize("destination", ["sheets", "airtable"])
def test_partial_run_can_send_existing_producer_value(workbook_runner, destination):
    sessions, sent = workbook_runner
    wid = create_workbook(sessions, destination, old_value="KEEP")
    result = asyncio.run(enrichment.run_workbook_enrichment(
        wid, column_ids=["send"], force=True, retry_passes=0, workspace_id="output-deps"))
    expected = ["KEEP"] if destination == "sheets" else {"Summary": "KEEP"}
    assert sent == [expected]
    assert result["completed"] == 1 and result["errors"] == 0


def test_template_output_dependencies_keep_existing_behavior():
    send = {"id": "send", "type": "output", "destination": "webhook",
            "destination_config": {"body": {"summary": "{summary}"}}}
    producer = {"id": "summary", "type": "formula", "formula": '"ready"'}
    assert _refs_in(send) == {"summary"}
    assert topo_sort_columns([send, producer]) == [producer, send]
    assert _refs_in({"type": "output", "destination": "sheets", "destination_config": {}}) == set()
