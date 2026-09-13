import asyncio

import httpx

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


def test_sheets_upsert_appends_then_updates_by_stable_key(monkeypatch):
    monkeypatch.setattr(sheets, "_token", lambda workspace_id=None: "token")
    calls = []
    existing_rows = []

    class Client:
        async def __aenter__(self):
            return self

        async def __aexit__(self, *args):
            return None

        async def get(self, url, **kwargs):
            calls.append(("GET", url))
            return httpx.Response(200, json={"values": existing_rows})

        async def post(self, url, **kwargs):
            calls.append(("POST", url, kwargs["json"]))
            return httpx.Response(
                200, json={"updates": {"updatedRange": "Leads!A2:C2"}}
            )

        async def put(self, url, **kwargs):
            calls.append(("PUT", url, kwargs["json"]))
            return httpx.Response(200, json={"updatedRange": "Leads!A2:C2"})

    monkeypatch.setattr(sheets.httpx, "AsyncClient", lambda **kwargs: Client())
    appended = asyncio.run(sheets.upsert_row(
        "sheet-1", ["Acme", "buyer@acme.test"], "delivery-key",
        "Leads!A:ZZ", "ws-a",
    ))
    assert appended == {
        "success": True, "range": "Leads!A2:C2", "operation": "appended",
    }
    assert calls[-1][0] == "POST"
    assert calls[-1][2]["values"] == [["delivery-key", "Acme", "buyer@acme.test"]]

    existing_rows[:] = [["OpenGTM key", "Company", "Email"], ["delivery-key", "Old"]]
    updated = asyncio.run(sheets.upsert_row(
        "sheet-1", ["Acme", "buyer@acme.test"], "delivery-key",
        "Leads!A:ZZ", "ws-a",
    ))
    assert updated["operation"] == "updated"
    assert calls[-1][0] == "PUT"
    assert "Leads%21A2%3AC2" in calls[-1][1]


def test_airtable_upsert_uses_atomic_merge_field(monkeypatch):
    from apps.api.services.integrations import airtable

    monkeypatch.setattr(airtable, "_token", lambda workspace_id=None: "token")
    observed = {}

    class Client:
        async def __aenter__(self):
            return self

        async def __aexit__(self, *args):
            return None

        async def patch(self, url, **kwargs):
            observed.update(url=url, body=kwargs["json"])
            return httpx.Response(
                200,
                json={"records": [{"id": "rec-1"}], "createdRecords": ["rec-1"]},
            )

    monkeypatch.setattr(airtable.httpx, "AsyncClient", lambda **kwargs: Client())
    result = asyncio.run(airtable.upsert_record(
        {"Company": "Acme"}, "base/1", "Sales Leads", "stable-key",
        "OpenGTM ID", workspace_id="ws-a",
    ))
    assert result == {"success": True, "record_id": "rec-1", "operation": "created"}
    assert observed["url"].endswith("/base%2F1/Sales%20Leads")
    assert observed["body"]["performUpsert"] == {
        "fieldsToMergeOn": ["OpenGTM ID"],
    }
    assert observed["body"]["records"][0]["fields"]["OpenGTM ID"] == "stable-key"
