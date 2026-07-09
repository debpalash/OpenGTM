"""
Instantly + Smartlead output-column destinations.

HTTP is mocked via httpx.MockTransport (same pattern as test_http_column.py).
Covers: success, duplicate-lead (idempotent-friendly success), 401 bad key,
missing config (key / campaign id), and output.py dispatch routing.
"""
import asyncio
import json

import httpx

import apps.api.services.integrations.instantly as instantly
import apps.api.services.integrations.smartlead as smartlead
from apps.api.services.workbook.output import execute_output_column


def _patch_transport(monkeypatch, handler):
    """Make httpx.AsyncClient use a mock transport for the duration of a call."""
    real_init = httpx.AsyncClient.__init__

    def init(self, *a, **kw):
        kw["transport"] = httpx.MockTransport(handler)
        kw.pop("follow_redirects", None)
        real_init(self, *a, **kw)

    monkeypatch.setattr(httpx.AsyncClient, "__init__", init)


COLS = [
    {"id": "email", "name": "Email", "type": "lead_field"},
    {"id": "first_name", "name": "First Name", "type": "lead_field"},
    {"id": "last_name", "name": "Last Name", "type": "lead_field"},
    {"id": "company", "name": "Company", "type": "lead_field"},
    {"id": "icebreaker", "name": "Icebreaker", "type": "ai_formula"},
]
LEAD = {
    "email": "elon@spacex.com", "first_name": "Elon", "last_name": "Musk",
    "company": "SpaceX", "icebreaker": "Loved the Starship launch",
}


def _instantly_key(monkeypatch, key="ik-123"):
    monkeypatch.setattr(instantly, "_api_key", lambda workspace_id=None: key)


def _smartlead_key(monkeypatch, key="sk-123"):
    monkeypatch.setattr(smartlead, "_api_key", lambda workspace_id=None: key)


# ── Instantly adapter ────────────────────────────────────────────────────────

def test_instantly_success(monkeypatch):
    _instantly_key(monkeypatch)
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["url"] = str(request.url)
        seen["auth"] = request.headers.get("authorization")
        seen["body"] = json.loads(request.content)
        return httpx.Response(200, json={"id": "lead-1", "status": 1})

    _patch_transport(monkeypatch, handler)
    res = asyncio.run(instantly.add_lead_to_campaign("camp-uuid", {
        "email": "elon@spacex.com", "first_name": "Elon",
        "company_name": "SpaceX", "personalization": "hi",
        "icp_score": "97",  # not a top-level Instantly field → custom_variables
    }))
    assert res["success"] and not res["duplicate"] and res["lead_id"] == "lead-1"
    assert seen["url"] == "https://api.instantly.ai/api/v2/leads"
    assert seen["auth"] == "Bearer ik-123"
    b = seen["body"]
    assert b["campaign"] == "camp-uuid" and b["email"] == "elon@spacex.com"
    assert b["first_name"] == "Elon" and b["company_name"] == "SpaceX"
    assert b["personalization"] == "hi"
    assert b["custom_variables"] == {"icp_score": "97"}
    assert b["skip_if_in_campaign"] is True


def test_instantly_duplicate_is_success(monkeypatch):
    _instantly_key(monkeypatch)
    # a) skip_if_in_campaign hit → 200 with lead status -3 (Skipped)
    _patch_transport(monkeypatch, lambda r: httpx.Response(200, json={"id": "lead-1", "status": -3}))
    res = asyncio.run(instantly.add_lead_to_campaign("c", {"email": "a@b.co"}))
    assert res["success"] is True and res["duplicate"] is True
    # b) explicit duplicate rejection text → still success (idempotent-friendly)
    _patch_transport(monkeypatch, lambda r: httpx.Response(
        400, json={"message": "Lead already exists in this campaign"}))
    res = asyncio.run(instantly.add_lead_to_campaign("c", {"email": "a@b.co"}))
    assert res["success"] is True and res["duplicate"] is True


def test_instantly_bad_key_401(monkeypatch):
    _instantly_key(monkeypatch, "bad")
    _patch_transport(monkeypatch, lambda r: httpx.Response(401, json={"message": "Unauthorized"}))
    res = asyncio.run(instantly.add_lead_to_campaign("c", {"email": "a@b.co"}))
    assert res["success"] is False and "INSTANTLY_API_KEY" in res["error"]


def test_instantly_missing_config():
    import unittest.mock as mock
    with mock.patch.object(instantly, "_api_key", return_value=""):
        res = asyncio.run(instantly.add_lead_to_campaign("c", {"email": "a@b.co"}))
        assert res["success"] is False and "not connected" in res["error"]
    with mock.patch.object(instantly, "_api_key", return_value="k"):
        res = asyncio.run(instantly.add_lead_to_campaign("", {"email": "a@b.co"}))
        assert res["success"] is False and "campaign_id" in res["error"]
        res = asyncio.run(instantly.add_lead_to_campaign("c", {}))
        assert res["success"] is False and res["error"] == "no_email"


def test_instantly_network_error_never_raises(monkeypatch):
    _instantly_key(monkeypatch)

    def handler(request):
        raise httpx.ConnectError("boom")

    _patch_transport(monkeypatch, handler)
    res = asyncio.run(instantly.add_lead_to_campaign("c", {"email": "a@b.co"}))
    assert res["success"] is False and "boom" in res["error"]


# ── Smartlead adapter ────────────────────────────────────────────────────────

def test_smartlead_success(monkeypatch):
    _smartlead_key(monkeypatch)
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["url"] = str(request.url)
        seen["body"] = json.loads(request.content)
        return httpx.Response(200, json={"ok": True, "upload_count": 1, "total_leads": 1})

    _patch_transport(monkeypatch, handler)
    res = asyncio.run(smartlead.add_lead_to_campaign("42", {
        "email": "elon@spacex.com", "first_name": "Elon",
        "company_name": "SpaceX",
        "icebreaker": "hi",  # not a top-level Smartlead field → custom_fields
    }, settings={"ignore_duplicate_leads_in_other_campaign": True}))
    assert res["success"] and not res["duplicate"]
    assert seen["url"].startswith("https://server.smartlead.ai/api/v1/campaigns/42/leads")
    assert "api_key=sk-123" in seen["url"]
    entry = seen["body"]["lead_list"][0]
    assert entry["email"] == "elon@spacex.com" and entry["company_name"] == "SpaceX"
    assert entry["custom_fields"] == {"icebreaker": "hi"}
    assert seen["body"]["settings"] == {"ignore_duplicate_leads_in_other_campaign": True}


def test_smartlead_duplicate_is_success(monkeypatch):
    _smartlead_key(monkeypatch)
    # a) 200 body reporting the lead was already in the campaign
    _patch_transport(monkeypatch, lambda r: httpx.Response(200, json={
        "ok": True, "upload_count": 0, "total_leads": 1, "already_added_to_campaign": 1}))
    res = asyncio.run(smartlead.add_lead_to_campaign("42", {"email": "a@b.co"}))
    assert res["success"] is True and res["duplicate"] is True
    # b) rejection with duplicate wording → still success
    _patch_transport(monkeypatch, lambda r: httpx.Response(
        400, json={"message": "Lead already exists in campaign"}))
    res = asyncio.run(smartlead.add_lead_to_campaign("42", {"email": "a@b.co"}))
    assert res["success"] is True and res["duplicate"] is True


def test_smartlead_bad_key_401(monkeypatch):
    _smartlead_key(monkeypatch, "bad")
    _patch_transport(monkeypatch, lambda r: httpx.Response(401, text="Invalid API key"))
    res = asyncio.run(smartlead.add_lead_to_campaign("42", {"email": "a@b.co"}))
    assert res["success"] is False and "SMARTLEAD_API_KEY" in res["error"]


def test_smartlead_missing_config():
    import unittest.mock as mock
    with mock.patch.object(smartlead, "_api_key", return_value=""):
        res = asyncio.run(smartlead.add_lead_to_campaign("42", {"email": "a@b.co"}))
        assert res["success"] is False and "not connected" in res["error"]
    with mock.patch.object(smartlead, "_api_key", return_value="k"):
        res = asyncio.run(smartlead.add_lead_to_campaign("", {"email": "a@b.co"}))
        assert res["success"] is False and "campaign_id" in res["error"]
        res = asyncio.run(smartlead.add_lead_to_campaign("42", {}))
        assert res["success"] is False and res["error"] == "no_email"


# ── output.py dispatch (execute_output_column routing + field_map resolve) ──

def _run_output(dest, cfg, monkeypatch, handler):
    _patch_transport(monkeypatch, handler)
    col = {"type": "output", "destination": dest, "destination_config": cfg}
    return asyncio.run(execute_output_column(
        col_config=col, lead_data=LEAD, columns_config=COLS,
        workbook_id="wb1", lead_id=1, workspace_id=None,
    ))


def test_dispatch_instantly_routes_and_resolves_field_map(monkeypatch):
    _instantly_key(monkeypatch)
    seen = {}

    def handler(request):
        seen["host"] = request.url.host
        seen["body"] = json.loads(request.content)
        return httpx.Response(200, json={"id": "l1", "status": 1})

    res = _run_output("instantly", {
        "campaign_id": "camp-uuid",
        "field_map": {
            "email": "email",
            "first_name": "first_name",
            "company": "company_name",
            "icebreaker": "personalization",   # ai column → vendor field
            "{first_name} {last_name}": "full_name",  # template key → custom var
            "nonexistent_col": "ghost",        # unresolvable → dropped
        },
    }, monkeypatch, handler)
    assert res["success"] is True and res["value"] == "Instantly: added"
    assert seen["host"] == "api.instantly.ai"
    b = seen["body"]
    assert b["campaign"] == "camp-uuid"
    assert b["email"] == "elon@spacex.com" and b["company_name"] == "SpaceX"
    assert b["personalization"] == "Loved the Starship launch"
    assert b["custom_variables"] == {"full_name": "Elon Musk"}
    assert "ghost" not in b and "ghost" not in b.get("custom_variables", {})


def test_dispatch_smartlead_routes_with_default_field_map(monkeypatch):
    _smartlead_key(monkeypatch)
    seen = {}

    def handler(request):
        seen["host"] = request.url.host
        seen["path"] = request.url.path
        seen["body"] = json.loads(request.content)
        return httpx.Response(200, json={"ok": True, "upload_count": 1})

    res = _run_output("smartlead", {"campaign_id": "77"}, monkeypatch, handler)
    assert res["success"] is True and res["value"] == "Smartlead: added"
    assert seen["host"] == "server.smartlead.ai"
    assert seen["path"] == "/api/v1/campaigns/77/leads"
    entry = seen["body"]["lead_list"][0]
    assert entry == {"email": "elon@spacex.com", "first_name": "Elon",
                     "last_name": "Musk", "company_name": "SpaceX"}


def test_dispatch_instantly_duplicate_note_in_cell_value(monkeypatch):
    _instantly_key(monkeypatch)
    res = _run_output("instantly", {"campaign_id": "c"}, monkeypatch,
                      lambda r: httpx.Response(200, json={"id": "l1", "status": -3}))
    assert res["success"] is True and res["value"] == "Instantly: already in campaign"


def test_dispatch_smartlead_duplicate_note_in_cell_value(monkeypatch):
    _smartlead_key(monkeypatch)
    res = _run_output("smartlead", {"campaign_id": "9"}, monkeypatch,
                      lambda r: httpx.Response(200, json={"ok": True, "already_added_to_campaign": 1}))
    assert res["success"] is True and res["value"] == "Smartlead: already in campaign"


def test_dispatch_missing_campaign_id_clean_error(monkeypatch):
    _instantly_key(monkeypatch)
    _smartlead_key(monkeypatch)

    def handler(request):  # must never be reached
        raise AssertionError("no HTTP call expected without campaign_id")

    res = _run_output("instantly", {}, monkeypatch, handler)
    assert res["success"] is False and "campaign_id" in res["error"]
    res = _run_output("smartlead", {}, monkeypatch, handler)
    assert res["success"] is False and "campaign_id" in res["error"]


def test_dispatch_unknown_destination_still_clean():
    res = asyncio.run(execute_output_column(
        col_config={"type": "output", "destination": "mailmerge9000",
                    "destination_config": {}},
        lead_data=LEAD, columns_config=COLS, workbook_id="wb1", lead_id=1,
    ))
    assert res["success"] is False and "unknown destination" in res["error"]
