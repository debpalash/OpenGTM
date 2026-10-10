"""The public migration route snapshots every matching legacy lead."""
import asyncio
import uuid

import pytest

from apps.api.core.tenancy import WorkspaceCtx, current_workspace_var
from apps.api.database import SessionLocal
from apps.api.routers.workbooks import migrate_workbook_to_v2
from apps.api.services.leadgen.models import Lead
from apps.api.services.workbook.models import Workbook, WorkbookEnrichment, WorkbookRow


@pytest.mark.parametrize("count", [0, 2, 500, 501, 1001])
def test_migration_snapshots_all_pages(count):
    workspace = "migration-proof-" + uuid.uuid4().hex
    token = current_workspace_var.set(workspace)
    ctx = WorkspaceCtx(user=None, workspace_id=workspace, slug=workspace)
    try:
        store = ctx.lead_db()
        try:
            ids = [store.upsert_lead(Lead(company=f"Company {i}", city="Proof", score=count-i,
                                          workspace_id=workspace)) for i in range(count)]
            store.upsert_lead(Lead(company="Excluded company", city="Elsewhere", workspace_id=workspace))
        finally:
            store.close()
        with SessionLocal() as db:
            wb = Workbook(name="Legacy migration", workspace_id=workspace, source_type="leads_filter",
                          filter_criteria={"city": "Proof"},
                          columns_config=[{"id":"company","type":"lead_field","lead_field":"company"}])
            db.add(wb)
            db.commit()
            wid = wb.id
            if ids:
                db.add(WorkbookEnrichment(workbook_id=wid, workspace_id=workspace, lead_id=ids[-1],
                                         column_id="tail", value="tail result", status="complete"))
                db.commit()
            result = asyncio.run(migrate_workbook_to_v2(wid, db=db, ctx=ctx))
        assert result["rows"] == count
        with SessionLocal() as db:
            rows = db.query(WorkbookRow).filter_by(workbook_id=wid).order_by(WorkbookRow.position).all()
            assert [row.lead_id for row in rows] == ids
            assert [row.position for row in rows] == list(range(count))
            if rows:
                assert rows[-1].enrichments["tail"]["value"] == "tail result"
            assert db.get(Workbook, wid).source_config["row_storage_version"] == 2
            if count:
                rerun = asyncio.run(migrate_workbook_to_v2(wid, db=db, ctx=ctx))
                assert rerun == {"status":"already_migrated", "rows":count}
                assert db.query(WorkbookRow).filter_by(workbook_id=wid).count() == count
    finally:
        current_workspace_var.reset(token)


@pytest.mark.parametrize("failure", ["raise", "empty"])
def test_later_page_failure_rolls_back_snapshot(monkeypatch, failure):
    from apps.api.routers import workbooks
    workspace = "migration-failure-" + uuid.uuid4().hex
    token = current_workspace_var.set(workspace)
    ctx = WorkspaceCtx(user=None, workspace_id=workspace, slug=workspace)
    try:
        store = ctx.lead_db()
        try:
            for i in range(501):
                store.upsert_lead(Lead(company=f"Company {i}", city="Proof", score=501-i,
                                       workspace_id=workspace))
        finally:
            store.close()
        with SessionLocal() as db:
            wb = Workbook(name="Interrupted migration", workspace_id=workspace, source_type="leads_filter",
                          filter_criteria={"city":"Proof"}, columns_config=[])
            db.add(wb)
            db.commit()
            wid = wb.id
        original = workbooks._query_leads

        def interrupted(store, filters, page=1, page_size=100):
            if page == 2:
                if failure == "empty":
                    return [], 501
                raise RuntimeError("Source page unavailable")
            return original(store, filters, page=page, page_size=page_size)

        with monkeypatch.context() as patch:
            patch.setattr(workbooks, "_query_leads", interrupted)
            with SessionLocal() as db:
                with pytest.raises(RuntimeError):
                    asyncio.run(workbooks.migrate_workbook_to_v2(wid, db=db, ctx=ctx))
        with SessionLocal() as db:
            assert db.query(WorkbookRow).filter_by(workbook_id=wid).count() == 0
            assert (db.get(Workbook, wid).source_config or {}).get("row_storage_version") != 2
            retry = asyncio.run(workbooks.migrate_workbook_to_v2(wid, db=db, ctx=ctx))
            assert retry["rows"] == 501
    finally:
        current_workspace_var.reset(token)
