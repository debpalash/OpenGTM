"""Standing refresh spends only on stale cells, preserving fresh results."""
import asyncio
from datetime import datetime, timedelta, timezone

import pytest
from sqlalchemy import create_engine
from sqlalchemy.orm import sessionmaker

from apps.api.database import Base
from apps.api.services.workbook import enrichment, refresh
from apps.api.services.workbook.models import Workbook, WorkbookEnrichment, WorkbookRow


@pytest.fixture
def refresh_runner(tmp_path, monkeypatch):
    engine = create_engine(f"sqlite:///{tmp_path / 'refresh-cells.db'}")
    Base.metadata.create_all(engine)
    sessions = sessionmaker(bind=engine, autoflush=False)
    now = datetime.now(timezone.utc)
    calls, sent = [], []

    async def ai(**kwargs):
        email = kwargs["row_cells"]["email"]["value"]
        calls.append((kwargs["prompt_template"], email))
        return {"value": "UPDATED", "error": None}

    async def append_row(spreadsheet, values, range_name, workspace_id):
        sent.append(values)
        return {"success": True, "range": range_name}

    monkeypatch.setattr(refresh, "SessionLocal", sessions)
    monkeypatch.setattr(refresh, "_now", lambda: now)
    monkeypatch.setattr(enrichment, "SessionLocal", sessions)
    monkeypatch.setattr(enrichment, "execute_ai_column", ai)
    monkeypatch.setattr(enrichment, "_make_redis", lambda: None)
    monkeypatch.setattr(enrichment, "flush_row_change_emits", lambda *args: None)
    monkeypatch.setattr(enrichment, "BATCH_ENABLED", False)
    monkeypatch.setattr("apps.api.services.integrations.sheets.append_row", append_row)
    yield sessions, now, calls, sent
    engine.dispose()


def populate(sessions, now, linked, *, all_fresh=False, stale_status="complete"):
    columns = [
        {"id": "email", "type": "input"},
        {"id": "summary", "type": "ai_formula", "prompt": "summary {email}"},
        {"id": "pitch", "type": "ai_formula", "prompt": "pitch {email}"},
        {"id": "send", "type": "output", "destination": "sheets",
         "destination_config": {"columns": ["email"]}},
    ]
    with sessions() as db:
        wb = Workbook(name="Refresh scope", workspace_id="refresh-scope", columns_config=columns,
                      refresh_policy={"staleness_ttl_days": {"summary": 7, "pitch": 30}})
        db.add(wb)
        db.flush()
        wid = wb.id
        for row_id, stale_col in [(101, "summary"), (202, "pitch")]:
            lead_id = row_id + 800 if linked else None
            row = WorkbookRow(id=row_id, workbook_id=wid, workspace_id="refresh-scope",
                              position=row_id, lead_id=lead_id,
                              data={"email": f"row{row_id}@example.test"}, enrichments={})
            db.add(row)
            db.flush()
            for col_id in ["summary", "pitch"]:
                stale = col_id == stale_col and not all_fresh
                cell = {"value": f"KEEP-{row_id}-{col_id}", "status": stale_status if stale else "complete"}
                row.enrichments = {**row.enrichments, col_id: cell}
                db.add(WorkbookEnrichment(
                    workbook_id=wid, workspace_id="refresh-scope", lead_id=lead_id or row_id,
                    column_id=col_id, value=cell["value"], status=cell["status"],
                    updated_at=now - timedelta(days=60) if stale else now))
        db.commit()
        return wid


@pytest.mark.parametrize("linked", [False, True])
@pytest.mark.parametrize("stale_status", ["complete", "error"])
def test_refresh_only_runs_each_rows_stale_cell(refresh_runner, linked, stale_status):
    sessions, now, calls, sent = refresh_runner
    wid = populate(sessions, now, linked, stale_status=stale_status)
    result = asyncio.run(refresh.refresh_workbook(wid, workspace_id="refresh-scope"))
    assert sorted(calls) == [("pitch {email}", "row202@example.test"),
                             ("summary {email}", "row101@example.test")]
    assert sent == []  # A schedule is not permission to dispatch output columns.
    assert result == {"sourced": 0, "reenriched": 2, "stale_rows": 2}
    with sessions() as db:
        for row_id, stale_col in [(101, "summary"), (202, "pitch")]:
            row = db.get(WorkbookRow, row_id)
            assert row.enrichments[stale_col]["value"] == "UPDATED"
            fresh_col = "pitch" if stale_col == "summary" else "summary"
            assert row.enrichments[fresh_col] == {"value": f"KEEP-{row_id}-{fresh_col}", "status": "complete"}
            assert "send" not in row.enrichments
    assert asyncio.run(refresh.refresh_workbook(wid, workspace_id="refresh-scope"))["stale_rows"] == 0
    assert len(calls) == 2 and sent == []


@pytest.mark.parametrize("linked", [False, True])
def test_all_fresh_cells_make_refresh_a_noop(refresh_runner, linked):
    sessions, now, calls, sent = refresh_runner
    wid = populate(sessions, now, linked, all_fresh=True)
    result = asyncio.run(refresh.refresh_workbook(wid, workspace_id="refresh-scope"))
    assert result == {"sourced": 0, "reenriched": 0, "stale_rows": 0}
    assert calls == sent == []
