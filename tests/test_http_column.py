"""
HTTP action column (Phase 4, PR A): templating + SSRF guard + JSONPath extract.
Network is mocked via httpx.MockTransport.
"""
import asyncio
import socket

import httpx
import pytest

import apps.api.services.workbook.http_column as hc
from apps.api.services.workbook.http_column import execute_http_column


def _patch_transport(monkeypatch, handler):
    """Make httpx.AsyncClient use a mock transport for the duration of a call.

    Also stub socket.getaddrinfo so the SSRF guard's resolve=True check sees a
    public IP for the test host (the guard now resolves hostnames; without this
    the offline test box can't resolve api.example.com and the guard would
    rightly block the request before the mock transport is reached)."""
    real_init = httpx.AsyncClient.__init__

    def init(self, *a, **kw):
        kw["transport"] = httpx.MockTransport(handler)
        kw.pop("follow_redirects", None)
        real_init(self, *a, **kw)

    monkeypatch.setattr(httpx.AsyncClient, "__init__", init)
    monkeypatch.setattr(
        socket, "getaddrinfo",
        lambda *a, **k: [(socket.AF_INET, None, None, "", ("93.184.216.34", 0))],
    )


COLS = [
    {"id": "company", "name": "Company", "type": "lead_field"},
    {"id": "website", "name": "Website", "type": "lead_field"},
]
LEAD = {"company": "SpaceX", "website": "spacex.com"}


def test_templated_url_and_jsonpath_extract(monkeypatch):
    seen = {}
    def handler(request: httpx.Request) -> httpx.Response:
        seen["url"] = str(request.url)
        return httpx.Response(200, json={"data": {"email": "elon@spacex.com"}})
    _patch_transport(monkeypatch, handler)

    col = {"type": "http", "http_url": "https://api.example.com/find?domain={website}",
           "http_extract": "$.data.email"}
    res = asyncio.run(execute_http_column(col, LEAD, COLS))
    assert res["success"] and res["value"] == "elon@spacex.com"
    assert "domain=spacex.com" in seen["url"]  # {website} resolved


def test_post_body_templating(monkeypatch):
    body_seen = {}
    def handler(request):
        import json
        body_seen.update(json.loads(request.content))
        return httpx.Response(200, json={"ok": True, "phone": "+1-555"})
    _patch_transport(monkeypatch, handler)

    col = {"type": "http", "http_url": "https://api.example.com/x", "http_method": "POST",
           "http_body": {"company": "{company}"}, "http_extract": "$.phone"}
    res = asyncio.run(execute_http_column(col, LEAD, COLS))
    assert res["success"] and res["value"] == "+1-555"
    assert body_seen == {"company": "SpaceX"}


def test_ssrf_blocks_private_target(monkeypatch):
    # url resolves to a metadata IP via a row value → blocked before any request
    col = {"type": "http", "http_url": "http://{website}/x", "http_extract": "$.x"}
    res = asyncio.run(execute_http_column(col, {"website": "169.254.169.254"}, COLS))
    assert res["success"] is False and res["error"].startswith("blocked_url")


def test_missing_url():
    res = asyncio.run(execute_http_column({"type": "http"}, LEAD, COLS))
    assert res["success"] is False and res["error"] == "no_url"


def test_http_error_status(monkeypatch):
    _patch_transport(monkeypatch, lambda r: httpx.Response(404))
    col = {"type": "http", "http_url": "https://api.example.com/x", "http_extract": "$.x"}
    res = asyncio.run(execute_http_column(col, LEAD, COLS))
    assert res["success"] is False and res["error"] == "http_404"


def test_no_match_returns_error(monkeypatch):
    _patch_transport(monkeypatch, lambda r: httpx.Response(200, json={"data": {}}))
    col = {"type": "http", "http_url": "https://api.example.com/x", "http_extract": "$.data.email"}
    res = asyncio.run(execute_http_column(col, LEAD, COLS))
    assert res["success"] is False and res["error"] == "no_match"


def test_raw_text_when_no_extract(monkeypatch):
    _patch_transport(monkeypatch, lambda r: httpx.Response(200, text="hello world"))
    col = {"type": "http", "http_url": "https://api.example.com/x"}
    res = asyncio.run(execute_http_column(col, LEAD, COLS))
    assert res["success"] and res["value"] == "hello world"


def test_redirect_is_not_followed(monkeypatch):
    calls = []

    def handler(request):
        calls.append(str(request.url))
        return httpx.Response(302, headers={"location": "http://127.0.0.1/admin"})

    _patch_transport(monkeypatch, handler)
    col = {"type": "http", "http_url": "https://api.example.com/x"}
    res = asyncio.run(execute_http_column(col, LEAD, COLS))
    assert res["success"] is False and res["error"] == "http_302"
    assert calls == ["https://api.example.com/x"]
