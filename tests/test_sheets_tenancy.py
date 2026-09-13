import asyncio

from apps.api.services.integrations import sheets
from apps.api.services.workbook.output import execute_output_column


def test_sheets_resolves_token_for_job_workspace(monkeypatch):
    observed = {}

    def fake_secret(workspace_id, key, default):
        observed.update(workspace_id=workspace_id, key=key, default=default)
        return "workspace-token"

    monkeypatch.setattr(
        "apps.api.services.workspace.secrets.get_secret",
        fake_secret,
    )
    assert sheets._token("ws-a") == "workspace-token"
    assert observed == {
        "workspace_id": "ws-a",
        "key": "GOOGLE_SHEETS_TOKEN",
        "default": "",
    }


def test_workbook_output_threads_workspace_into_sheets_adapter(monkeypatch):
    observed = {}

    async def fake_append(spreadsheet_id, values, sheet_range, workspace_id):
        observed.update(
            spreadsheet_id=spreadsheet_id,
            values=values,
            sheet_range=sheet_range,
            workspace_id=workspace_id,
        )
        return {"success": True, "range": "Leads!A2:B2"}

    monkeypatch.setattr(sheets, "append_row", fake_append)
    result = asyncio.run(execute_output_column(
        col_config={
            "type": "output",
            "destination": "sheets",
            "destination_config": {
                "spreadsheet_id": "sheet-1",
                "range": "Leads",
                "columns": ["company", "email"],
            },
        },
        lead_data={"company": "Acme", "email": "buyer@acme.test"},
        columns_config=[],
        workbook_id="wb-1",
        lead_id=1,
        workspace_id="ws-a",
    ))
    assert result["success"] is True
    assert observed == {
        "spreadsheet_id": "sheet-1",
        "values": ["Acme", "buyer@acme.test"],
        "sheet_range": "Leads",
        "workspace_id": "ws-a",
    }


def test_workbook_output_threads_workspace_into_airtable_adapter(monkeypatch):
    from apps.api.services.integrations import airtable

    observed = {}

    async def fake_push(fields, base_id, table, typecast=True, workspace_id=None):
        observed.update(
            fields=fields, base_id=base_id, table=table, workspace_id=workspace_id,
        )
        return {"success": True, "record_id": "rec-1"}

    monkeypatch.setattr(airtable, "push_record", fake_push)
    result = asyncio.run(execute_output_column(
        col_config={
            "type": "output",
            "destination": "airtable",
            "destination_config": {"base_id": "base-1", "table": "Leads"},
        },
        lead_data={"company": "Acme", "email": "buyer@acme.test"},
        columns_config=[],
        workbook_id="wb-1",
        lead_id=1,
        workspace_id="ws-a",
    ))
    assert result["success"] is True
    assert observed["workspace_id"] == "ws-a"
