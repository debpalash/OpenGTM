"""Terminal outcomes must survive the worker-owned session closing."""
import asyncio
import uuid

import pytest

from apps.api.core.tenancy import current_workspace_var
from apps.api.database import SessionLocal
from apps.api.services.workbook.cell_scope import row_execution_data
from apps.api.services.workbook.enrichment import _run_one_cell
from apps.api.services.workbook.models import Workbook, WorkbookEnrichment, WorkbookRow


@pytest.mark.parametrize("prior_value", [None, 42])
@pytest.mark.parametrize("column, expected_status, expected_error", [
    ({"id": "calculated", "name": "Calculated", "type": "formula", "formula": "2 + 2",
      "condition": '{flag} == "yes"'}, "skipped", "condition_not_met"),
    ({"id": "calculated", "name": "Calculated", "type": "ai_formula", "prompt": ""}, "error", "no_prompt"),
    ({"id": "calculated", "name": "Calculated", "type": "research", "prompt": ""}, "error", "no_prompt"),
])
def test_terminal_outcome_survives_worker_session(column, expected_status, expected_error, prior_value):
    workspace = "terminal-proof-" + uuid.uuid4().hex
    token = current_workspace_var.set(workspace)
    columns = [{"id": "flag", "name": "Flag", "type": "input"}, column]
    try:
        with SessionLocal() as db:
            workbook = Workbook(name="Terminal persistence", workspace_id=workspace, columns_config=columns)
            db.add(workbook)
            db.commit()
            overlay = {} if prior_value is None else {
                "calculated": {"value": prior_value, "status": "complete", "provider": "formula"}}
            row = WorkbookRow(workbook_id=workbook.id, workspace_id=workspace,
                              position=0, data={"flag": "no", "calculated": prior_value}, enrichments=overlay)
            db.add(row)
            db.commit()
            wid, rid = workbook.id, row.id
            if prior_value is not None:
                db.add(WorkbookEnrichment(workbook_id=wid, workspace_id=workspace,
                                         lead_id=rid, column_id="calculated", value="42", status="complete"))
                db.commit()
            execution_data = row_execution_data(row, columns)
        result = asyncio.run(_run_one_cell(wid, execution_data, column, columns, None))
        assert result["success"] is False
        assert result["error"] == expected_error
        with SessionLocal() as db:
            row = db.query(WorkbookRow).filter_by(id=rid).one()
            receipt = db.query(WorkbookEnrichment).filter_by(workbook_id=wid, column_id="calculated").one()
            assert receipt.status == expected_status
            assert receipt.value is None
            cell = row.enrichments["calculated"]
            assert cell["status"] == expected_status
            assert cell["value"] is None
            assert cell["error"] == (None if expected_status == "skipped" else expected_error)
            assert "calculated" not in row_execution_data(row, columns)
    finally:
        current_workspace_var.reset(token)


def test_successful_formula_still_commits():
    workspace = "terminal-control-" + uuid.uuid4().hex
    token = current_workspace_var.set(workspace)
    col = {"id": "calculated", "name": "Calculated", "type": "formula", "formula": "2 + 2"}
    try:
        with SessionLocal() as db:
            workbook = Workbook(name="Success control", workspace_id=workspace, columns_config=[col])
            db.add(workbook)
            db.commit()
            row = WorkbookRow(workbook_id=workbook.id, workspace_id=workspace,
                              position=0, data={}, enrichments={})
            db.add(row)
            db.commit()
            wid, rid = workbook.id, row.id
            execution_data = row_execution_data(row, [col])
        result = asyncio.run(_run_one_cell(wid, execution_data, col, [col], None))
        assert result["success"] is True and result["value"] == 4
        with SessionLocal() as db:
            row = db.query(WorkbookRow).filter_by(id=rid).one()
            assert row.enrichments["calculated"]["status"] == "complete"
            assert row.enrichments["calculated"]["value"] == 4
    finally:
        current_workspace_var.reset(token)


@pytest.mark.parametrize("loss", ["reclaimed", "cancelled"])
@pytest.mark.parametrize("kind", ["skip", "ai_formula", "research"])
def test_terminal_write_rechecks_lease_after_worker_preflight(loss, kind):
    from datetime import datetime, timedelta
    from sqlalchemy import event
    from apps.api.models import Job
    from apps.api.services.workbook.batch_attempts import batch_lease_scope

    workspace = "terminal-lease-" + uuid.uuid4().hex
    token = current_workspace_var.set(workspace)
    col = {"id":"calculated", "name":"Calculated", "type":kind, "prompt":""}
    if kind == "skip":
        col = {"id":"calculated", "name":"Calculated", "type":"formula", "formula":"2 + 2",
               "condition":'{flag} == "yes"'}
    columns = [{"id":"flag", "name":"Flag", "type":"input"}, col]
    prior_cell = {"value":42, "status":"complete", "provider":"formula"}
    locked_at = datetime(2026, 1, 1)
    try:
        with SessionLocal() as db:
            wb = Workbook(name="Lease control", workspace_id=workspace, columns_config=columns, status="running")
            db.add(wb)
            db.commit()
            row = WorkbookRow(workbook_id=wb.id, workspace_id=workspace, position=0,
                              data={"flag":"no"}, enrichments={"calculated":prior_cell})
            db.add(row)
            db.commit()
            wid, rid = wb.id, row.id
            db.add(WorkbookEnrichment(workbook_id=wid, workspace_id=workspace, lead_id=rid,
                                     column_id="calculated", value="42", status="complete"))
            job = Job(type="run_workbook", workspace_id=workspace, status="processing",
                      worker_id="original", locked_at=locked_at, payload={"workbook_id":wid})
            db.add(job)
            db.commit()
            jid = job.id
            execution_data = row_execution_data(row, columns)
        payload = {"workspace_id":workspace, "workbook_id":wid,
                   "__queue_lease":{"worker_id":"original", "locked_at":locked_at.isoformat()}}
        changed = []

        def revoke_after_preflight(session, transaction):
            if transaction.parent is not None or changed:
                return
            changed.append(True)
            # The first worker transaction is preflight. Its writer lock has
            # been released; a real independent transaction changes ownership.
            with SessionLocal() as db:
                job = db.get(Job, jid)
                if loss == "reclaimed":
                    job.worker_id = "replacement"
                    job.locked_at = locked_at + timedelta(seconds=60)
                else:
                    job.status = "cancelled"
                    db.get(Workbook, wid).status = "paused"
                db.commit()

        event.listen(SessionLocal, "after_transaction_end", revoke_after_preflight)
        try:
            with batch_lease_scope(jid, payload):
                result = asyncio.run(_run_one_cell(wid, execution_data, col, columns, None))
        finally:
            event.remove(SessionLocal, "after_transaction_end", revoke_after_preflight)
        assert changed == [True]
        assert result["success"] is False and "lease" in result["error"]
        with SessionLocal() as db:
            assert db.get(WorkbookRow, rid).enrichments["calculated"] == prior_cell
            receipt = db.query(WorkbookEnrichment).filter_by(workbook_id=wid, column_id="calculated").one()
            assert receipt.status == "complete" and receipt.value == "42"
            assert db.get(Job, jid).worker_id == ("replacement" if loss == "reclaimed" else "original")
            assert db.get(Job, jid).status == ("processing" if loss == "reclaimed" else "cancelled")
    finally:
        current_workspace_var.reset(token)
