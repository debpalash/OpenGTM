"""Standing refresh uses persisted workbook row identities, not optional lead links."""
import asyncio
from datetime import datetime, timezone

import pytest
from sqlalchemy import create_engine, event
from sqlalchemy.orm import sessionmaker

from apps.api.database import Base
from apps.api.services.workbook import enrichment, refresh
from apps.api.services.workbook.models import Workbook, WorkbookEnrichment, WorkbookRow
from apps.api.services.workbook.activity_models import WorkbookActivity


@pytest.mark.parametrize("linked", [False, True])
def test_refresh_executes_stale_row_and_preserves_fresh_row(tmp_path, monkeypatch, linked):
    connection = create_engine(f"sqlite:///{tmp_path / 'refresh.db'}")
    @event.listens_for(connection, "connect")
    def foreign_keys(dbapi, _):
        dbapi.execute("PRAGMA foreign_keys=ON")
    Base.metadata.create_all(connection)
    sessions = sessionmaker(bind=connection, autoflush=False)
    with sessions() as db:
        wb = Workbook(name="Refresh fixture", workspace_id="refresh-test",
                      columns_config=[{"id": "summary", "type": "ai_formula", "prompt": "Summarize {email}"}])
        db.add(wb)
        db.flush()
        wid = wb.id
        stale = WorkbookRow(id=101, workbook_id=wid, workspace_id="refresh-test", position=0,
                            lead_id=901 if linked else None, data={"email": "stale@example.test"})
        fresh = WorkbookRow(id=202, workbook_id=wid, workspace_id="refresh-test", position=1,
                            lead_id=902 if linked else None, data={"email": "fresh@example.test"},
                            enrichments={"summary": {"value": "KEEP", "status": "complete"}})
        db.add_all([stale, fresh])
        db.flush()
        db.add(WorkbookEnrichment(workbook_id=wid, workspace_id="refresh-test", lead_id=fresh.lead_id or fresh.id,
                                 column_id="summary", status="complete", value="KEEP",
                                 updated_at=datetime.now(timezone.utc)))
        db.commit()
    calls = []
    async def provider(**kwargs):
        calls.append(kwargs["row_cells"]["email"]["value"])
        return {"value": "UPDATED", "error": None}
    monkeypatch.setattr(refresh, "SessionLocal", sessions)
    monkeypatch.setattr(enrichment, "SessionLocal", sessions)
    monkeypatch.setattr(enrichment, "execute_ai_column", provider)
    monkeypatch.setattr(enrichment, "_make_redis", lambda: None)
    monkeypatch.setattr(enrichment, "flush_row_change_emits", lambda *args: None)
    try:
        result = asyncio.run(refresh.refresh_workbook(wid, workspace_id="refresh-test"))
        assert result == {"sourced": 0, "reenriched": 1, "stale_rows": 1}
        assert calls == ["stale@example.test"]
        with sessions() as db:
            assert db.get(WorkbookRow, 101).enrichments["summary"]["value"] == "UPDATED"
            assert db.get(WorkbookRow, 202).enrichments["summary"] == {"value": "KEEP", "status": "complete"}
            assert {(o.lead_id, o.value) for o in db.query(WorkbookEnrichment)} == {(901 if linked else 101, "UPDATED"), (902 if linked else 202, "KEEP")}
            assert db.query(WorkbookActivity).one().kind == "refresh"
        # Fresh receipts must make a second scheduled cycle a no-op.
        assert asyncio.run(refresh.refresh_workbook(wid, workspace_id="refresh-test"))["reenriched"] == 0
        assert len(calls) == 1
    finally:
        connection.dispose()
